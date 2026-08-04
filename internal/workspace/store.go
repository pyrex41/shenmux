// Package workspace contains the experimental capability-scoped object
// service used by the browser WASI workspace. It deliberately speaks in
// storage-neutral operations so an implementation can be backed by a local
// directory, S3, or another object store without exposing its credentials.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	ErrNotFound    = errors.New("workspace object not found")
	ErrConflict    = errors.New("workspace object version conflict")
	ErrInvalidPath = errors.New("invalid workspace path")
)

type Entry struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Size int64  `json:"size"`
}

type ReadResult struct {
	Data  []byte
	ETag  string
	Size  int64
	Start int64
}

// Store is the minimum object/filesystem surface the HTTP handler needs.
// Implementations should make Write idempotent when operationID is reused.
type Store interface {
	List(path string) ([]Entry, error)
	Read(path string, offset, length int64) (ReadResult, error)
	Write(path string, data []byte, ifMatch, operationID string) (etag string, err error)
	Mkdir(path, operationID string) error
}

// LocalStore is a safe development backend and a useful single-node Fly or
// home-server deployment. All paths are rooted below Root and writes are
// replaced atomically.
type LocalStore struct {
	Root string

	mu          sync.Mutex
	idempotency map[string]idempotentResult
}

type idempotentResult struct {
	op   string
	path string
	etag string
}

func NewLocalStore(root string) *LocalStore {
	_ = os.MkdirAll(root, 0o755)
	return &LocalStore{Root: root, idempotency: make(map[string]idempotentResult)}
}

func (s *LocalStore) List(path string) ([]Entry, error) {
	filePath, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	result := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".shenmux-") {
			continue
		}
		kind := "file"
		size := int64(0)
		if entry.IsDir() {
			kind = "directory"
		} else if info, statErr := entry.Info(); statErr == nil {
			size = info.Size()
		}
		result = append(result, Entry{Name: entry.Name(), Kind: kind, Size: size})
	}
	return result, nil
}

func (s *LocalStore) Read(path string, offset, length int64) (ReadResult, error) {
	if offset < 0 || length < 0 {
		return ReadResult{}, ErrInvalidPath
	}
	filePath, err := s.resolve(path)
	if err != nil {
		return ReadResult{}, err
	}
	file, err := os.Open(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return ReadResult{}, ErrNotFound
	}
	if err != nil {
		return ReadResult{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ReadResult{}, err
	}
	if info.IsDir() {
		return ReadResult{}, ErrInvalidPath
	}
	if offset > info.Size() {
		offset = info.Size()
	}
	if length == 0 || offset+length > info.Size() {
		length = info.Size() - offset
	}
	data := make([]byte, length)
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return ReadResult{}, err
	}
	if _, err := io.ReadFull(file, data); err != nil && !errors.Is(err, io.EOF) {
		return ReadResult{}, err
	}
	all, err := os.ReadFile(filePath)
	if err != nil {
		return ReadResult{}, err
	}
	return ReadResult{Data: data, ETag: etag(all), Size: info.Size(), Start: offset}, nil
}

func (s *LocalStore) Write(path string, data []byte, ifMatch, operationID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filePath, err := s.resolve(path)
	if err != nil {
		return "", err
	}
	if operationID != "" {
		if previous, ok := s.idempotency[operationID]; ok {
			if previous.op != "write" || previous.path != path {
				return "", ErrConflict
			}
			return previous.etag, nil
		}
	}
	if ifMatch != "" {
		current, readErr := os.ReadFile(filePath)
		if errors.Is(readErr, os.ErrNotExist) || etag(current) != ifMatch {
			return "", ErrConflict
		}
		if readErr != nil {
			return "", readErr
		}
	}
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(filepath.Dir(filePath), ".shenmux-write-*")
	if err != nil {
		return "", err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return "", err
	}
	if err := temp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tempName, filePath); err != nil {
		return "", err
	}
	newETag := etag(data)
	if operationID != "" {
		s.idempotency[operationID] = idempotentResult{op: "write", path: path, etag: newETag}
	}
	return newETag, nil
}

func (s *LocalStore) Mkdir(path, operationID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if operationID != "" {
		if previous, ok := s.idempotency[operationID]; ok {
			if previous.op != "mkdir" || previous.path != path {
				return ErrConflict
			}
			return nil
		}
	}
	filePath, err := s.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filePath, 0o755); err != nil {
		return err
	}
	if operationID != "" {
		s.idempotency[operationID] = idempotentResult{op: "mkdir", path: path}
	}
	return nil
}

func (s *LocalStore) resolve(path string) (string, error) {
	clean, err := cleanPath(path)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(s.Root)
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(root, strings.TrimPrefix(clean, "/"))
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", ErrInvalidPath
	}
	return resolved, nil
}

func cleanPath(path string) (string, error) {
	if path == "" || !strings.HasPrefix(path, "/") || strings.ContainsRune(path, 0) {
		return "", ErrInvalidPath
	}
	parts := make([]string, 0)
	for _, part := range strings.Split(path, "/") {
		switch part {
		case "", ".":
		case "..":
			if len(parts) == 0 {
				return "", ErrInvalidPath
			}
			parts = parts[:len(parts)-1]
		default:
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "/", nil
	}
	return "/" + strings.Join(parts, "/"), nil
}

func etag(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}
