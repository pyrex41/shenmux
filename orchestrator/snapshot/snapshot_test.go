package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// handTreeHash recomputes the tree hash independently of TreeHash, replicating
// the autopoiesis canonical format "F:path:hash:mode:size" concatenated over
// path-sorted entries. This is the "hand-computed" reference the format is
// proven against.
func handTreeHash(entries []Entry) string {
	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	SortEntries(sorted)
	h := sha256.New()
	for _, e := range sorted {
		s := "F:" + e.Path + ":" + e.Hash + ":" +
			itoa(e.Mode) + ":" + itoa(e.Size)
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestTreeHashCanonicalAndOrderIndependent(t *testing.T) {
	// A tiny fixture with fixed hashes/modes/sizes proves the exact canonical
	// format, independent of any filesystem.
	a := Entry{Path: "a.txt", Hash: "aaaa", Mode: 33188, Size: 3, Mtime: 111}
	b := Entry{Path: "sub/b.txt", Hash: "bbbb", Mode: 33188, Size: 5, Mtime: 222}

	want := handTreeHash([]Entry{a, b})
	if got := TreeHash([]Entry{a, b}); got != want {
		t.Fatalf("TreeHash mismatch:\n got %s\nwant %s", got, want)
	}

	// Order-independence: TreeHash sorts internally, so a reversed input yields
	// the same root.
	if got := TreeHash([]Entry{b, a}); got != want {
		t.Fatalf("TreeHash not order-independent:\n got %s\nwant %s", got, want)
	}

	// mtime must NOT affect the hash.
	a2 := a
	a2.Mtime = 999999
	b2 := b
	b2.Mtime = 888888
	if got := TreeHash([]Entry{a2, b2}); got != want {
		t.Fatalf("mtime affected TreeHash: got %s want %s", got, want)
	}
}

func TestScanMatchesHandHash(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "hello")
	mustWrite(t, filepath.Join(dir, "sub", "b.txt"), "world!!")
	// Fixed modes so the raw st_mode is deterministic (33188 == 0100644).
	os.Chmod(filepath.Join(dir, "a.txt"), 0o644)
	os.Chmod(filepath.Join(dir, "sub", "b.txt"), 0o644)

	entries, root, err := Scan(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	// Verify raw POSIX mode is captured (byte-compatible with autopoiesis).
	for _, e := range entries {
		var st syscall.Stat_t
		if err := syscall.Stat(filepath.Join(dir, e.Path), &st); err != nil {
			t.Fatal(err)
		}
		if e.Mode != int64(st.Mode) {
			t.Fatalf("mode mismatch for %s: entry=%d stat=%d", e.Path, e.Mode, st.Mode)
		}
		if e.Mode != 33188 {
			t.Fatalf("expected mode 33188 for %s, got %d", e.Path, e.Mode)
		}
	}
	if root != handTreeHash(entries) {
		t.Fatalf("scan tree-root %s != hand hash %s", root, handTreeHash(entries))
	}
	if root != TreeHash(entries) {
		t.Fatalf("scan tree-root %s != TreeHash %s", root, TreeHash(entries))
	}

	// Content hash sanity.
	sum := sha256.Sum256([]byte("hello"))
	for _, e := range entries {
		if e.Path == "a.txt" && e.Hash != hex.EncodeToString(sum[:]) {
			t.Fatalf("a.txt content hash wrong: %s", e.Hash)
		}
	}
}

func TestAgentStateHashFixed(t *testing.T) {
	const want = "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d"
	if got := AgentStateHash(); got != want {
		t.Fatalf("AgentStateHash = %s, want %s", got, want)
	}
}

func TestCheckpointRestoreRoundTrip(t *testing.T) {
	ws := t.TempDir()
	store := t.TempDir()
	mustWrite(t, filepath.Join(ws, "notes.md"), "line one\n")
	mustWrite(t, filepath.Join(ws, "progress.txt"), "1")
	mustWrite(t, filepath.Join(ws, "nested", "deep.txt"), "deep content")

	manifest := filepath.Join(t.TempDir(), "cp.sexpr")
	id, err := Checkpoint(ws, store, manifest, "")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("empty checkpoint id")
	}

	into := t.TempDir()
	if err := Restore(manifest, store, into); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"notes.md", "progress.txt", "nested/deep.txt"} {
		orig := readFile(t, filepath.Join(ws, f))
		got := readFile(t, filepath.Join(into, filepath.FromSlash(f)))
		if orig != got {
			t.Fatalf("round-trip mismatch for %s: %q != %q", f, orig, got)
		}
	}
}

func TestForkNewIDParentIdenticalTree(t *testing.T) {
	ws := t.TempDir()
	store := t.TempDir()
	mustWrite(t, filepath.Join(ws, "a.txt"), "alpha")
	mustWrite(t, filepath.Join(ws, "b.txt"), "beta")

	mdir := t.TempDir()
	srcManifest := filepath.Join(mdir, "src.sexpr")
	srcID, err := Checkpoint(ws, store, srcManifest, "")
	if err != nil {
		t.Fatal(err)
	}

	into := t.TempDir()
	newPath, err := Fork(srcManifest, store, into)
	if err != nil {
		t.Fatal(err)
	}
	forked, err := ReadManifest(newPath)
	if err != nil {
		t.Fatal(err)
	}
	src, err := ReadManifest(srcManifest)
	if err != nil {
		t.Fatal(err)
	}
	if forked.ID == srcID {
		t.Fatal("fork produced same id as source")
	}
	if forked.Parent != srcID {
		t.Fatalf("fork parent = %q, want %q", forked.Parent, srcID)
	}
	if forked.TreeRoot != src.TreeRoot {
		t.Fatalf("fork tree-root %s != source %s", forked.TreeRoot, src.TreeRoot)
	}
	if len(forked.TreeEntries) != len(src.TreeEntries) {
		t.Fatalf("fork tree entry count differs")
	}
	// Restored files match the original workspace.
	if readFile(t, filepath.Join(into, "a.txt")) != "alpha" {
		t.Fatal("forked a.txt content wrong")
	}
	// Diff of identical trees is empty.
	if changes := DiffEntries(src.TreeEntries, forked.TreeEntries); len(changes) != 0 {
		t.Fatalf("expected no diff between source and fork, got %v", changes)
	}
}

func TestDiffDetectsAddedAndModified(t *testing.T) {
	ws := t.TempDir()
	store := t.TempDir()
	mustWrite(t, filepath.Join(ws, "keep.txt"), "same")
	mustWrite(t, filepath.Join(ws, "change.txt"), "before")

	mdir := t.TempDir()
	mA := filepath.Join(mdir, "a.sexpr")
	if _, err := Checkpoint(ws, store, mA, ""); err != nil {
		t.Fatal(err)
	}

	// Modify one file, add another.
	mustWrite(t, filepath.Join(ws, "change.txt"), "after-longer")
	mustWrite(t, filepath.Join(ws, "added.txt"), "brand new")
	mB := filepath.Join(mdir, "b.sexpr")
	if _, err := Checkpoint(ws, store, mB, ""); err != nil {
		t.Fatal(err)
	}

	changes, err := Diff(mA, mB)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, c := range changes {
		kinds[c.Path] = c.Kind
	}
	if kinds["added.txt"] != "added" {
		t.Fatalf("expected added.txt added, got %v", changes)
	}
	if kinds["change.txt"] != "modified" {
		t.Fatalf("expected change.txt modified, got %v", changes)
	}
	if _, ok := kinds["keep.txt"]; ok {
		t.Fatalf("keep.txt should be unchanged, got %v", changes)
	}
}

func TestManifestRoundTripEncoding(t *testing.T) {
	entries := []Entry{
		{Path: "a.txt", Hash: "deadbeef", Mode: 33188, Size: 5, Mtime: 100},
		{Path: "sub/b.txt", Hash: "cafef00d", Mode: 33188, Size: 7, Mtime: 200},
	}
	m := NewManifest("11111111-2222-4333-8444-555555555555", 1700000000, "parent-id",
		TreeHash(entries), entries, "archivehash123")
	parsed, err := ParseManifest([]byte(m.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ID != m.ID || parsed.Parent != m.Parent ||
		parsed.TreeRoot != m.TreeRoot || parsed.UpperdirArchive != m.UpperdirArchive {
		t.Fatalf("manifest round-trip mismatch: %+v", parsed)
	}
	if parsed.Hash != AgentStateHash() {
		t.Fatalf("agent-state hash not preserved: %s", parsed.Hash)
	}
	if len(parsed.TreeEntries) != 2 || parsed.TreeEntries[1].Path != "sub/b.txt" ||
		parsed.TreeEntries[1].Size != 7 {
		t.Fatalf("tree entries not parsed: %+v", parsed.TreeEntries)
	}
	// nil parent encodes/parses as empty.
	m.Parent = ""
	p2, err := ParseManifest([]byte(m.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	if p2.Parent != "" {
		t.Fatalf("nil parent should parse empty, got %q", p2.Parent)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
