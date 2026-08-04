package webgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesEmbeddedClient(t *testing.T) {
	handler := New(context.Background(), Config{}).Handler()
	for _, test := range []struct {
		path string
		want string
	}{
		{path: "/", want: "<!doctype html>"},
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
