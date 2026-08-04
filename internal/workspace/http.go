package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Authorize receives the opaque bearer capability, an operation name, and a
// normalized path. It should enforce tenant/session/path scope and can reject
// reads and writes independently. The token is never passed to Store.
type Authorize func(capability, operation, path string) bool

type HTTPConfig struct {
	Store     Store
	Authorize Authorize
	MaxWrite  int64
}

func Handler(cfg HTTPConfig) http.Handler {
	maxWrite := cfg.MaxWrite
	if maxWrite <= 0 {
		maxWrite = 64 << 20
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, err := cleanPath(r.URL.Query().Get("path"))
		if err != nil {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		operation := operationFor(r)
		capability, ok := bearer(r.Header.Get("Authorization"))
		if !ok || cfg.Authorize == nil || !cfg.Authorize(capability, operation, path) {
			http.Error(w, "capability required", http.StatusUnauthorized)
			return
		}
		if cfg.Store == nil {
			http.Error(w, "workspace backend unavailable", http.StatusServiceUnavailable)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/manifest":
			entries, err := cfg.Store.List(path)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"path": path, "entries": entries})
		case r.Method == http.MethodGet && r.URL.Path == "/objects":
			offset, length, partial, err := rangeRequest(r.Header.Get("Range"))
			if err != nil {
				http.Error(w, "invalid range", http.StatusBadRequest)
				return
			}
			result, err := cfg.Store.Read(path, offset, length)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			w.Header().Set("ETag", result.ETag)
			w.Header().Set("Accept-Ranges", "bytes")
			if etagMatches(r.Header.Get("If-None-Match"), result.ETag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(result.Data)))
			if partial {
				if len(result.Data) > 0 {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", result.Start, result.Start+int64(len(result.Data))-1, result.Size))
				}
				w.WriteHeader(http.StatusPartialContent)
			}
			_, _ = w.Write(result.Data)
		case r.Method == http.MethodPut && r.URL.Path == "/objects":
			if r.ContentLength > maxWrite {
				http.Error(w, "object too large", http.StatusRequestEntityTooLarge)
				return
			}
			data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWrite))
			if err != nil {
				http.Error(w, "object too large", http.StatusRequestEntityTooLarge)
				return
			}
			etag, err := cfg.Store.Write(path, data, r.Header.Get("If-Match"), r.Header.Get("Idempotency-Key"))
			if err != nil {
				writeStoreError(w, err)
				return
			}
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/directories":
			if err := cfg.Store.Mkdir(path, r.Header.Get("Idempotency-Key")); err != nil {
				writeStoreError(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
}

func etagMatches(value, current string) bool {
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == current || strings.TrimPrefix(candidate, "W/") == current {
			return candidate != ""
		}
	}
	return false
}

func operationFor(r *http.Request) string {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/manifest":
		return "list"
	case r.Method == http.MethodGet && r.URL.Path == "/objects":
		return "read"
	case r.Method == http.MethodPut && r.URL.Path == "/objects":
		return "write"
	case r.Method == http.MethodPost && r.URL.Path == "/directories":
		return "mkdir"
	default:
		return "unknown"
	}
}

func bearer(value string) (string, bool) {
	parts := strings.Fields(value)
	return func() (string, bool) {
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") || parts[1] == "" {
			return "", false
		}
		return parts[1], true
	}()
}

func rangeRequest(value string) (offset, length int64, partial bool, err error) {
	if value == "" {
		return 0, 0, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") {
		return 0, 0, false, errors.New("range")
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 || parts[0] == "" {
		return 0, 0, false, errors.New("range")
	}
	offset, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil || offset < 0 {
		return 0, 0, false, errors.New("range")
	}
	if parts[1] == "" {
		return offset, 0, true, nil
	}
	end, parseErr := strconv.ParseInt(parts[1], 10, 64)
	if parseErr != nil || end < offset {
		return 0, 0, false, errors.New("range")
	}
	return offset, end - offset + 1, true, nil
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrConflict):
		http.Error(w, "conflict", http.StatusConflict)
	case errors.Is(err, ErrInvalidPath):
		http.Error(w, "invalid path", http.StatusBadRequest)
	default:
		http.Error(w, "workspace backend error", http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
