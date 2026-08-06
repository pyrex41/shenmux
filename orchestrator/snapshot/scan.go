package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Scan walks root recursively and returns a sorted slice of file entries plus
// the tree hash. Every regular file's content hash is the hex SHA-256 of its
// bytes. Mode is the raw POSIX st_mode, so it matches the integers autopoiesis
// records (e.g. 33188). If store is non-nil, each file's content is also
// written into the content-addressed store (dedup by hash).
func Scan(root string, store *Store) ([]Entry, string, error) {
	var entries []Entry
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil // directories, symlinks, sockets, etc. are not file entries
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		hexsum := hex.EncodeToString(sum[:])

		if store != nil {
			if err := store.PutBlob(data); err != nil {
				return err
			}
		}

		var mode, mtime int64
		size := int64(len(data))
		if st, ok := d.Info(); ok == nil {
			if raw, rok := st.Sys().(*syscall.Stat_t); rok {
				mode = int64(raw.Mode)
				mtime = raw.Mtim.Sec
			} else {
				mode = int64(st.Mode().Perm())
				mtime = st.ModTime().Unix()
			}
		}
		entries = append(entries, Entry{
			Path:  rel,
			Hash:  hexsum,
			Mode:  mode,
			Size:  size,
			Mtime: mtime,
		})
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	SortEntries(entries)
	return entries, TreeHash(entries), nil
}
