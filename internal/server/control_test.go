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
	mu       sync.Mutex
	queue    []stubControlEvent
	replies  []stubControlReply
	gone     map[string]bool
	stalled  map[string]bool
	attempts map[string]int
	released chan struct{}
	closed   bool
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
	return &stubControlSocket{
		gone:     make(map[string]bool),
		stalled:  make(map[string]bool),
		attempts: make(map[string]int),
		released: make(chan struct{}),
	}
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
	if s.closed {
		s.mu.Unlock()
		return zmqx.ErrClosed
	}
	if len(frames) == 0 || len(frames[0]) == 0 {
		s.mu.Unlock()
		return zmq.ErrNoIdentity
	}
	identity := string(frames[0])
	if s.stalled[identity] {
		// Verbatim tomi77/zmq4 behaviour for a peer that has stopped reading:
		// ROUTER.Send discards its context and p.send waits on the socket's own
		// close channel, so nothing short of closing the socket returns.
		s.attempts[identity]++
		released := s.released
		s.mu.Unlock()
		<-released
		return zmqx.ErrClosed
	}
	defer s.mu.Unlock()
	if s.gone[identity] {
		// Verbatim zmq4 ROUTER.Send behaviour for an identity with no pipe.
		return fmt.Errorf("%w: identity %x", zmq.ErrNoRoute, frames[0])
	}
	msg, err := protocol.Decode(frames[1:])
	if err != nil {
		return err
	}
	s.replies = append(s.replies, stubControlReply{identity: identity, msg: msg})
	return nil
}

func (s *stubControlSocket) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	// Closing the socket is the only thing that frees a send parked in the
	// driver, so the stub has to free them too or the test leaks the goroutine
	// it is measuring.
	close(s.released)
	return nil
}

// vanish makes every later reply to identity fail the way it fails once that
// client has disconnected.
func (s *stubControlSocket) vanish(identity string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gone[identity] = true
}

// stall makes every later reply to identity block until the socket closes,
// which is what the pure-Go ROUTER does once that peer's queue is full.
func (s *stubControlSocket) stall(identity string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stalled[identity] = true
}

// sendAttempts counts the sends to identity that actually reached the socket
// and parked there, which is the number of goroutines the control loop has left
// waiting on that one client.
func (s *stubControlSocket) sendAttempts(identity string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[identity]
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

// awaitErrorReply is awaitReply for the answers that are supposed to be errors.
func awaitErrorReply(t *testing.T, socket *stubControlSocket, identity string) protocol.Message {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, reply := range socket.repliesTo(identity) {
			if reply.Kind == protocol.KindError {
				return reply
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s was never told why its message was refused", identity)
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
	// A long lease deliberately: with the default short test lease the
	// background reaper would free the departed client's control lease on a
	// timer, and this test would pass whether or not dropClient ran at all.
	runtime, pty, _ := newTestRuntimeWith(t, protocol.DefaultStoreLimits(), time.Hour)
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

	// More frames than the receive path will hand back at all, so the message
	// is consumed and refused with no identity left to answer -- the one case
	// ControlServer.Errors documents as a deliberate silent drop.
	frames := make([][]byte, maxControlRecvFrames+1)
	frames[0] = []byte("noisy")
	for i := 1; i < len(frames); i++ {
		frames[i] = []byte{byte(i)}
	}
	socket.push(stubControlEvent{frames: frames})

	serveNextClient(t, socket, testCID(t, "client-next"), pty)

	select {
	case err, ok := <-server.Errors():
		t.Fatalf("control server reported err=%v (open=%t); one bad message must not be fatal", err, ok)
	default:
	}
}

// TestControlServerAnswersAMalformedFrameCount covers the third outcome
// handleControl used to have and never admitted to: a message it could neither
// parse nor reply to. A frame count the receive path hands back still carries
// the sender's ROUTER identity, so the sender can be told what it did instead
// of waiting out its own timeout for an answer that was never coming.
func TestControlServerAnswersAMalformedFrameCount(t *testing.T) {
	runtime, pty, _ := newTestRuntime(t)
	socket := newStubControlSocket()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newControlServer(ctx, socket, "test", runtime, stubReadyWaiter{})
	defer server.Close()

	noisy := testCID(t, "client-noisy")
	// One frame past the protocol but inside the receive cap, which is the
	// point of the slack in maxControlRecvFrames.
	socket.push(stubControlEvent{frames: [][]byte{[]byte(noisy.String()), {1}, {2}, {3}, {4}}})

	reply := awaitErrorReply(t, socket, noisy.String())
	if !strings.Contains(reply.Meta.Error, "frames") {
		t.Fatalf("error reply = %q, want it to say the frame count was wrong", reply.Meta.Error)
	}

	// A message too short to be a request is answerable for the same reason.
	socket.push(stubControlEvent{frames: [][]byte{[]byte(noisy.String()), {1}}})
	if len(socket.repliesTo(noisy.String())) == 0 {
		t.Fatal("precondition: the first refusal should already be recorded")
	}

	serveNextClient(t, socket, testCID(t, "client-next"), pty)

	select {
	case err, ok := <-server.Errors():
		t.Fatalf("control server reported err=%v (open=%t); one bad message must not be fatal", err, ok)
	default:
	}
}

// TestControlServerSurvivesAClientThatStopsTakingDelivery covers the hang that
// outlived the crash fix. ROUTER.Send takes a context.Context and never reads
// it, and under the driver's default blocking overflow policy a peer that fills
// its queue parks the sender until the socket closes. One such client used to
// stop the whole loop: no receives, no lease reaping, nobody else served, and
// ControlServer.Close blocked forever on <-s.done.
func TestControlServerSurvivesAClientThatStopsTakingDelivery(t *testing.T) {
	// A lease long enough that only the drop path can free it. With the default
	// test lease the reaper would release it on a timer and the assertion below
	// would pass without the code under test doing anything at all.
	runtime, pty, _ := newTestRuntimeWith(t, protocol.DefaultStoreLimits(), time.Hour)
	stuck := testCID(t, "web-e91ea4799bb00e0ac4b18599")
	next := testCID(t, "client-next")
	if _, err := runtime.AttachSnapshot(stuck); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AcquireControl(stuck); err != nil {
		t.Fatal(err)
	}
	if owner := runtime.Status().ControlOwner; owner != stuck.String() {
		t.Fatalf("control owner = %q, want %q", owner, stuck)
	}

	socket := newStubControlSocket()
	socket.stall(stuck.String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newControlServer(ctx, socket, "test", runtime, stubReadyWaiter{})
	// Close the socket first: that is what frees a send parked in the driver,
	// so it is also what lets the loop finish and Close return.
	defer func() {
		_ = socket.Close()
		_ = server.Close()
	}()

	// Two requests. The first must not hold the loop forever; the second must
	// not park a second goroutine behind the first.
	socket.deliver(t, stuck.String(), protocol.Message{Kind: protocol.KindPing, Meta: protocol.Meta{Version: protocol.Version, Session: "test", RequestID: 1}})
	socket.deliver(t, stuck.String(), protocol.Message{Kind: protocol.KindPing, Meta: protocol.Meta{Version: protocol.Version, Session: "test", RequestID: 2}})

	// A client that has stopped reading is a client that is gone, so it goes
	// through the same teardown, and the exclusive lease it was holding comes
	// back. Generous against a slow machine: the bound itself is 2s.
	deadline := time.Now().Add(10 * time.Second)
	for runtime.Status().ControlOwner != "" && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if owner := runtime.Status().ControlOwner; owner != "" {
		t.Fatalf("control lease still owned by %q after that client stopped taking delivery", owner)
	}

	serveNextClient(t, socket, next, pty)

	if got := socket.sendAttempts(stuck.String()); got != 1 {
		t.Fatalf("the loop parked %d sends on one client that stopped reading; want 1, or every request it keeps sending costs another goroutine", got)
	}

	select {
	case err, ok := <-server.Errors():
		t.Fatalf("control server reported err=%v (open=%t); one stalled client must not be fatal", err, ok)
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
	err := sendControl(newBoundedSender(newStubControlSocket()), []byte("client-a"), protocol.Message{
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
