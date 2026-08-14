package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	muxclient "github.com/pyrex41/shenmux/client"
	"github.com/pyrex41/shenmux/protocol"
	"github.com/pyrex41/shenmux/internal/relay"
	"github.com/pyrex41/shenmux/screen"
)

type fakeSession struct {
	events chan protocol.Message
	closed bool
	once   sync.Once

	mu         sync.Mutex
	calls      []string
	releaseErr error
	detachErr  error
}

func (f *fakeSession) record(name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
}

func (f *fakeSession) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSession) Attach(context.Context) (muxclient.Snapshot, error) {
	state, err := screen.BlankState(80, 24)
	if err != nil {
		return muxclient.Snapshot{}, err
	}
	archive := protocol.Archive{Format: 2, HistoryLimit: 10, Checkpoint: protocol.Checkpoint{Screen: state}}
	return muxclient.Snapshot{Meta: protocol.Meta{Version: protocol.Version, Seq: 0, CheckpointSeq: 0}, Archive: archive, Current: archive.Checkpoint}, nil
}
func (f *fakeSession) Resync(ctx context.Context) (muxclient.Snapshot, error) { return f.Attach(ctx) }
func (f *fakeSession) Input(context.Context, []byte) error                    { return nil }
func (f *fakeSession) Resize(context.Context, int, int) error                 { return nil }
func (f *fakeSession) AcquireControl(context.Context) (protocol.Meta, error) {
	return protocol.Meta{Version: protocol.Version}, nil
}
func (f *fakeSession) ReleaseControl(context.Context) (protocol.Meta, error) {
	f.record("release")
	f.mu.Lock()
	err := f.releaseErr
	f.mu.Unlock()
	if err != nil {
		return protocol.Meta{}, err
	}
	return protocol.Meta{Version: protocol.Version}, nil
}
func (f *fakeSession) Detach(context.Context) error {
	f.record("detach")
	f.mu.Lock()
	err := f.detachErr
	f.mu.Unlock()
	return err
}
func (f *fakeSession) Events() <-chan protocol.Message { return f.events }
func (f *fakeSession) Close() error {
	f.record("close")
	f.once.Do(func() {
		f.closed = true
		close(f.events)
	})
	return nil
}

func TestBridgeMultiplexesAttachToLocalSession(t *testing.T) {
	fake := &fakeSession{events: make(chan protocol.Message)}
	bridge := NewBridge("device-1", nil)
	bridge.OpenClient = func(context.Context, string) (sessionClient, error) { return fake, nil }
	server := httptest.NewServer(httpHandler(func(conn *websocket.Conn) {
		if err := bridge.Serve(context.Background(), conn); err == nil {
			t.Error("bridge returned without websocket close")
		}
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	endpoint.Scheme = "ws"
	conn, _, err := websocket.DefaultDialer.Dial(endpoint.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	frames, err := protocol.Encode(protocol.Message{Kind: protocol.KindAttach, Meta: protocol.Meta{Version: protocol.Version, Session: "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := relay.MarshalFrames(frames)
	data, _ := (relay.Envelope{Header: relay.Header{FrameType: relay.FrameSession, DeviceID: "device-1", SessionID: "codex", StreamID: "stream-1", Counter: 1}, Payload: payload}).Encode()
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		t.Fatal(err)
	}
	_, response, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	env, err := relay.Decode(response)
	if err != nil || env.Header.FrameType != relay.FrameSession {
		t.Fatalf("response envelope = %+v, %v", env.Header, err)
	}
	decoded, err := relay.UnmarshalFrames(env.Payload)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.Decode(decoded)
	if err != nil || msg.Kind != protocol.KindAttached {
		t.Fatalf("response message = %+v, %v", msg, err)
	}
}

func TestBridgeBlindStreamEncryptsSessionFrames(t *testing.T) {
	fake := &fakeSession{events: make(chan protocol.Message)}
	seed := bytes.Repeat([]byte{0x37}, ed25519.SeedSize)
	_, private, err := ed25519.GenerateKey(bytes.NewReader(seed))
	if err != nil {
		t.Fatal(err)
	}
	bridge := NewBridge("device-1", nil)
	bridge.TrustMode = relay.TrustBlind
	bridge.PrivateKey = private
	bridge.OpenClient = func(context.Context, string) (sessionClient, error) { return fake, nil }
	server := httptest.NewServer(httpHandler(func(conn *websocket.Conn) {
		if err := bridge.Serve(context.Background(), conn); err == nil {
			t.Error("bridge returned without websocket close")
		}
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	endpoint.Scheme = "ws"
	conn, _, err := websocket.DefaultDialer.Dial(endpoint.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	public := private.Public().(ed25519.PublicKey)
	binding, err := relay.NewStreamBinding("device-1", "codex", "blind-1", "alice", "control", []byte("cap-token"))
	if err != nil {
		t.Fatal(err)
	}
	client, hello, err := relay.NewBlindClientStream(binding, public)
	if err != nil {
		t.Fatal(err)
	}
	blindHello, err := json.Marshal(struct {
		ID         string          `json:"id"`
		Token      string          `json:"token"`
		Permission string          `json:"permission"`
		Blind      json.RawMessage `json:"blind"`
	}{ID: "cap-1", Token: "cap-token", Permission: "control", Blind: hello.Payload})
	if err != nil {
		t.Fatal(err)
	}
	open := relay.Envelope{Header: relay.Header{FrameType: relay.FrameOpen, DeviceID: "device-1", SessionID: "codex", StreamID: "blind-1", Counter: 1}, Payload: blindHello}
	openBytes, _ := open.Encode()
	if err := conn.WriteMessage(websocket.BinaryMessage, openBytes); err != nil {
		t.Fatal(err)
	}
	_, responseBytes, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	response, err := relay.Decode(responseBytes)
	if err != nil || response.Header.FrameType != relay.FrameOpen {
		t.Fatalf("blind response = %+v, %v", response.Header, err)
	}
	finish, err := client.HandleOpen(response)
	if err != nil {
		t.Fatal(err)
	}
	finish.Header.Counter = 2
	finishBytes, _ := finish.Encode()
	if err := conn.WriteMessage(websocket.BinaryMessage, finishBytes); err != nil {
		t.Fatal(err)
	}
	_, attachedBytes, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	attachedEnv, err := relay.Decode(attachedBytes)
	if err != nil || attachedEnv.Header.FrameType != relay.FrameData {
		t.Fatalf("blind attached envelope = %+v, %v", attachedEnv.Header, err)
	}
	attached, err := client.OpenData(attachedEnv)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := relay.UnmarshalFrames(attached.Payload)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.Decode(frames)
	if err != nil || msg.Kind != protocol.KindAttached {
		t.Fatalf("blind attached message = %+v, %v", msg, err)
	}
	inputFrames, _ := protocol.Encode(protocol.Message{Kind: protocol.KindInput, Meta: protocol.Meta{Version: protocol.Version, Session: "codex"}, Payload: []byte("ls\n")})
	inputPayload, _ := relay.MarshalFrames(inputFrames)
	input := relay.Envelope{Header: relay.Header{FrameType: relay.FrameData, DeviceID: "device-1", SessionID: "codex", StreamID: "blind-1", Counter: 3}, Payload: inputPayload}
	encryptedInput, err := client.SealData(input)
	if err != nil {
		t.Fatal(err)
	}
	encodedInput, _ := encryptedInput.Encode()
	if bytes.Contains(encodedInput, inputPayload) {
		t.Fatal("blind input exposed the inner session payload")
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, encodedInput); err != nil {
		t.Fatal(err)
	}
	_, pongBytes, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	pongEnv, err := relay.Decode(pongBytes)
	if err != nil || pongEnv.Header.FrameType != relay.FrameData {
		t.Fatalf("blind pong envelope = %+v, %v", pongEnv.Header, err)
	}
	pong, err := client.OpenData(pongEnv)
	if err != nil {
		t.Fatal(err)
	}
	pongFrames, _ := relay.UnmarshalFrames(pong.Payload)
	pongMsg, err := protocol.Decode(pongFrames)
	if err != nil || pongMsg.Kind != protocol.KindPong {
		t.Fatalf("blind pong message = %+v, %v", pongMsg, err)
	}
}

func TestBridgeMalformedStreamDoesNotCloseTunnel(t *testing.T) {
	bridge := NewBridge("device-1", nil)
	bridge.OpenClient = func(context.Context, string) (sessionClient, error) {
		return &fakeSession{events: make(chan protocol.Message)}, nil
	}
	server := httptest.NewServer(httpHandler(func(conn *websocket.Conn) {
		_ = bridge.Serve(context.Background(), conn)
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	endpoint.Scheme = "ws"
	conn, _, err := websocket.DefaultDialer.Dial(endpoint.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	bad, _ := (relay.Envelope{Header: relay.Header{FrameType: relay.FrameSession, DeviceID: "device-1", SessionID: "shell", StreamID: "bad", Counter: 1}, Payload: []byte("not framed")}).Encode()
	if err := conn.WriteMessage(websocket.BinaryMessage, bad); err != nil {
		t.Fatal(err)
	}
	_, closedBytes, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	closed, err := relay.Decode(closedBytes)
	if err != nil || closed.Header.FrameType != relay.FrameClose || closed.Header.StreamID != "bad" {
		t.Fatalf("bad stream response = %+v, %v", closed.Header, err)
	}

	frames, _ := protocol.Encode(protocol.Message{Kind: protocol.KindAttach, Meta: protocol.Meta{Version: protocol.Version, Session: "shell"}})
	payload, _ := relay.MarshalFrames(frames)
	good, _ := (relay.Envelope{Header: relay.Header{FrameType: relay.FrameSession, DeviceID: "device-1", SessionID: "shell", StreamID: "good", Counter: 1}, Payload: payload}).Encode()
	if err := conn.WriteMessage(websocket.BinaryMessage, good); err != nil {
		t.Fatal(err)
	}
	_, attachedBytes, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("tunnel died with malformed peer stream: %v", err)
	}
	_, attached, err := relay.DecodeMessage(attachedBytes)
	if err != nil || attached.Kind != protocol.KindAttached {
		t.Fatalf("good stream response = %+v, %v", attached, err)
	}
}

func TestBridgeServeExitClosesEveryLocalStream(t *testing.T) {
	bridge := NewBridge("device-1", nil)
	var sessions []*fakeSession
	bridge.OpenClient = func(context.Context, string) (sessionClient, error) {
		fake := &fakeSession{events: make(chan protocol.Message)}
		sessions = append(sessions, fake)
		return fake, nil
	}
	done := make(chan struct{})
	server := httptest.NewServer(httpHandler(func(conn *websocket.Conn) {
		_ = bridge.Serve(context.Background(), conn)
		close(done)
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	endpoint.Scheme = "ws"
	conn, _, err := websocket.DefaultDialer.Dial(endpoint.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	frames, _ := protocol.Encode(protocol.Message{Kind: protocol.KindAttach, Meta: protocol.Meta{Version: protocol.Version, Session: "shell"}})
	payload, _ := relay.MarshalFrames(frames)
	for _, streamID := range []string{"one", "two"} {
		data, _ := (relay.Envelope{Header: relay.Header{FrameType: relay.FrameSession, DeviceID: "device-1", SessionID: "shell", StreamID: streamID, Counter: 1}, Payload: payload}).Encode()
		if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
			t.Fatal(err)
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not exit after tunnel close")
	}
	if len(sessions) != 2 || !sessions[0].closed || !sessions[1].closed {
		t.Fatalf("local streams not closed on exit: %#v", sessions)
	}
}

// tearDownAttachedStream attaches one relay stream to fake, drops the tunnel,
// and returns once Serve has finished tearing every local stream down.
func tearDownAttachedStream(t *testing.T, fake *fakeSession) {
	t.Helper()
	bridge := NewBridge("device-1", nil)
	bridge.OpenClient = func(context.Context, string) (sessionClient, error) { return fake, nil }
	done := make(chan struct{})
	server := httptest.NewServer(httpHandler(func(conn *websocket.Conn) {
		_ = bridge.Serve(context.Background(), conn)
		close(done)
	}))
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	endpoint.Scheme = "ws"
	conn, _, err := websocket.DefaultDialer.Dial(endpoint.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	frames, _ := protocol.Encode(protocol.Message{Kind: protocol.KindAttach, Meta: protocol.Meta{Version: protocol.Version, Session: "shell"}})
	payload, _ := relay.MarshalFrames(frames)
	data, _ := (relay.Envelope{Header: relay.Header{FrameType: relay.FrameSession, DeviceID: "device-1", SessionID: "shell", StreamID: "stream-1", Counter: 1}, Payload: payload}).Encode()
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatal(err)
	}
	// The browser tab goes away: the tunnel drops with the stream still
	// attached and still holding the exclusive control lease.
	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge teardown hung after tunnel close")
	}
}

func TestBridgeStreamTeardownReleasesControlAndDetaches(t *testing.T) {
	fake := &fakeSession{events: make(chan protocol.Message)}
	tearDownAttachedStream(t, fake)
	calls := fake.callLog()
	want := []string{"release", "detach", "close"}
	if len(calls) != len(want) {
		t.Fatalf("teardown calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("teardown calls = %v, want %v", calls, want)
		}
	}
	if !fake.closed {
		t.Fatal("teardown left the local session open")
	}
}

func TestBridgeStreamTeardownClosesWhenReleaseAndDetachFail(t *testing.T) {
	// Teardown normally runs because the tunnel already died, so the daemon
	// round trips usually fail. That must not abort or hang the teardown.
	fake := &fakeSession{
		events:     make(chan protocol.Message),
		releaseErr: errors.New("relay tunnel is gone"),
		detachErr:  errors.New("relay tunnel is gone"),
	}
	tearDownAttachedStream(t, fake)
	calls := fake.callLog()
	if len(calls) != 3 || calls[0] != "release" || calls[1] != "detach" || calls[2] != "close" {
		t.Fatalf("failed teardown calls = %v, want [release detach close]", calls)
	}
	if !fake.closed {
		t.Fatal("failed release/detach prevented the local session from closing")
	}
}

func TestLocalClientIDIsUniquePerStream(t *testing.T) {
	one := localClientID("device", "shell", "one")
	two := localClientID("device", "shell", "two")
	if one == two || one == "" || two == "" {
		t.Fatalf("local client IDs = %q and %q", one, two)
	}
}

type httpHandler func(*websocket.Conn)

func (h httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	h(conn)
}
