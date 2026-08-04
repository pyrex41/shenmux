package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/shenguard"
	"github.com/pyrex41/shenmux/internal/zmqx"
)

const (
	controlPollInterval = 2 * time.Millisecond
	leasePollInterval   = 250 * time.Millisecond
)

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
	ctx, cancel := context.WithCancel(parent)
	server := &ControlServer{cancel: cancel, done: make(chan struct{}), err: make(chan error, 1)}
	go server.run(ctx, socket, session, runtime, publisher)
	return server, nil
}

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
	socket *zmqx.Socket,
	session string,
	runtime *Runtime,
	publisher *ZMQPublisher,
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
	for {
		for {
			frames, err := socket.RecvMultipartLimit(zmqx.DontWait, protocol.MaxPayloadSize, 4)
			if errors.Is(err, zmqx.ErrWouldBlock) {
				break
			}
			if err != nil {
				fail(err)
				return
			}
			if err := handleControl(ctx, socket, frames, session, runtime, publisher); err != nil {
				// Per-request failures are encoded for that client. Only inability
				// to send a response reaches this branch.
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
	socket *zmqx.Socket,
	frames [][]byte,
	session string,
	runtime *Runtime,
	publisher *ZMQPublisher,
) error {
	if len(frames) < 3 || len(frames) > 4 {
		// ROUTER prepends exactly one identity frame to the 2-3 protocol
		// frames. Ignore malformed messages that cannot be replied to safely.
		return nil
	}
	identity := append([]byte(nil), frames[0]...)
	cid, err := shenguard.NewClientID(string(identity))
	if err != nil {
		return sendControlError(socket, identity, 0, session, fmt.Errorf("invalid ROUTER identity: %w", err))
	}
	msg, err := protocol.Decode(frames[1:])
	if err != nil {
		return sendControlError(socket, identity, 0, session, err)
	}
	requestID := msg.Meta.RequestID
	if msg.Meta.Session != "" && msg.Meta.Session != session {
		return sendControlError(socket, identity, requestID, session, fmt.Errorf("unknown session %q", msg.Meta.Session))
	}
	if msg.Meta.ClientID != "" && msg.Meta.ClientID != cid.String() {
		return sendControlError(socket, identity, requestID, session, errors.New("client_id does not match ROUTER identity"))
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
		return sendControlError(socket, identity, requestID, session, err)
	}
	if response == nil {
		return nil
	}
	return sendControl(socket, identity, *response)
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

func sendControlError(socket *zmqx.Socket, identity []byte, requestID uint64, session string, err error) error {
	return sendControl(socket, identity, protocol.Message{
		Kind: protocol.KindError,
		Meta: protocol.Meta{Version: protocol.Version, Session: session, RequestID: requestID, Error: err.Error()},
	})
}

func sendControl(socket *zmqx.Socket, identity []byte, msg protocol.Message) error {
	frames, err := protocol.Encode(msg)
	if err != nil {
		return err
	}
	return socket.SendMultipart(append([][]byte{identity}, frames...), 0)
}
