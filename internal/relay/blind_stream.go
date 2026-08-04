package relay

// Blind stream adapters bind the reviewed StreamCipher handshake to the
// relay's OPEN/DATA/CLOSE envelopes. Controllers never call these helpers:
// they forward the encoded bytes and only inspect routing metadata.

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type blindOpenWire struct {
	Kind     string              `json:"kind"`
	Binding  StreamBinding       `json:"binding"`
	Hello    *BlindClientHello   `json:"hello,omitempty"`
	Response *BlindAgentResponse `json:"response,omitempty"`
	Finish   *BlindClientFinish  `json:"finish,omitempty"`
}

type blindCapabilityOpen struct {
	ID         string          `json:"id,omitempty"`
	Token      string          `json:"token,omitempty"`
	Permission string          `json:"permission,omitempty"`
	Blind      json.RawMessage `json:"blind"`
}

// BlindStreamSession is an endpoint-side stream state machine. A client is
// created with NewBlindClientStream and an agent with NewBlindAgentStream.
// HandleOpen consumes the peer's OPEN handshake message and returns the next
// OPEN message (or nil once the handshake is complete).
type BlindStreamSession struct {
	binding       StreamBinding
	client        bool
	agentIdentity ed25519.PrivateKey
	peerIdentity  ed25519.PublicKey
	minimum       TrustMode
	clientHS      *BlindClientHandshake
	agentHS       *BlindAgentHandshake
	cipher        *StreamCipher
}

func NewBlindClientStream(binding StreamBinding, agentIdentity ed25519.PublicKey) (*BlindStreamSession, Envelope, error) {
	if len(agentIdentity) != ed25519.PublicKeySize {
		return nil, Envelope{}, errors.New("invalid agent identity")
	}
	hs, hello, err := NewBlindClientHandshake(binding, nil)
	if err != nil {
		return nil, Envelope{}, err
	}
	s := &BlindStreamSession{binding: binding, client: true, peerIdentity: append(ed25519.PublicKey(nil), agentIdentity...), clientHS: hs}
	return s, s.openEnvelope(blindOpenWire{Kind: "hello", Binding: binding, Hello: &hello}), nil
}

func NewBlindAgentStream(binding StreamBinding, agentIdentity ed25519.PrivateKey, minimum TrustMode) (*BlindStreamSession, error) {
	if len(agentIdentity) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid agent identity")
	}
	if err := minimum.Validate(); err != nil {
		return nil, err
	}
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	return &BlindStreamSession{binding: binding, agentIdentity: append(ed25519.PrivateKey(nil), agentIdentity...), minimum: minimum}, nil
}

func (s *BlindStreamSession) openEnvelope(w blindOpenWire) Envelope {
	payload, _ := json.Marshal(w)
	return Envelope{Header: Header{Version: Version, FrameType: FrameOpen, DeviceID: s.binding.DeviceID, SessionID: s.binding.SessionID, StreamID: s.binding.StreamID}, Payload: payload}
}

func (s *BlindStreamSession) HandleOpen(env Envelope) (Envelope, error) {
	if env.Header.FrameType != FrameOpen || env.Header.DeviceID != s.binding.DeviceID || env.Header.SessionID != s.binding.SessionID || env.Header.StreamID != s.binding.StreamID {
		return Envelope{}, errors.New("blind OPEN routing mismatch")
	}
	payload := env.Payload
	var presented blindCapabilityOpen
	if err := json.Unmarshal(payload, &presented); err == nil && len(presented.Blind) > 0 {
		payload = presented.Blind
	}
	var wire blindOpenWire
	if err := json.Unmarshal(payload, &wire); err != nil {
		return Envelope{}, fmt.Errorf("decode blind OPEN: %w", err)
	}
	if wire.Binding != s.binding {
		return Envelope{}, errors.New("blind OPEN binding mismatch")
	}
	if s.client {
		if wire.Kind != "response" || wire.Response == nil || s.clientHS == nil {
			return Envelope{}, errors.New("expected blind agent response")
		}
		cipher, finish, err := s.clientHS.Finish(*wire.Response, s.peerIdentity)
		if err != nil {
			return Envelope{}, err
		}
		s.cipher = cipher
		return s.openEnvelope(blindOpenWire{Kind: "finish", Binding: s.binding, Finish: &finish}), nil
	}
	if wire.Kind == "hello" && wire.Hello != nil {
		hs, response, err := AcceptBlindClientHello(s.binding, *wire.Hello, s.agentIdentity, s.minimum, nil)
		if err != nil {
			return Envelope{}, err
		}
		s.agentHS = hs
		return s.openEnvelope(blindOpenWire{Kind: "response", Binding: s.binding, Response: &response}), nil
	}
	if wire.Kind == "finish" && wire.Finish != nil && s.agentHS != nil {
		cipher, err := s.agentHS.Confirm(*wire.Finish)
		if err != nil {
			return Envelope{}, err
		}
		s.cipher = cipher
		return Envelope{}, nil
	}
	return Envelope{}, errors.New("unexpected blind OPEN message")
}

func (s *BlindStreamSession) Ready() bool { return s != nil && s.cipher != nil }

func (s *BlindStreamSession) SealData(env Envelope) (Envelope, error) {
	if env.Header.FrameType != FrameData && env.Header.FrameType != FrameSession {
		return Envelope{}, errors.New("blind data requires DATA frame")
	}
	if s.cipher == nil {
		return Envelope{}, errors.New("blind stream handshake is incomplete")
	}
	return s.cipher.SealEnvelope(env)
}

func (s *BlindStreamSession) OpenData(env Envelope) (Envelope, error) {
	if env.Header.FrameType != FrameData && env.Header.FrameType != FrameSession {
		return Envelope{}, errors.New("blind data requires DATA frame")
	}
	if s.cipher == nil {
		return Envelope{}, errors.New("blind stream handshake is incomplete")
	}
	return s.cipher.OpenEnvelope(env)
}

// NewRotation advances this endpoint to the next epoch. Callers should send
// the returned rotation (inside the encrypted stream) and have the peer call
// ApplyRotation before sending data in the new epoch.
func (s *BlindStreamSession) NewRotation(random io.Reader) (BlindKeyRotation, error) {
	if s.cipher == nil {
		return BlindKeyRotation{}, errors.New("blind stream handshake is incomplete")
	}
	rotation, next, err := s.cipher.NewRotation(random)
	if err != nil {
		return BlindKeyRotation{}, err
	}
	s.cipher = next
	return rotation, nil
}

func (s *BlindStreamSession) ApplyRotation(rotation BlindKeyRotation) error {
	if s.cipher == nil {
		return errors.New("blind stream handshake is incomplete")
	}
	next, err := s.cipher.ApplyRotation(rotation)
	if err != nil {
		return err
	}
	s.cipher = next
	return nil
}
