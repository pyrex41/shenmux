package history

import (
	"os"
	"path/filepath"
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

func TestListAndRejectPathTraversal(t *testing.T) {
	root := t.TempDir()
	archive := protocol.Archive{Format: 2, HistoryLimit: 1, Checkpoint: protocol.Checkpoint{Screen: screen.State{Frame: screen.Frame{Cols: 1, Rows: 1, Lines: []screen.Row{{{Text: "x", Width: 1}}}}}}}
	if err := Save(root, "b", archive); err != nil {
		t.Fatal(err)
	}
	if err := Save(root, "a", archive); err != nil {
		t.Fatal(err)
	}
	got, err := List(root)
	if err != nil || len(got) != 2 || got[0].Session != "a" || got[1].Session != "b" {
		t.Fatalf("list=%v err=%v", got, err)
	}
	if _, err := Load(root, "../a"); err == nil {
		t.Fatal("expected traversal rejection")
	}
}

// A file shenmux never wrote must not take the whole listing down with it,
// while a record it did write and can no longer read still must.
func TestListSkipsForeignFilesButReportsDamagedRecords(t *testing.T) {
	root := t.TempDir()
	archive := protocol.Archive{Format: 2, HistoryLimit: 1, Checkpoint: protocol.Checkpoint{Screen: screen.State{Frame: screen.Frame{Cols: 1, Rows: 1, Lines: []screen.Row{{{Text: "x", Width: 1}}}}}}}
	if err := Save(root, "work", archive); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.backup.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := List(root)
	if err != nil {
		t.Fatalf("a foreign file must not fail the listing: %v", err)
	}
	if len(got) != 1 || got[0].Session != "work" {
		t.Fatalf("list=%v, want only the real session", got)
	}

	// A record we did write, now unreadable, is real history loss and must surface.
	if err := os.WriteFile(filepath.Join(root, "work.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := List(root); err == nil {
		t.Fatal("a damaged record of ours must fail the listing rather than vanish")
	}
}
