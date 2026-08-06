package webgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// A tab left open from a previous `shenmux web` process must be refused rather
// than allowed to attach and take the exclusive control lease. Observed live:
// after a demo recycled a port, a stale tab reconnected to the new gateway,
// took control, and resized the session out from under the person using it.
func TestStaleInstanceIsRefused(t *testing.T) {
	srv := New(context.Background(), Config{
		Session: "test", ControlEndpoint: "ipc:///tmp/x.ctl", DataEndpoint: "ipc:///tmp/x.pub",
	})
	handler := srv.Handler()

	if srv.Instance() == "" {
		t.Fatal("every gateway process must have an instance id")
	}

	cases := []struct {
		name        string
		instance    string
		wantStatus  int
		wantRefusal string
	}{
		{"a page from a previous process", "some-older-instance", http.StatusConflict, RefusalStaleInstance},
		{"a page with no instance at all", "", http.StatusConflict, RefusalStaleInstance},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ws?instance="+url.QueryEscape(test.instance), nil)
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", res.Code, test.wantStatus, res.Body.String())
			}
			if got := res.Header().Get("X-Shenmux-Refusal"); got != test.wantRefusal {
				t.Fatalf("refusal reason = %q, want %q", got, test.wantRefusal)
			}
			// The point of the refusal is that a human can act on it.
			if !strings.Contains(res.Body.String(), "reload") {
				t.Fatalf("refusal must tell the user to reload, got %q", res.Body.String())
			}
		})
	}

	// The current page must get past the instance check. It cannot complete a
	// websocket handshake through httptest, but it must not be refused here.
	req := httptest.NewRequest(http.MethodGet, "/ws?instance="+url.QueryEscape(srv.Instance()), nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Header().Get("X-Shenmux-Refusal") == RefusalStaleInstance {
		t.Fatalf("the instance this gateway serves must not be refused: %q", res.Body.String())
	}
}

// The token is defence in depth for the same hijack: with one set, only a
// caller that has the printed URL may attach or read history.
func TestTokenGatesAccessWhenSet(t *testing.T) {
	srv := New(context.Background(), Config{
		Session: "test", ControlEndpoint: "ipc:///tmp/x.ctl", DataEndpoint: "ipc:///tmp/x.pub",
		Token: "sekrit",
	})
	handler := srv.Handler()
	instance := url.QueryEscape(srv.Instance())

	req := httptest.NewRequest(http.MethodGet, "/ws?instance="+instance, nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized || res.Header().Get("X-Shenmux-Refusal") != RefusalBadToken {
		t.Fatalf("a request without the token must be refused: status=%d refusal=%q",
			res.Code, res.Header().Get("X-Shenmux-Refusal"))
	}

	req = httptest.NewRequest(http.MethodGet, "/ws?instance="+instance+"&token=sekrit", nil)
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Header().Get("X-Shenmux-Refusal") == RefusalBadToken {
		t.Fatalf("the correct token must be accepted: %q", res.Body.String())
	}
}

// The served page must carry this process's identity, or the client has
// nothing to present back and every reconnect would be refused.
func TestServedPageCarriesThisInstance(t *testing.T) {
	srv := New(context.Background(), Config{
		Session: "test", ControlEndpoint: "ipc:///tmp/x.ctl", DataEndpoint: "ipc:///tmp/x.pub",
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, req)

	body := res.Body.String()
	if !strings.Contains(body, srv.Instance()) {
		t.Fatal("the served page must carry this gateway's instance id")
	}
	if strings.Contains(body, instancePlaceholder) {
		t.Fatal("the placeholder must be substituted, not served literally")
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
