package workspace

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalStoreAndCapabilityHTTP(t *testing.T) {
	root := t.TempDir()
	store := NewLocalStore(root)
	if err := store.Mkdir("/notes", "mkdir-1"); err != nil {
		t.Fatal(err)
	}
	handler := Handler(HTTPConfig{
		Store: store,
		Authorize: func(capability, operation, path string) bool {
			return capability == "cap-test" && strings.HasPrefix(path, "/notes") || capability == "cap-test" && path == "/"
		},
	})
	request := func(method, path string, body io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, body)
		req.Header.Set("Authorization", "Bearer cap-test")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/manifest?path=/", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	write := request(http.MethodPut, "/objects?path=/notes/demo.txt", strings.NewReader("hello workspace"))
	if write.Code != http.StatusNoContent {
		t.Fatalf("write status = %d body=%s", write.Code, write.Body.String())
	}
	etag := write.Header().Get("ETag")
	if etag == "" {
		t.Fatal("write did not return etag")
	}

	manifest := request(http.MethodGet, "/manifest?path=/notes", nil)
	if manifest.Code != http.StatusOK || !strings.Contains(manifest.Body.String(), "demo.txt") {
		t.Fatalf("manifest = %d %s", manifest.Code, manifest.Body.String())
	}

	read := request(http.MethodGet, "/objects?path=/notes/demo.txt", nil)
	if read.Code != http.StatusOK || read.Body.String() != "hello workspace" {
		t.Fatalf("read = %d %q", read.Code, read.Body.String())
	}
	notModifiedRequest := httptest.NewRequest(http.MethodGet, "/objects?path=/notes/demo.txt", nil)
	notModifiedRequest.Header.Set("Authorization", "Bearer cap-test")
	notModifiedRequest.Header.Set("If-None-Match", etag)
	notModified := httptest.NewRecorder()
	handler.ServeHTTP(notModified, notModifiedRequest)
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
		t.Fatalf("conditional read = %d %q", notModified.Code, notModified.Body.String())
	}
	// Issue a range request explicitly so the service exercises lazy reads.
	rangeRequest := httptest.NewRequest(http.MethodGet, "/objects?path=/notes/demo.txt", nil)
	rangeRequest.Header.Set("Authorization", "Bearer cap-test")
	rangeRequest.Header.Set("Range", "bytes=6-14")
	rangeResponse := httptest.NewRecorder()
	handler.ServeHTTP(rangeResponse, rangeRequest)
	if rangeResponse.Code != http.StatusPartialContent || rangeResponse.Body.String() != "workspace" {
		t.Fatalf("range = %d %q", rangeResponse.Code, rangeResponse.Body.String())
	}

	conflict := httptest.NewRequest(http.MethodPut, "/objects?path=/notes/demo.txt", strings.NewReader("stale"))
	conflict.Header.Set("Authorization", "Bearer cap-test")
	conflict.Header.Set("If-Match", `"stale"`)
	conflictResponse := httptest.NewRecorder()
	handler.ServeHTTP(conflictResponse, conflict)
	if conflictResponse.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d", conflictResponse.Code)
	}

	if _, err := os.Stat(filepath.Join(root, "notes", "demo.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestLocalStoreRejectsTraversal(t *testing.T) {
	store := NewLocalStore(t.TempDir())
	if _, err := store.List("/../secret"); err != ErrInvalidPath {
		t.Fatalf("got %v", err)
	}
}

func TestLocalStoreRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	store := NewLocalStore(root)
	if _, err := store.Read("/link/secret", 0, 0); err != ErrInvalidPath {
		t.Fatalf("read through symlink: got %v", err)
	}
	if _, err := store.Write("/link/new", []byte("escape"), "", "write-link"); err != ErrInvalidPath {
		t.Fatalf("write through symlink: got %v", err)
	}
	if err := store.Mkdir("/link/new-dir", "mkdir-link"); err != ErrInvalidPath {
		t.Fatalf("mkdir through symlink: got %v", err)
	}
	entries, err := store.List("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink leaked through manifest: %+v", entries)
	}
}
