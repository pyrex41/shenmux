package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type fakeDialer struct{ failures map[string]error }

func (d fakeDialer) Dial(_ context.Context, e Endpoint) (Session, error) {
	if err := d.failures[e.URL]; err != nil {
		return nil, err
	}
	return fakeSession{}, nil
}

type fakeSession struct{}

func (fakeSession) Send(context.Context, []byte) error   { return nil }
func (fakeSession) Recv(context.Context) ([]byte, error) { return nil, nil }
func (fakeSession) Close() error                         { return nil }

func TestSelectorPrefersDirectThenRelay(t *testing.T) {
	h := NewHealth()
	selector := Selector{Health: h}
	endpoints := []Endpoint{{Kind: KindRelay, URL: "wss://relay"}, {Kind: KindDirect, URL: "wireguard://peer"}}
	ordered := selector.Order(endpoints, time.Now())
	if ordered[0].Kind != KindDirect {
		t.Fatalf("order = %+v", ordered)
	}
	h.RecordFailure(ordered[0], time.Now())
	ordered = selector.Order(endpoints, time.Now())
	if ordered[0].Kind != KindRelay {
		t.Fatalf("fallback order = %+v", ordered)
	}
}

func TestConnectFallsBackAndRecordsHealth(t *testing.T) {
	h := NewHealth()
	endpoints := []Endpoint{{Kind: KindDirect, URL: "direct"}, {Kind: KindRelay, URL: "relay"}}
	session, endpoint, err := Connect(context.Background(), fakeDialer{failures: map[string]error{"direct": errors.New("unreachable")}}, Selector{Health: h}, endpoints)
	if err != nil || session == nil || endpoint.Kind != KindRelay {
		t.Fatalf("connect = %v, %s", err, endpoint.Kind)
	}
}

func TestWebSocketDialerConcreteDirectPath(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err == nil {
			_ = conn.WriteMessage(websocket.BinaryMessage, payload)
		}
	}))
	defer server.Close()
	url := "ws" + server.URL[len("http"):]
	session, err := (WebSocketDialer{Timeout: time.Second}).Dial(context.Background(), Endpoint{Kind: KindDirect, URL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Send(context.Background(), []byte("direct")); err != nil {
		t.Fatal(err)
	}
	got, err := session.Recv(context.Background())
	if err != nil || string(got) != "direct" {
		t.Fatalf("recv = %q, %v", got, err)
	}
}
