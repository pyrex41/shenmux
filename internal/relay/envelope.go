// Package relay contains the transport-neutral pieces of the outbound relay
// protocol.  The package deliberately does not open sockets: controllers and
// agents can use these codecs with any WebSocket implementation.
package relay

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/pyrex41/shenmux/protocol"
)

const (
	// Version is the outer relay envelope version.  The inner terminal protocol
	// remains protocol.Version (currently v2).
	Version uint16 = 1

	MaxHeaderSize   = 64 << 10
	MaxEnvelopeSize = 64 << 20
	MaxFrameCount   = 3
)

// Standard outer frame types. Unknown types are allowed by the codec so a
// controller can evolve its control plane without requiring an agent upgrade.
const (
	FrameHello     = "hello"
	FrameChallenge = "challenge"
	FrameProof     = "proof"
	FrameReady     = "ready"
	FrameResume    = "resume"
	FrameOpen      = "open"
	// FrameData carries one encrypted (blind mode) or trusted inner protocol
	// payload. FrameSession is retained as a wire-compatible alias for older
	// agents that used the initial vertical-slice name.
	FrameData    = "data"
	FrameClose   = "close"
	FrameSession = "session"
)

// Header identifies a relay stream and protects it from cross-stream replay.
// FrameType is intentionally opaque to this package; controllers may add
// control-plane frame types without changing terminal framing.
type Header struct {
	Version   uint16 `json:"v"`
	FrameType string `json:"type"`
	RequestID uint64 `json:"request_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	StreamID  string `json:"stream_id,omitempty"`
	Counter   uint64 `json:"counter"`
}

// Envelope is one binary WebSocket message.  The wire format is a four-byte
// big-endian header length, a JSON Header, and an inner payload.  Payload is
// usually MarshalFrames output, preserving the existing v2 multipart message
// byte-for-byte (including its kind, metadata, and optional binary payload).
type Envelope struct {
	Header  Header
	Payload []byte
}

func (e Envelope) Encode() ([]byte, error) {
	h := e.Header
	if h.Version == 0 {
		h.Version = Version
	}
	if h.Version != Version {
		return nil, fmt.Errorf("unsupported relay envelope version %d", h.Version)
	}
	if h.FrameType == "" {
		return nil, errors.New("relay frame type must not be empty")
	}
	encodedHeader, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("encode relay header: %w", err)
	}
	if len(encodedHeader) > MaxHeaderSize {
		return nil, fmt.Errorf("relay header exceeds %d bytes", MaxHeaderSize)
	}
	if len(e.Payload) > MaxEnvelopeSize-MaxHeaderSize-4 {
		return nil, fmt.Errorf("relay payload exceeds %d bytes", MaxEnvelopeSize-MaxHeaderSize-4)
	}
	out := make([]byte, 4+len(encodedHeader)+len(e.Payload))
	binary.BigEndian.PutUint32(out[:4], uint32(len(encodedHeader)))
	copy(out[4:], encodedHeader)
	copy(out[4+len(encodedHeader):], e.Payload)
	return out, nil
}

func Decode(data []byte) (Envelope, error) {
	if len(data) < 4 {
		return Envelope{}, errors.New("relay envelope is shorter than header length")
	}
	headerLen := int(binary.BigEndian.Uint32(data[:4]))
	if headerLen == 0 || headerLen > MaxHeaderSize {
		return Envelope{}, fmt.Errorf("relay header length %d outside 1..%d", headerLen, MaxHeaderSize)
	}
	if headerLen > len(data)-4 {
		return Envelope{}, errors.New("relay envelope has truncated header")
	}
	if len(data) > MaxEnvelopeSize {
		return Envelope{}, fmt.Errorf("relay envelope exceeds %d bytes", MaxEnvelopeSize)
	}
	var h Header
	if err := json.Unmarshal(data[4:4+headerLen], &h); err != nil {
		return Envelope{}, fmt.Errorf("decode relay header: %w", err)
	}
	if h.Version != Version {
		return Envelope{}, fmt.Errorf("unsupported relay envelope version %d", h.Version)
	}
	if h.FrameType == "" {
		return Envelope{}, errors.New("relay frame type must not be empty")
	}
	return Envelope{Header: h, Payload: append([]byte(nil), data[4+headerLen:]...)}, nil
}

// MarshalFrames serializes the existing multipart protocol frames into the
// envelope payload.  Length prefixes preserve frame boundaries exactly.
func MarshalFrames(frames [][]byte) ([]byte, error) {
	if len(frames) < 2 || len(frames) > MaxFrameCount {
		return nil, fmt.Errorf("inner message must contain 2 or 3 frames, got %d", len(frames))
	}
	var size int = 4
	for _, frame := range frames {
		if uint64(len(frame)) > uint64(^uint32(0)) {
			return nil, errors.New("inner frame exceeds uint32 length")
		}
		size += 4 + len(frame)
		if size > MaxEnvelopeSize {
			return nil, fmt.Errorf("inner frames exceed %d bytes", MaxEnvelopeSize)
		}
	}
	out := make([]byte, size)
	binary.BigEndian.PutUint32(out[:4], uint32(len(frames)))
	off := 4
	for _, frame := range frames {
		binary.BigEndian.PutUint32(out[off:off+4], uint32(len(frame)))
		off += 4
		copy(out[off:], frame)
		off += len(frame)
	}
	return out, nil
}

func UnmarshalFrames(data []byte) ([][]byte, error) {
	if len(data) < 4 {
		return nil, errors.New("inner frames are shorter than frame count")
	}
	count := int(binary.BigEndian.Uint32(data[:4]))
	if count < 2 || count > MaxFrameCount {
		return nil, fmt.Errorf("inner message must contain 2 or 3 frames, got %d", count)
	}
	frames := make([][]byte, count)
	off := 4
	for i := range frames {
		if len(data)-off < 4 {
			return nil, errors.New("inner frames have truncated length")
		}
		length := int(binary.BigEndian.Uint32(data[off : off+4]))
		off += 4
		if length > len(data)-off {
			return nil, errors.New("inner frames have truncated payload")
		}
		frames[i] = append([]byte(nil), data[off:off+length]...)
		off += length
	}
	if off != len(data) {
		return nil, errors.New("inner frames have trailing bytes")
	}
	return frames, nil
}

func EncodeMessage(header Header, msg protocol.Message) ([]byte, error) {
	frames, err := protocol.Encode(msg)
	if err != nil {
		return nil, err
	}
	payload, err := MarshalFrames(frames)
	if err != nil {
		return nil, err
	}
	return (Envelope{Header: header, Payload: payload}).Encode()
}

func DecodeMessage(data []byte) (Envelope, protocol.Message, error) {
	env, err := Decode(data)
	if err != nil {
		return Envelope{}, protocol.Message{}, err
	}
	frames, err := UnmarshalFrames(env.Payload)
	if err != nil {
		return Envelope{}, protocol.Message{}, err
	}
	msg, err := protocol.Decode(frames)
	if err != nil {
		return Envelope{}, protocol.Message{}, err
	}
	return env, msg, nil
}

// EqualPayload is useful to callers that need to assert that an inner frame
// was relayed without translation.
func EqualPayload(a, b []byte) bool { return bytes.Equal(a, b) }

// CounterTracker enforces strictly increasing outer transport counters for a
// stream. It is intentionally independent of the inner checkpoint sequence;
// reconnecting streams should use a fresh tracker.
type CounterTracker struct {
	mu   sync.Mutex
	last uint64
	seen bool
}

func (t *CounterTracker) Accept(counter uint64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen && counter <= t.last {
		return fmt.Errorf("relay counter %d is not greater than %d", counter, t.last)
	}
	t.last, t.seen = counter, true
	return nil
}

func (t *CounterTracker) Last() (uint64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last, t.seen
}
