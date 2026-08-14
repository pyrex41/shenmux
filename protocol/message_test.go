package protocol

import (
	"bytes"
	"testing"
)

func TestMessageRoundTripKeepsBinaryPayload(t *testing.T) {
	original := Message{
		Kind:    KindDelta,
		Meta:    Meta{Version: Version, Session: "s", Seq: 7},
		Payload: []byte{0, 1, 2, 0xff, '\n'},
	}
	frames, err := EncodePublished("session/s", original)
	if err != nil {
		t.Fatal(err)
	}
	topic, got, err := DecodePublished(frames)
	if err != nil {
		t.Fatal(err)
	}
	if topic != "session/s" || got.Kind != original.Kind || got.Meta.Seq != 7 {
		t.Fatalf("unexpected decoded message: topic=%q msg=%+v", topic, got)
	}
	if !bytes.Equal(got.Payload, original.Payload) {
		t.Fatalf("payload changed: %v != %v", got.Payload, original.Payload)
	}
}

func TestDecodeRejectsWrongVersion(t *testing.T) {
	_, err := Decode([][]byte{[]byte(KindPing), []byte(`{"v":99}`)})
	if err == nil {
		t.Fatal("wrong version was accepted")
	}
}
