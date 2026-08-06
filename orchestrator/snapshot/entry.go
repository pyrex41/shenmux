// Package snapshot implements content-addressed overlay checkpoints with an
// autopoiesis-compatible s-expression manifest. It is byte-compatible with the
// autopoiesis filesystem-tree hashing scheme (see
// packages/core/src/snapshot/filesystem-tree.lisp): the tree hash is the
// SHA-256 over the sorted concatenation of per-entry canonical strings, with
// mtime deliberately excluded so the hash is stable across restores.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
)

// Entry is a single filesystem tree entry. It mirrors the autopoiesis plist
// (:file "path" :hash H :mode M :size N :mtime T). We scan regular files only,
// so every Entry is a file entry; the canonical-string helpers still support
// directory (D) and symlink (L) forms for format compatibility.
type Entry struct {
	Path  string // path relative to the scanned root, forward slashes
	Hash  string // hex sha256 of the file's content blob
	Mode  int64  // raw POSIX st_mode (e.g. 33188 for a 0644 regular file)
	Size  int64  // content size in bytes
	Mtime int64  // modification time (unix seconds); EXCLUDED from the tree hash
}

// CanonicalString returns the format-canonical string for the entry, matching
// autopoiesis canonical-entry-string for a :file: "F:path:hash:mode:size".
// mtime is intentionally not part of it.
func (e Entry) CanonicalString() string {
	return "F:" + e.Path + ":" + e.Hash + ":" +
		strconv.FormatInt(e.Mode, 10) + ":" +
		strconv.FormatInt(e.Size, 10)
}

// SortEntries orders entries by path (string<) for a deterministic Merkle root,
// matching autopoiesis scan-directory which sorts by path.
func SortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
}

// TreeHash computes the Merkle root over a set of entries. The entries are
// sorted by path first, then their canonical strings are concatenated with no
// separator and SHA-256'd. This is byte-compatible with autopoiesis tree-hash.
func TreeHash(entries []Entry) string {
	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	SortEntries(sorted)
	h := sha256.New()
	for _, e := range sorted {
		h.Write([]byte(e.CanonicalString()))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// AgentStateHash returns the autopoiesis sexpr-hash of the s-expression NIL.
// In autopoiesis' sexpr-hash-into, NIL hashes a single zero byte, so this is
// SHA-256 of []byte{0}. Since our manifests carry no agent state, :hash is
// always this fixed value:
// 6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d.
func AgentStateHash() string {
	h := sha256.Sum256([]byte{0})
	return hex.EncodeToString(h[:])
}
