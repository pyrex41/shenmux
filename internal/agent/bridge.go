// Package agent bridges authenticated relay streams to sessions hosted by the
// local muxd instance. A Kubernetes sidecar can therefore share a Unix socket
// volume with the harness container while exposing the session through WSS.
package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	muxclient "github.com/pyrex41/shenmux/client"
	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/relay"
)

type sessionClient interface {
	Attach(context.Context) (muxclient.Snapshot, error)
	Resync(context.Context) (muxclient.Snapshot, error)
	Input(context.Context, []byte) error
	Resize(context.Context, int, int) error
	AcquireControl(context.Context) (protocol.Meta, error)
	ReleaseControl(context.Context) (protocol.Meta, error)
	Detach(context.Context) error
	Events() <-chan protocol.Message
	Close() error
}

type clientFactory func(context.Context, string) (sessionClient, error)

type stream struct {
	client     sessionClient
	blind      *relay.BlindStreamSession
	outCounter uint64
	inCounter  relay.CounterTracker
	cancel     context.CancelFunc
	mu         sync.Mutex
}

// Bridge multiplexes any number of relay streams onto local muxd sessions.
type Bridge struct {
	DeviceID string
	// TrustMode is the minimum content trust mode accepted for remote streams.
	// Trusted mode remains the default for backwards compatibility; blind mode
	// is negotiated per stream and never silently downgraded.
	TrustMode  relay.TrustMode
	PrivateKey ed25519.PrivateKey
	ControlFor func(string) (string, string, error)
	OpenClient clientFactory
	mu         sync.Mutex
	writeMu    sync.Mutex
	streams    map[string]*stream
}

func NewBridge(deviceID string, controlFor func(string) (string, string, error)) *Bridge {
	return &Bridge{DeviceID: deviceID, TrustMode: relay.TrustTrusted, ControlFor: controlFor, streams: make(map[string]*stream)}
}

func (b *Bridge) open(ctx context.Context, session, streamID string) (sessionClient, error) {
	if b.OpenClient != nil {
		return b.OpenClient(ctx, session)
	}
	if b.ControlFor == nil {
		return nil, errors.New("agent bridge has no local endpoint resolver")
	}
	control, data, err := b.ControlFor(session)
	if err != nil {
		return nil, err
	}
	return muxclient.New(ctx, muxclient.Config{Session: session, ClientID: localClientID(b.DeviceID, session, streamID), ControlEndpoint: control, DataEndpoint: data})
}

func localClientID(deviceID, session, streamID string) string {
	digest := sha256.Sum256([]byte(deviceID + "\x00" + session + "\x00" + streamID))
	return fmt.Sprintf("relay-%x", digest[:16])
}

// Serve reads relay messages until the tunnel closes. The caller owns the
// WebSocket write lock; Serve serializes all bridge output itself.
func (b *Bridge) Serve(ctx context.Context, conn *websocket.Conn) error {
	defer b.closeAllStreams()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		env, err := relay.Decode(data)
		if err != nil || env.Header.DeviceID != b.DeviceID {
			continue
		}
		if env.Header.FrameType == relay.FrameClose {
			b.closeStream(env.Header.StreamID)
			continue
		}
		if env.Header.FrameType == relay.FrameOpen {
			if err := b.handleOpen(ctx, conn, env); err != nil {
				b.rejectStream(conn, env, err)
			}
			continue
		}
		if env.Header.FrameType != relay.FrameSession && env.Header.FrameType != relay.FrameData {
			continue
		}
		b.mu.Lock()
		active := b.streams[env.Header.StreamID]
		b.mu.Unlock()
		if active != nil && active.blind != nil {
			opened, err := active.blind.OpenData(env)
			if err != nil {
				b.rejectStream(conn, env, fmt.Errorf("decrypt remote session: %w", err))
				continue
			}
			env = opened
		}
		frames, err := relay.UnmarshalFrames(env.Payload)
		if err != nil {
			b.rejectStream(conn, env, fmt.Errorf("decode remote session: %w", err))
			continue
		}
		msg, err := protocol.Decode(frames)
		if err != nil {
			b.rejectStream(conn, env, fmt.Errorf("decode remote session message: %w", err))
			continue
		}
		if err := b.handleMessage(ctx, conn, env, msg); err != nil {
			b.rejectStream(conn, env, err)
		}
	}
}

type blindOpenPresentation struct {
	Token      string          `json:"token"`
	Permission string          `json:"permission"`
	Blind      json.RawMessage `json:"blind"`
}

func (b *Bridge) handleOpen(ctx context.Context, conn *websocket.Conn, env relay.Envelope) error {
	b.mu.Lock()
	active := b.streams[env.Header.StreamID]
	b.mu.Unlock()
	if active != nil && active.blind != nil {
		if err := active.inCounter.Accept(env.Header.Counter); err != nil {
			return fmt.Errorf("replayed blind handshake frame: %w", err)
		}
		response, err := active.blind.HandleOpen(env)
		if err != nil {
			return err
		}
		if response.Payload != nil {
			return b.writeEnvelope(conn, response, active)
		}
		if !active.blind.Ready() {
			return nil
		}
		if active.client != nil {
			return nil
		}
		client, err := b.open(ctx, env.Header.SessionID, env.Header.StreamID)
		if err != nil {
			b.closeStream(env.Header.StreamID)
			return err
		}
		active.client = client
		streamCtx, cancel := context.WithCancel(ctx)
		active.cancel = cancel
		go b.forwardEvents(streamCtx, conn, env.Header.SessionID, env.Header.StreamID, active)
		return b.handleMessageAccepted(ctx, conn, env, protocol.Message{Kind: protocol.KindAttach, Meta: protocol.Meta{Version: protocol.Version, Session: env.Header.SessionID}}, active)
	}
	var presentation blindOpenPresentation
	if err := json.Unmarshal(env.Payload, &presentation); err != nil || len(presentation.Blind) == 0 {
		if b.TrustMode == relay.TrustBlind {
			return errors.New("blind trust mode requires an encrypted stream handshake")
		}
		return b.handleMessage(ctx, conn, env, protocol.Message{Kind: protocol.KindAttach, Meta: protocol.Meta{Version: protocol.Version, Session: env.Header.SessionID}})
	}
	if len(b.PrivateKey) != ed25519.PrivateKeySize {
		return errors.New("blind trust mode requires the enrolled device key")
	}
	var wire struct {
		Binding relay.StreamBinding `json:"binding"`
	}
	if err := json.Unmarshal(presentation.Blind, &wire); err != nil {
		return fmt.Errorf("decode blind binding: %w", err)
	}
	binding := wire.Binding
	if binding.DeviceID != b.DeviceID || binding.SessionID != env.Header.SessionID || binding.StreamID != env.Header.StreamID || binding.Permission != presentation.Permission {
		return errors.New("blind stream binding does not match relay routing")
	}
	if binding.Subject == "" || presentation.Token == "" {
		return errors.New("blind stream binding is missing capability identity")
	}
	// The capability token is the opaque authorization value validated by the
	// controller. Recompute the binding from it so a browser cannot substitute
	// a different digest in the encrypted-stream transcript.
	bound, err := relay.NewStreamBinding(binding.DeviceID, binding.SessionID, binding.StreamID, binding.Subject, binding.Permission, []byte(presentation.Token))
	if err != nil || bound.CapabilityDigest != binding.CapabilityDigest {
		return errors.New("blind stream capability binding mismatch")
	}
	b.mu.Lock()
	if _, exists := b.streams[env.Header.StreamID]; exists {
		b.mu.Unlock()
		return errors.New("blind stream already exists")
	}
	active = &stream{blind: nil, outCounter: 0, cancel: func() {}}
	b.streams[env.Header.StreamID] = active
	b.mu.Unlock()
	blind, err := relay.NewBlindAgentStream(binding, b.PrivateKey, b.TrustMode)
	if err != nil {
		b.closeStream(env.Header.StreamID)
		return err
	}
	active.blind = blind
	if err := active.inCounter.Accept(env.Header.Counter); err != nil {
		b.closeStream(env.Header.StreamID)
		return err
	}
	response, err := blind.HandleOpen(env)
	if err != nil {
		b.closeStream(env.Header.StreamID)
		return err
	}
	if response.Payload != nil {
		return b.writeEnvelope(conn, response, active)
	}
	if !blind.Ready() {
		return nil
	}
	client, err := b.open(ctx, env.Header.SessionID, env.Header.StreamID)
	if err != nil {
		b.closeStream(env.Header.StreamID)
		return err
	}
	active.client = client
	streamCtx, cancel := context.WithCancel(ctx)
	active.cancel = cancel
	go b.forwardEvents(streamCtx, conn, env.Header.SessionID, env.Header.StreamID, active)
	return b.handleMessageAccepted(ctx, conn, env, protocol.Message{Kind: protocol.KindAttach, Meta: protocol.Meta{Version: protocol.Version, Session: env.Header.SessionID}}, active)
}

func (b *Bridge) handleMessage(ctx context.Context, conn *websocket.Conn, env relay.Envelope, msg protocol.Message) error {
	b.mu.Lock()
	active := b.streams[env.Header.StreamID]
	b.mu.Unlock()
	if active != nil {
		if err := active.inCounter.Accept(env.Header.Counter); err != nil {
			return fmt.Errorf("replayed relay frame: %w", err)
		}
	}
	return b.handleMessageAccepted(ctx, conn, env, msg, active)
}

func (b *Bridge) handleMessageAccepted(ctx context.Context, conn *websocket.Conn, env relay.Envelope, msg protocol.Message, active *stream) error {
	if active == nil {
		if msg.Kind != protocol.KindAttach {
			return fmt.Errorf("session stream %q was not attached", env.Header.StreamID)
		}
		client, err := b.open(ctx, env.Header.SessionID, env.Header.StreamID)
		if err != nil {
			return err
		}
		streamCtx, cancel := context.WithCancel(ctx)
		active = &stream{client: client, outCounter: 0, cancel: cancel}
		if err := active.inCounter.Accept(env.Header.Counter); err != nil {
			cancel()
			_ = client.Close()
			return fmt.Errorf("invalid initial relay counter: %w", err)
		}
		b.mu.Lock()
		b.streams[env.Header.StreamID] = active
		b.mu.Unlock()
		go b.forwardEvents(streamCtx, conn, env.Header.SessionID, env.Header.StreamID, active)
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var response protocol.Message
	switch msg.Kind {
	case protocol.KindAttach:
		snapshot, err := active.client.Attach(callCtx)
		if err != nil {
			return err
		}
		archive, err := protocol.EncodeArchive(snapshot.Archive)
		if err != nil {
			return err
		}
		response = protocol.Message{Kind: protocol.KindAttached, Meta: snapshot.Meta, Payload: archive}
	case protocol.KindResync:
		snapshot, err := active.client.Resync(callCtx)
		if err != nil {
			return err
		}
		archive, err := protocol.EncodeArchive(snapshot.Archive)
		if err != nil {
			return err
		}
		response = protocol.Message{Kind: protocol.KindAttached, Meta: snapshot.Meta, Payload: archive}
	case protocol.KindInput:
		if err := active.client.Input(callCtx, msg.Payload); err != nil {
			return err
		}
		response = protocol.Message{Kind: protocol.KindPong, Meta: msg.Meta}
	case protocol.KindResize:
		if err := active.client.Resize(callCtx, int(msg.Meta.Cols), int(msg.Meta.Rows)); err != nil {
			return err
		}
		response = protocol.Message{Kind: protocol.KindPong, Meta: msg.Meta}
	case protocol.KindAcquireControl:
		meta, err := active.client.AcquireControl(callCtx)
		if err != nil {
			return err
		}
		response = protocol.Message{Kind: protocol.KindPong, Meta: meta}
	case protocol.KindReleaseControl:
		meta, err := active.client.ReleaseControl(callCtx)
		if err != nil {
			return err
		}
		response = protocol.Message{Kind: protocol.KindPong, Meta: meta}
	case protocol.KindDetach:
		if err := active.client.Detach(callCtx); err != nil {
			return err
		}
		b.closeStream(env.Header.StreamID)
		response = protocol.Message{Kind: protocol.KindPong, Meta: msg.Meta}
	default:
		return fmt.Errorf("unsupported remote session message %q", msg.Kind)
	}
	return b.writeMessage(conn, env, response, active)
}

func (b *Bridge) forwardEvents(ctx context.Context, conn *websocket.Conn, session, streamID string, active *stream) {
	defer b.closeStreamIf(streamID, active)
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-active.client.Events():
			if !ok {
				return
			}
			env := relay.Envelope{Header: relay.Header{FrameType: relay.FrameSession, DeviceID: b.DeviceID, SessionID: session, StreamID: streamID, Counter: active.outCounter}}
			if err := b.writeMessage(conn, env, msg, active); err != nil {
				return
			}
		}
	}
}

func (b *Bridge) writeEnvelope(conn *websocket.Conn, env relay.Envelope, active *stream) error {
	active.mu.Lock()
	defer active.mu.Unlock()
	active.outCounter++
	env.Header.DeviceID = b.DeviceID
	env.Header.Counter = active.outCounter
	encoded, err := env.Encode()
	if err != nil {
		return err
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return conn.WriteMessage(websocket.BinaryMessage, encoded)
}

func (b *Bridge) writeMessage(conn *websocket.Conn, env relay.Envelope, msg protocol.Message, active *stream) error {
	active.mu.Lock()
	defer active.mu.Unlock()
	frames, err := protocol.Encode(msg)
	if err != nil {
		return err
	}
	payload, err := relay.MarshalFrames(frames)
	if err != nil {
		return err
	}
	active.outCounter++
	env.Header.DeviceID = b.DeviceID
	env.Header.FrameType = relay.FrameSession
	env.Header.Counter = active.outCounter
	env.Payload = payload
	if active.blind != nil {
		env.Header.FrameType = relay.FrameData
		sealed, err := active.blind.SealData(env)
		if err != nil {
			return err
		}
		env = sealed
	}
	encoded, err := env.Encode()
	if err != nil {
		return err
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return conn.WriteMessage(websocket.BinaryMessage, encoded)
}

func (b *Bridge) closeStream(id string) {
	b.mu.Lock()
	active := b.streams[id]
	delete(b.streams, id)
	b.mu.Unlock()
	if active != nil {
		b.closeActive(active)
	}
}

func (b *Bridge) closeStreamIf(id string, want *stream) {
	b.mu.Lock()
	active := b.streams[id]
	if active == want {
		delete(b.streams, id)
	} else {
		active = nil
	}
	b.mu.Unlock()
	if active != nil {
		b.closeActive(active)
	}
}

func (b *Bridge) closeAllStreams() {
	b.mu.Lock()
	streams := b.streams
	b.streams = make(map[string]*stream)
	b.mu.Unlock()
	for _, active := range streams {
		b.closeActive(active)
	}
}

// teardownCallTimeout bounds each best-effort control call made while a stream
// is torn down. Teardown frequently runs because the tunnel already died, so
// these calls must fail fast instead of parking the bridge on a dead peer.
const teardownCallTimeout = time.Second

// closeActive tears a relay stream down deterministically.
//
// The session daemon has to learn at teardown time that this relay client is
// gone, otherwise the exclusive control lease stays parked on a client id that
// nobody can reach. localClientID derives the local client id from the relay
// stream id, so a browser that reconnects arrives as a *different* client and
// can never displace the stale owner: every later client (the rich local web
// UI, muxctl, anything) then fails AcquireControl with "session control is
// owned by another client" until the agent process is restarted.
//
// So release the lease and detach before closing, in that order. Both calls are
// best effort on their own short bounded context, and their errors are
// deliberately ignored: when the tunnel is already dead they will simply fail,
// and teardown must still complete. Releasing control this stream does not own
// is harmless -- the Shen release-control-ok? rule rejects it with "no-control"
// without mutating session state and without any daemon-side logging.
func (b *Bridge) closeActive(active *stream) {
	if active.client != nil {
		// Never reuse the stream context here: it is cancelled just below and
		// is usually cancelled already, which would abort both calls instantly.
		releaseCtx, cancelRelease := context.WithTimeout(context.Background(), teardownCallTimeout)
		_, _ = active.client.ReleaseControl(releaseCtx)
		cancelRelease()
		detachCtx, cancelDetach := context.WithTimeout(context.Background(), teardownCallTimeout)
		_ = active.client.Detach(detachCtx)
		cancelDetach()
	}
	if active.cancel != nil {
		active.cancel()
	}
	if active.client != nil {
		_ = active.client.Close()
	}
}

func (b *Bridge) rejectStream(conn *websocket.Conn, env relay.Envelope, cause error) {
	b.mu.Lock()
	active := b.streams[env.Header.StreamID]
	b.mu.Unlock()
	closed := relay.Envelope{Header: env.Header, Payload: []byte(cause.Error())}
	closed.Header.FrameType = relay.FrameClose
	if active != nil {
		_ = b.writeEnvelope(conn, closed, active)
	} else if encoded, err := closed.Encode(); err == nil {
		b.writeMu.Lock()
		_ = conn.WriteMessage(websocket.BinaryMessage, encoded)
		b.writeMu.Unlock()
	}
	b.closeStream(env.Header.StreamID)
}
