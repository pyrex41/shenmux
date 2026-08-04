package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gorilla/websocket"
	muxclient "github.com/pyrex41/shenmux/client"
	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/relay"
	"github.com/pyrex41/shenmux/internal/screen"
)

type fakeSession struct {
	events chan protocol.Message
	closed bool
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
func (f *fakeSession) Detach(context.Context) error                           { return nil }
func (f *fakeSession) Events() <-chan protocol.Message                        { return f.events }
func (f *fakeSession) Close() error                                           { f.closed = true; close(f.events); return nil }

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

type httpHandler func(*websocket.Conn)

func (h httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	h(conn)
}
