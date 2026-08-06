package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/shenguard"
	"github.com/pyrex41/shenmux/internal/zmqx"
)

const (
	controlPollInterval = 2 * time.Millisecond
	leasePollInterval   = 250 * time.Millisecond

	// maxConsecutiveRecvReject bounds how many unreadable control messages in a
	// row the loop tolerates before it stops trusting the receive path itself.
	// Each reject consumes exactly one message, so no client can live-lock the
	// loop; this only guards against a future receive failure that repeats
	// without consuming anything.
	maxConsecutiveRecvReject = 64
)

// controlSocket is the ROUTER surface the control loop uses. *zmqx.Socket
// satisfies it. Tests substitute a stub, because a reply that cannot be routed
// to a departed peer is otherwise only reachable by winning a race against
// that peer's disconnect.
type controlSocket interface {
	SendMultipart(frames [][]byte, flags int) error
	RecvMultipartLimit(flags, maxFrame, maxFrames int) ([][]byte, error)
	Close() error
}

// readyWaiter is the publisher surface the control loop uses: it blocks until a
// client's data-plane subscription is live, so an attach snapshot is never cut
// before the client can receive the deltas that follow it.
type readyWaiter interface {
	WaitReady(ctx context.Context, cid shenguard.ClientID) error
}

// clientFault is a control-plane failure confined to one client: the request
// was handled, but the reply could not be routed because that peer is gone.
// It is deliberately not fatal. Anything reaching ControlServer.Errors ends the
// session -- shell, scrollback and all -- so only failures that genuinely
// invalidate the server may travel that way.
type clientFault struct {
	client shenguard.ClientID // zero when the peer presented no usable identity
	err    error
}

func (f *clientFault) Error() string {
	if f.client.IsZero() {
		return fmt.Sprintf("control client: %v", f.err)
	}
	return fmt.Sprintf("control client %s: %v", f.client, f.err)
}

func (f *clientFault) Unwrap() error { return f.err }

// encodeFault marks a reply this server built but could not encode. It is a
// defect in one answer, not in the socket, so it must not travel to
// ControlServer.Errors and end the session. It is distinct from clientFault
// only because sendControl has no client id to attribute it to; the caller,
// which does, converts it.
type encodeFault struct{ err error }

func (e *encodeFault) Error() string { return "encode control reply: " + e.err.Error() }
func (e *encodeFault) Unwrap() error { return e.err }

// peerGone reports whether a control-plane send failed because the addressed
// peer is no longer connected, as opposed to because the socket is unusable.
//
// This is a sentinel comparison, not a message match: zmq4 declares ErrNoRoute
// and ErrNoIdentity as package-level errors, and ROUTER.Send wraps ErrNoRoute
// with %w when no pipe carries the requested identity. That wrapped value is
// exactly the "zmq4: no route to peer: identity web-..." failure that used to
// take a whole session down. Every other ROUTER.Send failure is zmq4.ErrClosed,
// because the pure-Go ROUTER queues with a blocking overflow policy and only
// refuses a message once the socket itself is closing; zmqx maps that to
// zmqx.ErrClosed, which stays fatal here.
func peerGone(err error) bool {
	return errors.Is(err, zmqx.ErrNoRoute) || errors.Is(err, zmqx.ErrNoIdentity)
}

// dropClient runs, for a client that vanished mid-reply, the same teardown a
// clean detach runs. Runtime.Detach removes the client from the session and,
// through the Shen rule mux.detach -> mux.release-if-owner, releases the
// exclusive control lease when that client held it. Without that release a
// departed browser tab leaves the lease parked on a client id nobody can reach
// and no later client can type -- the same wedge internal/agent/bridge.go
// fixes one layer up.
func dropClient(runtime *Runtime, fault *clientFault) {
	if fault.client.IsZero() {
		log.Printf("shenmux control: discarded a reply to an unidentified client: %v", fault.err)
		return
	}
	if err := runtime.Detach(fault.client); err != nil && !errors.Is(err, shenguard.ErrNotAttached) {
		log.Printf("shenmux control: client %s went away, detach failed: %v", fault.client, err)
		return
	}
	log.Printf("shenmux control: dropped client %s after a failed reply: %v", fault.client, fault.err)
}

type ControlServer struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    chan error
}

func NewControlServer(
	parent context.Context,
	zctx *zmqx.Context,
	endpoint, session string,
	runtime *Runtime,
	publisher *ZMQPublisher,
) (*ControlServer, error) {
	if zctx == nil || runtime == nil || publisher == nil {
		return nil, errors.New("control server dependencies must not be nil")
	}
	if endpoint == "" || session == "" {
		return nil, errors.New("control endpoint and session must not be empty")
	}
	socket, err := zctx.Socket(zmqx.Router)
	if err != nil {
		return nil, err
	}
	cleanup := func(err error) (*ControlServer, error) { _ = socket.Close(); return nil, err }
	for option, value := range map[int]int{
		zmqx.Linger: 0, zmqx.SndHWM: 10_000, zmqx.RcvHWM: 10_000,
	} {
		if err := socket.SetInt(option, value); err != nil {
			return cleanup(err)
		}
	}
	if err := socket.SetInt64(zmqx.MaxMsgSize, protocol.MaxPayloadSize); err != nil {
		return cleanup(err)
	}
	if err := socket.Bind(endpoint); err != nil {
		return cleanup(err)
	}
	return newControlServer(parent, socket, session, runtime, publisher), nil
}

// newControlServer starts the control loop over an already-bound socket.
func newControlServer(
	parent context.Context,
	socket controlSocket,
	session string,
	runtime *Runtime,
	publisher readyWaiter,
) *ControlServer {
	ctx, cancel := context.WithCancel(parent)
	server := &ControlServer{cancel: cancel, done: make(chan struct{}), err: make(chan error, 1)}
	go server.run(ctx, socket, session, runtime, publisher)
	return server
}

// Errors carries only failures that invalidate the server itself, such as the
// bound socket closing. Callers end the session on anything received here, so a
// failure attributable to a single client must never be sent down this channel.
func (s *ControlServer) Errors() <-chan error { return s.err }

func (s *ControlServer) Close() error {
	s.cancel()
	<-s.done
	select {
	case err := <-s.err:
		return err
	default:
		return nil
	}
}

func (s *ControlServer) run(
	ctx context.Context,
	socket controlSocket,
	session string,
	runtime *Runtime,
	publisher readyWaiter,
) {
	defer close(s.done)
	defer close(s.err)
	defer socket.Close()
	poll := time.NewTicker(controlPollInterval)
	defer poll.Stop()
	lease := time.NewTicker(leasePollInterval)
	defer lease.Stop()

	fail := func(err error) {
		select {
		case s.err <- err:
		default:
		}
	}
	rejects := 0
	for {
		for {
			frames, err := socket.RecvMultipartLimit(zmqx.DontWait, protocol.MaxPayloadSize, 4)
			if errors.Is(err, zmqx.ErrWouldBlock) {
				break
			}
			if err != nil {
				// Only a dead socket invalidates this server. Every other error
				// the receive path can produce here describes a message that was
				// received and then refused -- too many frames, or a frame over
				// the payload limit -- which is one client's fault and must not
				// end the session for everybody.
				if errors.Is(err, zmqx.ErrClosed) {
					fail(err)
					return
				}
				rejects++
				if rejects > maxConsecutiveRecvReject {
					fail(fmt.Errorf("control receive failed %d times consecutively: %w", rejects, err))
					return
				}
				log.Printf("shenmux control: dropped an unreadable control message: %v", err)
				continue
			}
			rejects = 0
			if err := handleControl(ctx, socket, frames, session, runtime, publisher); err != nil {
				// Per-request failures are encoded for that client, so only an
				// inability to send a response reaches this branch. A reply that
				// cannot be routed means that one peer has gone: tear its client
				// down and keep serving everyone else.
				var fault *clientFault
				if errors.As(err, &fault) {
					dropClient(runtime, fault)
					continue
				}
				fail(err)
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-lease.C:
			if err := runtime.ReapExpired(time.Now()); err != nil {
				fail(fmt.Errorf("reap control lease: %w", err))
				return
			}
		case <-poll.C:
		}
	}
}

func handleControl(
	ctx context.Context,
	socket controlSocket,
	frames [][]byte,
	session string,
	runtime *Runtime,
	publisher readyWaiter,
) error {
	if len(frames) < 3 || len(frames) > 4 {
		// ROUTER prepends exactly one identity frame to the 2-3 protocol
		// frames. Ignore malformed messages that cannot be replied to safely.
		return nil
	}
	identity := append([]byte(nil), frames[0]...)
	// cid is empty until the identity frame parses; reply closes over it so that
	// every answer to this peer is attributed to the right client.
	var cid shenguard.ClientID
	// All replies funnel through reply, so a peer that vanished between sending
	// its request and receiving its answer surfaces as a clientFault -- one dead
	// client -- rather than as a server failure that ends the session.
	reply := func(msg protocol.Message) error {
		err := sendControl(socket, identity, msg)
		if err == nil {
			return nil
		}
		// A reply that cannot be routed and a reply that cannot be encoded are
		// both about the answer owed to one client; neither says the socket is
		// unusable, so neither may end the session. Anything else on this path
		// is a dead socket and stays fatal.
		var encode *encodeFault
		if peerGone(err) || errors.As(err, &encode) {
			return &clientFault{client: cid, err: err}
		}
		return err
	}
	replyError := func(requestID uint64, cause error) error {
		return reply(protocol.Message{
			Kind: protocol.KindError,
			Meta: protocol.Meta{Version: protocol.Version, Session: session, RequestID: requestID, Error: cause.Error()},
		})
	}

	parsed, err := shenguard.NewClientID(string(identity))
	if err != nil {
		return replyError(0, fmt.Errorf("invalid ROUTER identity: %w", err))
	}
	cid = parsed
	msg, err := protocol.Decode(frames[1:])
	if err != nil {
		return replyError(0, err)
	}
	requestID := msg.Meta.RequestID
	if msg.Meta.Session != "" && msg.Meta.Session != session {
		return replyError(requestID, fmt.Errorf("unknown session %q", msg.Meta.Session))
	}
	if msg.Meta.ClientID != "" && msg.Meta.ClientID != cid.String() {
		return replyError(requestID, errors.New("client_id does not match ROUTER identity"))
	}

	// Any valid control-plane traffic renews the owner's lease. Touch is a
	// no-op before attachment and for observers not present in the model.
	runtime.Touch(cid, time.Now())

	var response *protocol.Message
	switch msg.Kind {
	case protocol.KindAttach, protocol.KindResync:
		readyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = publisher.WaitReady(readyCtx, cid)
		cancel()
		if err == nil {
			var attached protocol.Message
			if msg.Kind == protocol.KindResync {
				attached, err = runtime.ResyncSnapshot(cid)
			} else {
				attached, err = runtime.AttachSnapshot(cid)
			}
			if err == nil {
				attached.Meta.RequestID = requestID
				response = &attached
			}
		}
	case protocol.KindAcquireControl:
		err = runtime.AcquireControl(cid)
		if err == nil {
			response = controlAck(session, cid, requestID, runtime)
		}
	case protocol.KindReleaseControl:
		err = runtime.ReleaseControl(cid)
		if err == nil {
			response = controlAck(session, cid, requestID, runtime)
		}
	case protocol.KindInput:
		err = runtime.Input(cid, msg.Payload)
		if err == nil {
			response = controlAck(session, cid, requestID, runtime)
		}
	case protocol.KindResize:
		var dim shenguard.Dimensions
		dim, err = shenguard.NewDimensions(int(msg.Meta.Cols), int(msg.Meta.Rows))
		if err == nil {
			err = runtime.Resize(cid, dim)
		}
		if err == nil {
			response = controlAck(session, cid, requestID, runtime)
		}
	case protocol.KindDetach:
		err = runtime.Detach(cid)
		if errors.Is(err, shenguard.ErrNotAttached) {
			err = nil
		}
		if err == nil {
			response = controlAck(session, cid, requestID, runtime)
		}
	case protocol.KindPing:
		response = controlAck(session, cid, requestID, runtime)
	default:
		err = fmt.Errorf("message kind %q is not valid on the control plane", msg.Kind)
	}
	if err != nil {
		return replyError(requestID, err)
	}
	if response == nil {
		return nil
	}
	return reply(*response)
}

func controlAck(session string, cid shenguard.ClientID, requestID uint64, runtime *Runtime) *protocol.Message {
	status := runtime.Status()
	return &protocol.Message{
		Kind: protocol.KindPong,
		Meta: protocol.Meta{
			Version: protocol.Version, Session: session, ClientID: cid.String(),
			RequestID: requestID, Seq: status.Seq, CheckpointSeq: status.Store.CheckpointSeq,
			Cols: status.Dim.Cols(), Rows: status.Dim.Rows(), ExitCode: status.ExitCode,
			ControlOwner: status.ControlOwner, HasControl: status.ControlOwner == cid.String(),
		},
	}
}

func sendControl(socket controlSocket, identity []byte, msg protocol.Message) error {
	frames, err := protocol.Encode(msg)
	if err != nil {
		return &encodeFault{err: err}
	}
	return socket.SendMultipart(append([][]byte{identity}, frames...), 0)
}
