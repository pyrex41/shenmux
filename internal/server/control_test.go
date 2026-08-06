//go:build linux || darwin

package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/shenguard"
	"github.com/pyrex41/shenmux/internal/term"
	"github.com/pyrex41/shenmux/internal/zmqx"
	zmq "github.com/tomi77/zmq4"
)

func TestControlAttachAcquireAndInput(t *testing.T) {
	zctx, err := zmqx.NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer zctx.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stamp := time.Now().UnixNano()
	dataEndpoint := fmt.Sprintf("inproc://control-data-%d", stamp)
	controlEndpoint := fmt.Sprintf("inproc://control-router-%d", stamp)
	publisher, err := NewZMQPublisher(ctx, zctx, dataEndpoint, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	dim, _ := shenguard.NewDimensions(80, 24)
	pty := &fakePTY{dim: dim}
	runtime, err := NewRuntime(RuntimeConfig{
		Session: "test", Dimensions: dim, PTY: pty, Terminal: term.NewBasic(dim),
		Publisher: publisher, StoreLimits: protocol.DefaultStoreLimits(), ControlLease: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	control, err := NewControlServer(ctx, zctx, controlEndpoint, "test", runtime, publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	sub, err := zctx.Socket(zmqx.Sub)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if err := sub.Connect(dataEndpoint); err != nil {
		t.Fatal(err)
	}
	if err := sub.SetBytes(zmqx.Subscribe, []byte("session/test")); err != nil {
		t.Fatal(err)
	}
	if err := sub.SetBytes(zmqx.Subscribe, []byte("ready/client-1")); err != nil {
		t.Fatal(err)
	}

	dealer, err := zctx.Socket(zmqx.Dealer)
	if err != nil {
		t.Fatal(err)
	}
	defer dealer.Close()
	if err := dealer.SetBytes(zmqx.Identity, []byte("client-1")); err != nil {
		t.Fatal(err)
	}
	if err := dealer.Connect(controlEndpoint); err != nil {
		t.Fatal(err)
	}

	attached := controlCall(t, dealer, protocol.Message{Kind: protocol.KindAttach, Meta: protocol.Meta{Version: protocol.Version, Session: "test", RequestID: 1}})
	if attached.Kind != protocol.KindAttached || attached.Meta.RequestID != 1 || attached.Meta.HasControl {
		t.Fatalf("unexpected attach reply: %+v", attached)
	}
	if _, err := protocol.DecodeArchive(attached.Payload); err != nil {
		t.Fatal(err)
	}

	acquired := controlCall(t, dealer, protocol.Message{Kind: protocol.KindAcquireControl, Meta: protocol.Meta{Version: protocol.Version, Session: "test", RequestID: 2}})
	if acquired.Kind != protocol.KindPong || !acquired.Meta.HasControl || acquired.Meta.ControlOwner != "client-1" {
		t.Fatalf("unexpected acquire reply: %+v", acquired)
	}

	input := controlCall(t, dealer, protocol.Message{Kind: protocol.KindInput, Meta: protocol.Meta{Version: protocol.Version, Session: "test", RequestID: 3}, Payload: []byte("abc")})
	if input.Kind != protocol.KindPong || input.Meta.RequestID != 3 {
		t.Fatalf("unexpected input reply: %+v", input)
	}
	eventually(t, func() bool { return pty.String() == "abc" })
}

type stubReadyWaiter struct{}

func (stubReadyWaiter) WaitReady(context.Context, shenguard.ClientID) error { return nil }

// stubControlSocket stands in for the bound ROUTER. Replies addressed to an
// identity marked gone fail exactly as the pure-Go ROUTER fails once that
// peer's pipe has disappeared, which is the only way to exercise a client that
// vanishes between its request and its reply without racing a real disconnect.
type stubControlSocket struct {
	mu      sync.Mutex
	queue   []stubControlEvent
	replies []stubControlReply
	gone    map[string]bool
	closed  bool
}

type stubControlEvent struct {
	frames [][]byte
	err    error
}

type stubControlReply struct {
	identity string
	msg      protocol.Message
}

func newStubControlSocket() *stubControlSocket {
	return &stubControlSocket{gone: make(map[string]bool)}
}

func (s *stubControlSocket) RecvMultipartLimit(_, _, maxFrames int) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return nil, zmqx.ErrWouldBlock
	}
	event := s.queue[0]
	s.queue = s.queue[1:]
	if event.err != nil {
		return nil, event.err
	}
	if len(event.frames) > maxFrames {
		// zmqx consumes the message and then refuses it, so the offending
		// message is gone by the time the caller sees the error.
		return nil, fmt.Errorf("zmq multipart message exceeds %d frames", maxFrames)
	}
	return event.frames, nil
}

func (s *stubControlSocket) SendMultipart(frames [][]byte, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return zmqx.ErrClosed
	}
	if len(frames) == 0 || len(frames[0]) == 0 {
		return zmq.ErrNoIdentity
	}
	if s.gone[string(frames[0])] {
		// Verbatim zmq4 ROUTER.Send behaviour for an identity with no pipe.
		return fmt.Errorf("%w: identity %x", zmq.ErrNoRoute, frames[0])
	}
	msg, err := protocol.Decode(frames[1:])
	if err != nil {
		return err
	}
	s.replies = append(s.replies, stubControlReply{identity: string(frames[0]), msg: msg})
	return nil
}

func (s *stubControlSocket) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// vanish makes every later reply to identity fail the way it fails once that
// client has disconnected.
func (s *stubControlSocket) vanish(identity string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gone[identity] = true
}

func (s *stubControlSocket) push(event stubControlEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, event)
}

func (s *stubControlSocket) deliver(t *testing.T, identity string, msg protocol.Message) {
	t.Helper()
	frames, err := protocol.Encode(msg)
	if err != nil {
		t.Fatal(err)
	}
	s.push(stubControlEvent{frames: append([][]byte{[]byte(identity)}, frames...)})
}

func (s *stubControlSocket) repliesTo(identity string) []protocol.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []protocol.Message
	for _, reply := range s.replies {
		if reply.identity == identity {
			out = append(out, reply.msg)
		}
	}
	return out
}

// awaitReply blocks until the control server answers one request, and fails
// with a message that says what it means when it never does.
func awaitReply(t *testing.T, socket *stubControlSocket, identity string, requestID uint64) protocol.Message {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, reply := range socket.repliesTo(identity) {
			if reply.Meta.RequestID != requestID {
				continue
			}
			if reply.Kind == protocol.KindError {
				t.Fatalf("control error for request %d: %s", requestID, reply.Meta.Error)
			}
			return reply
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("request %d from %s was never answered: the control server stopped serving", requestID, identity)
	return protocol.Message{}
}

// serveNextClient drives a fresh client all the way through attach, taking
// control, and typing, which is only possible if the session is still alive and
// the control lease is free.
func serveNextClient(t *testing.T, socket *stubControlSocket, cid shenguard.ClientID, pty *fakePTY) {
	t.Helper()
	socket.deliver(t, cid.String(), protocol.Message{Kind: protocol.KindAttach, Meta: protocol.Meta{Version: protocol.Version, Session: "test", RequestID: 11}})
	if reply := awaitReply(t, socket, cid.String(), 11); reply.Kind != protocol.KindAttached {
		t.Fatalf("attach reply = %+v, want an attached snapshot", reply)
	}
	socket.deliver(t, cid.String(), protocol.Message{Kind: protocol.KindAcquireControl, Meta: protocol.Meta{Version: protocol.Version, Session: "test", RequestID: 12}})
	if reply := awaitReply(t, socket, cid.String(), 12); !reply.Meta.HasControl || reply.Meta.ControlOwner != cid.String() {
		t.Fatalf("acquire reply = %+v, want the control lease held by %s", reply, cid)
	}
	socket.deliver(t, cid.String(), protocol.Message{Kind: protocol.KindInput, Meta: protocol.Meta{Version: protocol.Version, Session: "test", RequestID: 13}, Payload: []byte("hello")})
	awaitReply(t, socket, cid.String(), 13)
	eventually(t, func() bool { return pty.String() == "hello" })
}

// TestControlServerSurvivesAClientThatVanishesMidReply covers the failure that
// killed a live session: a web client had gone away, its reply could not be
// routed, and the daemon treated that per-peer failure as a dead server. The
// session, its shell and its scrollback all went with it.
func TestControlServerSurvivesAClientThatVanishesMidReply(t *testing.T) {
	runtime, pty, _ := newTestRuntime(t)
	gone := testCID(t, "web-e91ea4799bb00e0ac4b18599")
	next := testCID(t, "client-next")
	if _, err := runtime.AttachSnapshot(gone); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AcquireControl(gone); err != nil {
		t.Fatal(err)
	}
	if owner := runtime.Status().ControlOwner; owner != gone.String() {
		t.Fatalf("control owner = %q, want %q", owner, gone)
	}

	socket := newStubControlSocket()
	socket.vanish(gone.String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newControlServer(ctx, socket, "test", runtime, stubReadyWaiter{})
	defer server.Close()

	// The tab is closed, but a request it sent before closing is still queued.
	socket.deliver(t, gone.String(), protocol.Message{Kind: protocol.KindPing, Meta: protocol.Meta{Version: protocol.Version, Session: "test", RequestID: 1}})

	// The vanished client held the exclusive lease. Dropping it has to run the
	// same teardown a clean detach runs, or the lease stays parked on a client
	// id nobody can reach and no later client can ever type.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.Status().ControlOwner != "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if owner := runtime.Status().ControlOwner; owner != "" {
		t.Fatalf("control lease still owned by %q after that client vanished", owner)
	}

	serveNextClient(t, socket, next, pty)

	select {
	case err, ok := <-server.Errors():
		t.Fatalf("control server reported err=%v (open=%t); one departed client must not be fatal", err, ok)
	default:
	}
}

// TestControlServerSurvivesAnUnreadableClientMessage covers the same defect on
// the receive side: a message the control plane refuses to parse is one
// client's fault and must not end the session either.
func TestControlServerSurvivesAnUnreadableClientMessage(t *testing.T) {
	runtime, pty, _ := newTestRuntime(t)
	socket := newStubControlSocket()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newControlServer(ctx, socket, "test", runtime, stubReadyWaiter{})
	defer server.Close()

	// More frames than a control request may carry.
	socket.push(stubControlEvent{frames: [][]byte{[]byte("noisy"), {1}, {2}, {3}, {4}}})

	serveNextClient(t, socket, testCID(t, "client-next"), pty)

	select {
	case err, ok := <-server.Errors():
		t.Fatalf("control server reported err=%v (open=%t); one bad message must not be fatal", err, ok)
	default:
	}
}

// TestControlServerReportsADeadSocketAsFatal is the other half of the
// classification: a socket that is genuinely gone must still end the server,
// because it can no longer serve anybody.
func TestControlServerReportsADeadSocketAsFatal(t *testing.T) {
	runtime, _, _ := newTestRuntime(t)
	socket := newStubControlSocket()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newControlServer(ctx, socket, "test", runtime, stubReadyWaiter{})
	defer server.Close()

	socket.push(stubControlEvent{err: zmqx.ErrClosed})
	select {
	case err := <-server.Errors():
		if !errors.Is(err, zmqx.ErrClosed) {
			t.Fatalf("fatal error = %v, want %v", err, zmqx.ErrClosed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a dead control socket was not reported as fatal")
	}
}

func controlCall(t *testing.T, socket *zmqx.Socket, msg protocol.Message) protocol.Message {
	t.Helper()
	frames, err := protocol.Encode(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := socket.SendMultipart(frames, 0); err != nil {
		t.Fatal(err)
	}
	replyFrames, err := socket.RecvMultipartLimit(0, protocol.MaxPayloadSize, 3)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := protocol.Decode(replyFrames)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Kind == protocol.KindError {
		t.Fatalf("control error: %s", reply.Meta.Error)
	}
	return reply
}

// A reply this server cannot encode is a defect in one answer, not in the
// socket. It used to travel to Errors() and end the session, which is exactly
// the failure classifying send errors exists to prevent -- one frame further
// up, where the original fix did not look.
func TestControlServerSurvivesAReplyItCannotEncode(t *testing.T) {
	// An error string past the protocol's payload ceiling cannot be encoded,
	// so replyError fails at protocol.Encode rather than at the socket.
	unencodable := strings.Repeat("x", protocol.MaxPayloadSize+1)
	err := sendControl(newStubControlSocket(), []byte("client-a"), protocol.Message{
		Kind: protocol.KindError,
		Meta: protocol.Meta{Version: protocol.Version, Session: "test", Error: unencodable},
	})
	if err == nil {
		t.Fatal("precondition: this reply should fail to encode")
	}

	var encode *encodeFault
	if !errors.As(err, &encode) {
		t.Fatalf("an unencodable reply must be an encodeFault, got %T: %v", err, err)
	}
	// The fatal path keys on everything that is not a client fault, so an
	// encode failure reaching it would end the session.
	if peerGone(err) {
		t.Fatal("an encode failure must not be misreported as a departed peer")
	}
}
