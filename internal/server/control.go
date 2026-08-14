package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/pyrex41/shenmux/protocol"
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

	// maxControlFrames is what a well-formed control message carries: the one
	// ROUTER identity frame plus protocol.Encode's two or three frames.
	maxControlFrames = 4

	// maxControlRecvFrames is deliberately looser than maxControlFrames. The
	// receive path refuses an over-long message *after* consuming it, returning
	// an error and no frames at all, so a message refused there cannot be
	// answered -- there is no identity left to answer. Receiving with slack and
	// enforcing the real limit in handleControl, where the identity has been
	// read, turns the commonest malformed message into a KindError the sender
	// can act on. It costs no memory: the driver has already read the whole
	// message by the time zmqx counts its frames, so this cap decides who we can
	// answer, not how much we hold.
	maxControlRecvFrames = 8

	// controlSendDeadline bounds how long one reply may hold the control loop.
	//
	// It sits between two facts. A client that is merely busy -- a backgrounded
	// browser tab, a laptop resuming from sleep -- drains a local IPC pipe in
	// milliseconds and cannot fill a 10_000-message queue at all unless it has
	// stopped reading altogether, so blocking here already means "not taking
	// delivery" rather than "slow". On the other side this loop is the only
	// thing that reaps expired control leases and serves every other client, so
	// the wait is a hard cap on how long everybody else is stalled. Two seconds
	// is far past any scheduling hiccup, is the same order as the WaitReady
	// timeout a client already tolerates on attach, and stays well inside
	// DefaultControlLease (8s) so a stalled peer cannot delay reaping past the
	// lease it is holding. It is paid at most once per peer, because
	// boundedSender refuses to park a second send behind a stalled one.
	controlSendDeadline = 2 * time.Second

	// maxParkedSends caps how many replies may sit in the driver at once after
	// the loop has given up on them. It is the backstop for the per-peer rule
	// below, which cannot cover a peer that keeps presenting fresh identities.
	maxParkedSends = 64

	// maxStalledPeers caps the per-peer memory of stalled sends. The memo is an
	// optimisation -- maxParkedSends is what actually bounds the goroutines --
	// so refusing to grow it past this point is safe.
	maxStalledPeers = 1024
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

// errPeerNotDraining marks a reply the addressed peer never took delivery of.
// It is the same condition as a missing route -- a client that is not receiving
// -- reached from the other side: the peer still has a pipe, it has simply
// stopped emptying it. The classification treats the two alike so there is one
// concept here ("the client is gone, clean up after it") rather than two.
var errPeerNotDraining = errors.New("control client is not taking delivery")

// deliveryOutcome names, in the Shen model's vocabulary, why a reply addressed
// to one identified client did not arrive. It only recognises; it does not
// decide. Whether an outcome ends the session is mux.delivery-fatal?'s
// judgement, and an outcome this function fails to recognise becomes
// DeliveryUnknown rather than falling through to fatal -- which is how a
// missing arm here cost a whole session twice.
//
// For the routing failures this is a sentinel comparison, not a message match:
// zmq4 declares ErrNoRoute and ErrNoIdentity as package-level errors, and
// ROUTER.Send wraps ErrNoRoute with %w when no pipe carries the requested
// identity. That wrapped value is exactly the "zmq4: no route to peer: identity
// web-..." failure that used to take a whole session down. zmq4.ErrClosed is
// the one outcome that is about the socket rather than the peer; zmqx maps it
// to zmqx.ErrClosed.
//
// errPeerNotDraining is our own: a peer whose queue is full never produces a
// zmq4 error at all, it just parks the sender forever (see boundedSender).
func deliveryOutcome(err error) string {
	var encode *encodeFault
	switch {
	case errors.Is(err, zmqx.ErrNoRoute):
		return shenguard.DeliveryNoRoute
	case errors.Is(err, zmqx.ErrNoIdentity):
		return shenguard.DeliveryNoIdentity
	case errors.Is(err, errPeerNotDraining):
		return shenguard.DeliveryNotDraining
	case errors.As(err, &encode):
		return shenguard.DeliveryEncodeFault
	case errors.Is(err, zmqx.ErrClosed):
		return shenguard.DeliverySocketClosed
	default:
		return shenguard.DeliveryUnknown
	}
}

// perPeerFault reports whether a failed reply is one client's problem rather
// than the session's. The rule lives in specs/mux.shen so that a new transport
// outcome cannot silently change the answer, and so that the safe reading of an
// unrecognised one is written down once instead of implied by a switch default.
func perPeerFault(err error) bool {
	return !shenguard.DeliveryFatal(deliveryOutcome(err))
}

// boundedSender is the control loop's only way onto the socket. It exists
// because the ROUTER underneath cannot be told to give up. zmq4's ROUTER.Send
// takes a context.Context and never reads it; under the driver's default
// blocking overflow policy a full per-peer queue parks the caller on the
// socket's own close channel. One client that stops reading would therefore
// stop receives, lease reaping and every other client, and ControlServer.Close
// would block forever on <-s.done. The two obvious repairs do not work: the
// Drop overflow policy reports a dropped message as zmq4.ErrClosed, which we
// must classify as fatal, and zmqx.SendMultipartContext hands its context to
// the same call that discards it.
//
// So the bound lives here. Each send runs on its own goroutine; the loop waits
// controlSendDeadline for it and otherwise walks away with errPeerNotDraining,
// which perPeerFault reports as one client's problem so the dropClient path
// detaches the client and frees its control lease.
//
// The goroutine outlives the deadline. That is the honest cost of a driver with
// no cancellable send: it stays parked until the peer drains or the socket
// closes, and its result is discarded because the client it belonged to has
// already been dropped. Two rules keep those from accumulating:
//
//   - a peer whose previous send is still parked is never given a second one,
//     so a stalled client costs one goroutine and one deadline, not one per
//     request it keeps sending; the memo clears itself as soon as that send
//     finally returns, so a peer that recovers is served again;
//   - at most maxParkedSends may be parked at once whatever identities they
//     were addressed to, which is the backstop for a peer that reconnects
//     under fresh identities faster than the memo can name them.
//
// Only the control loop goroutine calls send, so stalled needs no lock; parked
// is atomic because the send goroutines decrement it.
type boundedSender struct {
	socket  controlSocket
	parked  atomic.Int64
	stalled map[string]chan error
}

func newBoundedSender(socket controlSocket) *boundedSender {
	return &boundedSender{socket: socket, stalled: make(map[string]chan error)}
}

// send delivers frames to identity, or gives up after controlSendDeadline.
func (b *boundedSender) send(identity []byte, frames [][]byte) error {
	if b.isStalled(string(identity)) {
		return fmt.Errorf("%w: an earlier reply is still queued for it", errPeerNotDraining)
	}
	if b.parked.Load() >= maxParkedSends {
		return fmt.Errorf("%w: %d replies are already parked in the socket", errPeerNotDraining, maxParkedSends)
	}
	// Buffered, because this goroutine must never block on a reader that has
	// already given up on it.
	done := make(chan error, 1)
	b.parked.Add(1)
	go func() {
		err := b.socket.SendMultipart(frames, 0)
		b.parked.Add(-1)
		done <- err
	}()
	timer := time.NewTimer(controlSendDeadline)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		b.remember(string(identity), done)
		return fmt.Errorf("%w after %s", errPeerNotDraining, controlSendDeadline)
	}
}

// isStalled reports whether an earlier send to identity is still parked, and
// forgets the peer once that send has returned.
func (b *boundedSender) isStalled(identity string) bool {
	done, known := b.stalled[identity]
	if !known {
		return false
	}
	select {
	case <-done:
		delete(b.stalled, identity)
		return false
	default:
		return true
	}
}

func (b *boundedSender) remember(identity string, done chan error) {
	if len(b.stalled) >= maxStalledPeers {
		b.forgetFinished()
	}
	if len(b.stalled) >= maxStalledPeers {
		return
	}
	b.stalled[identity] = done
}

// forgetFinished drops the peers whose parked send has since returned. It runs
// only when the memo is at its cap, which needs a client population large
// enough that the parked-send cap is doing the real work anyway.
func (b *boundedSender) forgetFinished() {
	for identity, done := range b.stalled {
		select {
		case <-done:
			delete(b.stalled, identity)
		default:
		}
	}
}

// dropClient runs, for a client that vanished mid-reply, the same teardown a
// clean detach runs. Runtime.PeerLost removes the client from the session and,
// through the Shen rule mux.detach -> mux.release-if-owner, releases the
// exclusive control lease when that client held it. Without that release a
// departed browser tab leaves the lease parked on a client id nobody can reach
// and no later client can type -- the same wedge internal/agent/bridge.go
// fixes one layer up.
//
// The "was it even attached?" case is the model's (mux.reduce-peer-lost is
// idempotent), not a condition repeated at each failure path.
func dropClient(runtime *Runtime, fault *clientFault) {
	if fault.client.IsZero() {
		log.Printf("shenmux control: discarded a reply to an unidentified client: %v", fault.err)
		return
	}
	if err := runtime.PeerLost(fault.client); err != nil {
		log.Printf("shenmux control: client %s went away, teardown failed: %v", fault.client, err)
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
//
// One client-visible outcome is deliberately reported nowhere, here or to the
// client: a message the receive path refuses outright -- more than
// maxControlRecvFrames frames, or a frame larger than protocol.MaxPayloadSize
// -- is consumed before zmqx will hand it back, so no ROUTER identity survives
// to answer it. Such a message is logged and dropped, and its sender gets no
// reply and must fall back on its own request timeout. Everything the receive
// path does hand back is answered, including a malformed frame count, which
// handleControl turns into a KindError.
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
	sender := newBoundedSender(socket)
	rejects := 0
	for {
		for {
			frames, err := socket.RecvMultipartLimit(zmqx.DontWait, protocol.MaxPayloadSize, maxControlRecvFrames)
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
			if err := handleControl(ctx, sender, frames, session, runtime, publisher); err != nil {
				// Per-request failures are encoded for that client, so only an
				// inability to send a response reaches this branch. A reply that
				// cannot be routed, or that the peer never takes delivery of,
				// means that one peer has gone: tear its client down and keep
				// serving everyone else.
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
	sender *boundedSender,
	frames [][]byte,
	session string,
	runtime *Runtime,
	publisher readyWaiter,
) error {
	if len(frames) == 0 {
		// Unreachable from a ROUTER, which always prepends an identity frame,
		// and the one case where there is genuinely nobody to answer.
		log.Print("shenmux control: discarded a control message with no frames")
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
		err := sendControl(sender, identity, msg)
		if err == nil {
			return nil
		}
		// This send was addressed to one identified client, so what it learned is
		// about that client. Only an outcome the model calls fatal -- the socket
		// itself -- may end the session.
		if perPeerFault(err) {
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
	if len(frames) < 3 || len(frames) > maxControlFrames {
		// The frame count is checked here rather than at the socket because
		// frames[0] is the sender's identity: a message we can count is a
		// message we can answer, and a client left waiting on a request it
		// malformed learns nothing from the silence.
		return replyError(0, fmt.Errorf(
			"control message carries %d frames; want a ROUTER identity plus 2 or 3 protocol frames", len(frames)))
	}
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

func sendControl(sender *boundedSender, identity []byte, msg protocol.Message) error {
	frames, err := protocol.Encode(msg)
	if err != nil {
		return &encodeFault{err: err}
	}
	return sender.send(identity, append([][]byte{identity}, frames...))
}
