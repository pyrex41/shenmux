package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Store is a content-addressed directory. File contents live under
// STORE/blobs/<sha256> and upperdir/delta archives under
// STORE/archives/<sha256>.tar.gz. Writes are deduplicated by hash: putting the
// same bytes twice is a no-op.
type Store struct {
	Base string
}

// OpenStore returns a Store rooted at base, creating the blobs/ and archives/
// subdirectories if needed.
func OpenStore(base string) (*Store, error) {
	s := &Store{Base: base}
	for _, sub := range []string{"blobs", "archives"} {
		if err := os.MkdirAll(filepath.Join(base, sub), 0o755); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) blobPath(hash string) string {
	return filepath.Join(s.Base, "blobs", hash)
}

func (s *Store) archivePath(hash string) string {
	return filepath.Join(s.Base, "archives", hash+".tar.gz")
}

// PutBlob writes data into the store and returns its hex sha256 address.
// Deduplicated: if the blob already exists it is not rewritten.
func (s *Store) PutBlob(data []byte) error {
	sum := sha256.Sum256(data)
	_, err := s.putAt(s.blobPath(hex.EncodeToString(sum[:])), data)
	return err
}

// HashBytes is a small helper returning the hex sha256 of data.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// GetBlob reads a blob by its hex sha256 address.
func (s *Store) GetBlob(hash string) ([]byte, error) {
	return os.ReadFile(s.blobPath(hash))
}

// HasBlob reports whether a blob with the given address is present.
func (s *Store) HasBlob(hash string) bool {
	_, err := os.Stat(s.blobPath(hash))
	return err == nil
}

// PutArchive stores an archive (tar.gz bytes) and returns its hex sha256
// address. Deduplicated by content.
func (s *Store) PutArchive(data []byte) (string, error) {
	hash := HashBytes(data)
	if _, err := s.putAt(s.archivePath(hash), data); err != nil {
		return "", err
	}
	return hash, nil
}

// ArchivePath returns the on-disk path of an archive by address. It does not
// check existence.
func (s *Store) ArchivePath(hash string) string {
	return s.archivePath(hash)
}

// GetArchive reads an archive's bytes by address.
func (s *Store) GetArchive(hash string) ([]byte, error) {
	return os.ReadFile(s.archivePath(hash))
}

// putAt writes data to path only if not already present (content-addressed, so
// identical path implies identical content). Writes atomically via a temp file.
func (s *Store) putAt(path string, data []byte) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil // already present, dedup
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return false, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return false, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return false, fmt.Errorf("commit blob: %w", err)
	}
	return true, nil
}
