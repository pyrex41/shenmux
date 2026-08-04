package webgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	workspacebackend "github.com/pyrex41/shenmux/internal/workspace"
)

func TestHandlerServesEmbeddedClient(t *testing.T) {
	handler := New(context.Background(), Config{}).Handler()
	for _, test := range []struct {
		path string
		want string
	}{
		{path: "/", want: "<!doctype html>"},
		{path: "/workspace", want: "local-first prototype"},
		{path: "/workspace-runtime.js", want: "RemoteObjectBackend"},
		{path: "/workspace-component.js", want: "shenmux:workspace/runtime"},
		{path: "/app.bundle.js", want: "WebSocket"},
		{path: "/style.css", want: "terminal-wrap"},
	} {
		req := httptest.NewRequest(http.MethodGet, test.path, nil)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", test.path, res.Code)
		}
		if !strings.Contains(res.Body.String(), test.want) {
			t.Fatalf("GET %s: body missing %q", test.path, test.want)
		}
	}
}

func TestHandlerMountsCapabilityWorkspace(t *testing.T) {
	store := workspacebackend.NewLocalStore(t.TempDir())
	server := New(context.Background(), Config{
		WorkspaceStore: store,
		WorkspaceAuthorize: func(capability, operation, path string) bool {
			return capability == "cap" && operation == "list" && path == "/"
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/workspace-objects/manifest?path=/", nil)
	req.Header.Set("Authorization", "Bearer cap")
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"entries"`) {
		t.Fatalf("workspace manifest = %d %s", res.Code, res.Body.String())
	}
}
