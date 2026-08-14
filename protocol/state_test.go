package protocol

import (
	"bytes"
	"testing"

	"github.com/pyrex41/shenmux/screen"
)

func testState(t *testing.T, text string) screen.State {
	t.Helper()
	frame, err := screen.BlankFrame(8, 2)
	if err != nil {
		t.Fatal(err)
	}
	for x, r := range []rune(text) {
		frame.Lines[0][x] = screen.Cell{Text: string(r), Width: 1}
	}
	return screen.State{Frame: frame}
}

func TestArchiveRoundTripUsesCanonicalState(t *testing.T) {
	initial := testState(t, "hello")
	nextFrame := initial.Frame.Clone()
	nextFrame.Lines[1][0] = screen.Cell{Text: "x", Width: 1}
	next, delta, err := screen.Advance(initial, nextFrame, 20)
	if err != nil {
		t.Fatal(err)
	}
	archive := Archive{
		Format: stateFormatVersion, HistoryLimit: 20,
		Checkpoint: Checkpoint{Seq: 4, Screen: initial, ControlOwner: "client-a"},
		Tail: []Event{
			{Kind: EventDelta, Seq: 5, Delta: &delta},
			{Kind: EventControl, Seq: 6, ControlOwner: "client-b"},
			{Kind: EventExit, Seq: 7, ExitCode: 9},
		},
	}
	payload, err := EncodeArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte("\x1b]52")) {
		t.Fatal("archive contains terminal control stream")
	}
	got, err := DecodeArchive(payload)
	if err != nil {
		t.Fatal(err)
	}
	current, err := got.Current()
	if err != nil {
		t.Fatal(err)
	}
	if current.Seq != 7 || current.ControlOwner != "client-b" || !current.Exited || current.ExitCode != 9 {
		t.Fatalf("unexpected current checkpoint: %+v", current)
	}
	if !screen.EqualState(current.Screen, next) {
		t.Fatal("screen state did not round-trip")
	}
}

func TestStoreCompactsBoundedTail(t *testing.T) {
	limits := StoreLimits{HistoryRows: 10, MaxTailEvents: 2, MaxTailBytes: 1 << 20}
	initial := Checkpoint{Screen: testState(t, "zero")}
	store, err := NewStore(initial, limits)
	if err != nil {
		t.Fatal(err)
	}
	current := initial
	for seq := uint64(1); seq <= 3; seq++ {
		current.Seq = seq
		current.ControlOwner = "owner"
		if err := store.Append(Event{Kind: EventControl, Seq: seq, ControlOwner: "owner"}, current); err != nil {
			t.Fatal(err)
		}
	}
	stats := store.Stats()
	if stats.Compactions != 1 || stats.CheckpointSeq != 3 || stats.TailEvents != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	archive := store.Snapshot()
	got, err := archive.Current()
	if err != nil {
		t.Fatal(err)
	}
	if got.Seq != 3 || got.ControlOwner != "owner" {
		t.Fatalf("unexpected compacted state: %+v", got)
	}
}

func TestDecodeDeltaRejectsTrailingJSON(t *testing.T) {
	state := testState(t, "x")
	_, delta, err := screen.Advance(screen.State{}, state.Frame, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := EncodeDelta(delta)
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, []byte("{}")...)
	if _, err := DecodeDelta(payload); err == nil {
		t.Fatal("trailing JSON accepted")
	}
}
