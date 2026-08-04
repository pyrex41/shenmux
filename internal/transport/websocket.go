package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocketDialer is the concrete direct transport. It connects to an agent's
// authenticated WSS endpoint when a direct address is available; callers can
// pass the same Dialer to Connect with the relay endpoint as the next option.
// Authentication remains at the relay/session protocol layer.
type WebSocketDialer struct {
	Dialer  *websocket.Dialer
	Header  http.Header
	TLS     *tls.Config
	Timeout time.Duration
}

// TailscaleDialer dials a peer over an existing Tailscale/WireGuard tailnet.
// Tailscale supplies encryption and routing; this dialer only performs the
// WebSocket upgrade used by the session protocol. Keeping it as a distinct
// type makes transport selection explicit while sharing the hardened WSS
// implementation and timeout behavior.
type TailscaleDialer struct{ WebSocketDialer }

func (d TailscaleDialer) Dial(ctx context.Context, endpoint Endpoint) (Session, error) {
	if endpoint.Kind != KindDirect && endpoint.Kind != KindTailscale {
		return nil, errors.New("tailscale dialer requires a direct/tailscale endpoint")
	}
	return d.WebSocketDialer.Dial(ctx, endpoint)
}

func (d WebSocketDialer) Dial(ctx context.Context, endpoint Endpoint) (Session, error) {
	if endpoint.URL == "" {
		return nil, errors.New("direct transport endpoint is empty")
	}
	// Tailnet/WireGuard endpoints are intentionally plain ws: the underlying
	// link is already encrypted by WireGuard. Accept both tailscale:// and
	// wireguard:// configuration forms and translate them for Gorilla.
	rawURL := endpoint.URL
	if parsed, err := url.Parse(rawURL); err == nil {
		switch strings.ToLower(parsed.Scheme) {
		case "tailscale", "ts", "wireguard", "wg":
			parsed.Scheme = "ws"
			rawURL = parsed.String()
		}
	}
	dialer := d.Dialer
	if dialer == nil {
		dialer = &websocket.Dialer{TLSClientConfig: d.TLS}
	}
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}
	conn, _, err := dialer.DialContext(ctx, rawURL, d.Header)
	if err != nil {
		return nil, err
	}
	return &webSocketSession{conn: conn}, nil
}

type webSocketSession struct{ conn *websocket.Conn }

func (s *webSocketSession) Send(ctx context.Context, payload []byte) error {
	if err := s.conn.SetWriteDeadline(deadline(ctx)); err != nil {
		return err
	}
	return s.conn.WriteMessage(websocket.BinaryMessage, payload)
}

func (s *webSocketSession) Recv(ctx context.Context) ([]byte, error) {
	if err := s.conn.SetReadDeadline(deadline(ctx)); err != nil {
		return nil, err
	}
	_, payload, err := s.conn.ReadMessage()
	return payload, err
}

func (s *webSocketSession) Close() error { return s.conn.Close() }

func deadline(ctx context.Context) time.Time {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return time.Time{}
}
