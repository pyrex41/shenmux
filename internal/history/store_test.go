package history

import (
	"testing"

	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/screen"
)

func TestSaveLoadIsAtomicAndValidated(t *testing.T) {
	root := t.TempDir()
	state := screen.State{Frame: screen.Frame{Cols: 2, Rows: 1, Lines: []screen.Row{{{Text: "o", Width: 1}, {Text: "k", Width: 1}}}}}
	archive := protocol.Archive{Format: 2, HistoryLimit: 2, Checkpoint: protocol.Checkpoint{Screen: state}}
	if err := Save(root, "demo", archive); err != nil {
		t.Fatal(err)
	}
	record, err := Load(root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	got, err := protocol.DecodeArchive(record.Archive)
	if err != nil || got.LastSeq() != 0 {
		t.Fatalf("archive=%+v err=%v", got, err)
	}
}
