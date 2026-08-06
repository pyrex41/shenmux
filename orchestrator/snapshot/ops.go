package snapshot

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

// Checkpoint captures workspace into the store and writes a manifest to
// manifestPath. It tar.gz's the whole workspace as the delta archive (the demo
// path, since a live overlay upperdir may be unavailable), hashes the file
// tree, and records a new UUID. parent may be "" for a root checkpoint. It
// returns the new snapshot id.
func Checkpoint(workspace, storeBase, manifestPath, parent string) (string, error) {
	store, err := OpenStore(storeBase)
	if err != nil {
		return "", err
	}
	archive, err := TarGzDir(workspace)
	if err != nil {
		return "", fmt.Errorf("archive workspace: %w", err)
	}
	archiveHash, err := store.PutArchive(archive)
	if err != nil {
		return "", fmt.Errorf("store archive: %w", err)
	}
	entries, treeRoot, err := Scan(workspace, store)
	if err != nil {
		return "", fmt.Errorf("scan workspace: %w", err)
	}
	id, err := NewUUID()
	if err != nil {
		return "", err
	}
	m := NewManifest(id, time.Now().Unix(), parent, treeRoot, entries, archiveHash)
	if err := WriteManifest(manifestPath, m); err != nil {
		return "", err
	}
	return id, nil
}

// Restore extracts the manifest's upperdir archive into the directory `into`.
func Restore(manifestPath, storeBase, into string) error {
	store, err := OpenStore(storeBase)
	if err != nil {
		return err
	}
	m, err := ReadManifest(manifestPath)
	if err != nil {
		return err
	}
	if m.UpperdirArchive == "" {
		return fmt.Errorf("manifest has no upperdir-archive to restore")
	}
	data, err := store.GetArchive(m.UpperdirArchive)
	if err != nil {
		return fmt.Errorf("read archive %s: %w", m.UpperdirArchive, err)
	}
	return UntarGz(data, into)
}

// Fork restores the source manifest into `into` under a fresh UUID whose parent
// is the source id, writes the new manifest next to the source manifest, and
// returns the new manifest path. The forked tree is identical to the source
// (same tree-root, entries and archive, deduped in the store).
func Fork(manifestPath, storeBase, into string) (string, error) {
	store, err := OpenStore(storeBase)
	if err != nil {
		return "", err
	}
	src, err := ReadManifest(manifestPath)
	if err != nil {
		return "", err
	}
	if src.UpperdirArchive == "" {
		return "", fmt.Errorf("source manifest has no upperdir-archive")
	}
	data, err := store.GetArchive(src.UpperdirArchive)
	if err != nil {
		return "", fmt.Errorf("read archive %s: %w", src.UpperdirArchive, err)
	}
	if err := UntarGz(data, into); err != nil {
		return "", err
	}
	id, err := NewUUID()
	if err != nil {
		return "", err
	}
	forked := NewManifest(id, time.Now().Unix(), src.ID, src.TreeRoot, src.TreeEntries, src.UpperdirArchive)
	newPath := filepath.Join(filepath.Dir(manifestPath), id+".sexpr")
	if err := WriteManifest(newPath, forked); err != nil {
		return "", err
	}
	return newPath, nil
}

// Change is one entry in a tree diff.
type Change struct {
	Kind string // "added", "removed", "modified"
	Path string
	Old  *Entry // nil for added
	New  *Entry // nil for removed
}

// Diff compares two manifests' tree-entries by path. A path present only in b
// is added, only in a is removed, and present in both with a differing
// hash/size/mode is modified (autopoiesis tree-diff semantics). Results are
// sorted by path.
func Diff(manifestA, manifestB string) ([]Change, error) {
	a, err := ReadManifest(manifestA)
	if err != nil {
		return nil, err
	}
	b, err := ReadManifest(manifestB)
	if err != nil {
		return nil, err
	}
	return DiffEntries(a.TreeEntries, b.TreeEntries), nil
}

// DiffEntries computes the tree diff between two entry slices.
func DiffEntries(aEntries, bEntries []Entry) []Change {
	aMap := map[string]Entry{}
	bMap := map[string]Entry{}
	for _, e := range aEntries {
		aMap[e.Path] = e
	}
	for _, e := range bEntries {
		bMap[e.Path] = e
	}
	var changes []Change
	for path, oe := range aMap {
		ne, ok := bMap[path]
		if !ok {
			old := oe
			changes = append(changes, Change{Kind: "removed", Path: path, Old: &old})
			continue
		}
		if oe.Hash != ne.Hash || oe.Size != ne.Size || oe.Mode != ne.Mode {
			old, nw := oe, ne
			changes = append(changes, Change{Kind: "modified", Path: path, Old: &old, New: &nw})
		}
	}
	for path, ne := range bMap {
		if _, ok := aMap[path]; !ok {
			nw := ne
			changes = append(changes, Change{Kind: "added", Path: path, New: &nw})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}
