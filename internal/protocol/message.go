package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	Version         = 2
	MaxMetadataSize = 64 << 10
	MaxPayloadSize  = 64 << 20
)

type Kind string

const (
	KindAttach         Kind = "attach"
	KindAttached       Kind = "attached"
	KindInput          Kind = "input"
	KindResize         Kind = "resize"
	KindDetach         Kind = "detach"
	KindPing           Kind = "ping"
	KindPong           Kind = "pong"
	KindDelta          Kind = "screen-delta"
	KindControl        Kind = "control-owner"
	KindAcquireControl Kind = "acquire-control"
	KindReleaseControl Kind = "release-control"
	KindExit           Kind = "exit"
	KindError          Kind = "error"
	KindResync         Kind = "resync"
)

var validKinds = map[Kind]struct{}{
	KindAttach: {}, KindAttached: {}, KindInput: {}, KindResize: {},
	KindDetach: {}, KindPing: {}, KindPong: {}, KindDelta: {}, KindControl: {},
	KindAcquireControl: {}, KindReleaseControl: {}, KindExit: {},
	KindError: {}, KindResync: {},
}

// Meta is the small JSON envelope. Canonical screen checkpoints and deltas stay
// in a separate frame; application-provided terminal control strings never
// cross the data plane.
type Meta struct {
	Version       uint16 `json:"v"`
	Session       string `json:"session,omitempty"`
	ClientID      string `json:"client_id,omitempty"`
	RequestID     uint64 `json:"request_id,omitempty"`
	Seq           uint64 `json:"seq,omitempty"`
	CheckpointSeq uint64 `json:"checkpoint_seq,omitempty"`
	Cols          uint16 `json:"cols,omitempty"`
	Rows          uint16 `json:"rows,omitempty"`
	ExitCode      int    `json:"exit_code,omitempty"`
	ControlOwner  string `json:"control_owner,omitempty"`
	HasControl    bool   `json:"has_control,omitempty"`
	Error         string `json:"error,omitempty"`
}

type Message struct {
	Kind    Kind
	Meta    Meta
	Payload []byte
}

func Encode(msg Message) ([][]byte, error) {
	if _, ok := validKinds[msg.Kind]; !ok {
		return nil, fmt.Errorf("unknown message kind %q", msg.Kind)
	}
	if len(msg.Payload) > MaxPayloadSize {
		return nil, fmt.Errorf("payload exceeds %d bytes", MaxPayloadSize)
	}
	meta := msg.Meta
	if meta.Version == 0 {
		meta.Version = Version
	}
	if meta.Version != Version {
		return nil, fmt.Errorf("unsupported protocol version %d", meta.Version)
	}
	encodedMeta, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("encode metadata: %w", err)
	}
	if len(encodedMeta) > MaxMetadataSize {
		return nil, fmt.Errorf("metadata exceeds %d bytes", MaxMetadataSize)
	}
	frames := [][]byte{[]byte(msg.Kind), encodedMeta}
	if msg.Payload != nil {
		frames = append(frames, append([]byte(nil), msg.Payload...))
	}
	return frames, nil
}

func Decode(frames [][]byte) (Message, error) {
	if len(frames) < 2 || len(frames) > 3 {
		return Message{}, fmt.Errorf("message must contain 2 or 3 frames, got %d", len(frames))
	}
	kind := Kind(string(frames[0]))
	if _, ok := validKinds[kind]; !ok {
		return Message{}, fmt.Errorf("unknown message kind %q", kind)
	}
	if len(frames[1]) > MaxMetadataSize {
		return Message{}, fmt.Errorf("metadata exceeds %d bytes", MaxMetadataSize)
	}
	var meta Meta
	if err := json.Unmarshal(frames[1], &meta); err != nil {
		return Message{}, fmt.Errorf("decode metadata: %w", err)
	}
	if meta.Version != Version {
		return Message{}, fmt.Errorf("unsupported protocol version %d", meta.Version)
	}
	var payload []byte
	if len(frames) == 3 {
		if len(frames[2]) > MaxPayloadSize {
			return Message{}, fmt.Errorf("payload exceeds %d bytes", MaxPayloadSize)
		}
		payload = append([]byte(nil), frames[2]...)
	}
	return Message{Kind: kind, Meta: meta, Payload: payload}, nil
}

func EncodePublished(topic string, msg Message) ([][]byte, error) {
	if topic == "" {
		return nil, errors.New("publish topic must not be empty")
	}
	frames, err := Encode(msg)
	if err != nil {
		return nil, err
	}
	return append([][]byte{[]byte(topic)}, frames...), nil
}

func DecodePublished(frames [][]byte) (string, Message, error) {
	if len(frames) < 3 {
		return "", Message{}, fmt.Errorf("published message must contain at least 3 frames, got %d", len(frames))
	}
	topic := string(frames[0])
	if topic == "" {
		return "", Message{}, errors.New("empty publish topic")
	}
	msg, err := Decode(frames[1:])
	if err != nil {
		return "", Message{}, err
	}
	return topic, msg, nil
}
