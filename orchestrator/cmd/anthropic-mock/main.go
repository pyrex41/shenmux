// Command anthropic-mock is a minimal local stand-in for the Anthropic
// Messages API, used only by the pi demo (demo/pi-demo.sh). It lets the REAL
// pi coding agent complete a turn without live credentials: the worker's
// secret-proxy MITMs api.anthropic.com and redirects it here, and this server
// streams back a valid one-turn text response.
//
// It also records the Authorization / x-api-key it received to --auth-file, so
// the demo can assert that the credential the proxy delivered here is the REAL
// value (proving the swap) while pi itself only ever held the placeholder. The
// received credential is never written to stdout/stderr.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8093", "listen address host:port")
	authFile := flag.String("auth-file", "", "write the received x-api-key/authorization here")
	flag.Parse()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))

		// Record the credential the proxy delivered (proves the swap).
		if *authFile != "" {
			cred := r.Header.Get("x-api-key")
			if cred == "" {
				cred = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			}
			_ = os.WriteFile(*authFile, []byte(cred), 0o600)
		}

		if !strings.Contains(r.URL.Path, "/messages") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"mock: only /v1/messages"}}`)
			return
		}

		reply := "PI-WORKER-OK: hello from the mock model." + promptEcho(body)
		streamMessage(w, reply)
	})

	fmt.Fprintf(os.Stderr, "anthropic-mock: listening on %s\n", *listen)
	if err := http.ListenAndServe(*listen, nil); err != nil {
		fmt.Fprintln(os.Stderr, "anthropic-mock:", err)
		os.Exit(1)
	}
}

// promptEcho pulls a short echo of the user's prompt from the request so that
// workers spawned with different prompts produce visibly different output.
func promptEcho(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		// content may be a string or an array of blocks.
		var s string
		if json.Unmarshal(req.Messages[i].Content, &s) == nil && s != "" {
			return " re: " + trim(s)
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(req.Messages[i].Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "text" && b.Text != "" {
					return " re: " + trim(b.Text)
				}
			}
		}
		break
	}
	return ""
}

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// streamMessage emits a valid single-turn Anthropic streaming response.
func streamMessage(w http.ResponseWriter, text string) {
	fl, ok := w.(http.Flusher)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"mock: no flush"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	bw := bufio.NewWriter(w)

	send := func(event string, data map[string]any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(bw, "event: %s\ndata: %s\n\n", event, b)
		bw.Flush()
		fl.Flush()
	}

	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_mock", "type": "message", "role": "assistant", "model": "mock",
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
	}})
	send("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""}})
	send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text}})
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 12}})
	send("message_stop", map[string]any{"type": "message_stop"})
}
