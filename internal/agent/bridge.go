// Package agent bridges authenticated relay streams to sessions hosted by the
// local muxd instance. A Kubernetes sidecar can therefore share a Unix socket
// volume with the harness container while exposing the session through WSS.
package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

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

func (b *Bridge) open(ctx context.Context, session string) (sessionClient, error) {
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
	return muxclient.New(ctx, muxclient.Config{Session: session, ClientID: "relay-" + b.DeviceID, ControlEndpoint: control, DataEndpoint: data})
}

// Serve reads relay messages until the tunnel closes. The caller owns the
// WebSocket write lock; Serve serializes all bridge output itself.
func (b *Bridge) Serve(ctx context.Context, conn *websocket.Conn) error {
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
				return err
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
				return fmt.Errorf("decrypt remote session: %w", err)
			}
			env = opened
		}
		frames, err := relay.UnmarshalFrames(env.Payload)
		if err != nil {
			return fmt.Errorf("decode remote session: %w", err)
		}
		msg, err := protocol.Decode(frames)
		if err != nil {
			return fmt.Errorf("decode remote session message: %w", err)
		}
		if err := b.handleMessage(ctx, conn, env, msg); err != nil {
			return err
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
		client, err := b.open(ctx, env.Header.SessionID)
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
	client, err := b.open(ctx, env.Header.SessionID)
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
		client, err := b.open(ctx, env.Header.SessionID)
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
		active.cancel()
		_ = active.client.Close()
	}
}
