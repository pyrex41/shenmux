// Package mockagent implements a scripted, env-driven harness that proves the
// whole worker loop without a real LLM key (CONTRACTS.md §4).
//
// It stands in for a real coding-agent CLI (codex/pi/opencode) inside a
// shenmux-run PTY. Each step it: prints a banner (which becomes the PTY session
// content), makes one authenticated HTTP GET through the secret-proxy so the
// placeholder-for-real swap is visible, and writes to its workspace so that
// checkpoints and forks visibly diverge.
package mockagent

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the harness's env-derived configuration. All fields come from the
// environment so the harness stays a plain subprocess the operator can launch.
type Config struct {
	// APIKey is the placeholder the agent holds (env ANTHROPIC_API_KEY). The
	// agent never sees the real value — the secret-proxy swaps it in flight.
	APIKey string
	// Proxy is the HTTPS_PROXY/HTTP_PROXY URL the outbound call is routed
	// through. Empty means a direct connection.
	Proxy string
	// Workspace is the harness working dir; notes.md/progress.txt live here.
	Workspace string
	// Target is the URL the agent GETs each step (env MOCK_TARGET).
	Target string
	// Steps is the number of iterations (env MOCK_STEPS).
	Steps int
	// Worker is this worker's identity (env WORKER, else hostname).
	Worker string
	// CAFile, if set, is a PEM CA bundle added to the client's trust roots so
	// the MITM proxy's per-host certs verify (env SSL_CERT_FILE /
	// NODE_EXTRA_CA_CERTS).
	CAFile string
	// StepDelay is the pause between steps (default ~300ms).
	StepDelay time.Duration
}

// firstEnv returns the first non-empty value among the named env vars.
func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// LoadConfig builds a Config from the process environment, applying the
// defaults from CONTRACTS.md §4.
func LoadConfig() Config {
	cfg := Config{
		APIKey:    os.Getenv("ANTHROPIC_API_KEY"),
		Proxy:     firstEnv("HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"),
		Workspace: os.Getenv("WORKSPACE"),
		Target:    os.Getenv("MOCK_TARGET"),
		Worker:    os.Getenv("WORKER"),
		CAFile:    firstEnv("SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE"),
		StepDelay: 300 * time.Millisecond,
	}
	if cfg.APIKey == "" {
		cfg.APIKey = "sk-placeholder-anthropic"
	}
	if cfg.Target == "" {
		// Default is a deliberately dead port; the demo overrides it.
		cfg.Target = "http://127.0.0.1:9"
	}
	if cfg.Workspace == "" {
		cfg.Workspace = "."
	}
	if cfg.Worker == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			cfg.Worker = h
		} else {
			cfg.Worker = "worker"
		}
	}
	cfg.Steps = 3
	if s := os.Getenv("MOCK_STEPS"); s != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			cfg.Steps = n
		}
	}
	return cfg
}

// buildClient constructs the HTTP client: a Transport whose Proxy always routes
// through cfg.Proxy (unlike http.ProxyFromEnvironment, which silently excludes
// loopback — fatal here, since the demo's proxy and upstream are both on
// 127.0.0.1), trusting cfg.CAFile on top of the system roots so the MITM proxy
// verifies.
func (cfg Config) buildClient() (*http.Client, error) {
	tr := &http.Transport{}

	if cfg.Proxy != "" {
		pu, err := url.Parse(cfg.Proxy)
		if err != nil {
			return nil, fmt.Errorf("bad proxy URL %q: %w", cfg.Proxy, err)
		}
		// Always return the configured proxy, including for loopback targets.
		tr.Proxy = func(*http.Request) (*url.URL, error) { return pu, nil }
	}

	if cfg.CAFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read CA file %q: %w", cfg.CAFile, err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certs parsed from CA file %q", cfg.CAFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	}

	return &http.Client{
		Transport: tr,
		Timeout:   10 * time.Second,
	}, nil
}

// upstreamResponse is the JSON the mock-upstream echoes back: the Authorization
// header value it actually received (CONTRACTS.md §4).
type upstreamResponse struct {
	ReceivedAuth string `json:"received_auth"`
}

// callTarget performs one authenticated GET and returns what the upstream
// reports it received. The returned string has any "Bearer " prefix trimmed so
// it lines up with the placeholder the agent holds.
func (cfg Config) callTarget(client *http.Client) (string, error) {
	req, err := http.NewRequest(http.MethodGet, cfg.Target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	var ur upstreamResponse
	if err := json.Unmarshal(body, &ur); err != nil {
		// Not JSON we understand; surface the raw (truncated) body so the
		// operator can see what came back.
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return "", fmt.Errorf("unexpected response (status %d): %q", resp.StatusCode, snippet)
	}
	return strings.TrimSpace(strings.TrimPrefix(ur.ReceivedAuth, "Bearer ")), nil
}

// writeWorkspace appends the step marker to notes.md and records the current
// step count in progress.txt, so forked workspaces visibly diverge.
func (cfg Config) writeWorkspace(step int) error {
	if err := os.MkdirAll(cfg.Workspace, 0o755); err != nil {
		return err
	}
	notes := filepath.Join(cfg.Workspace, "notes.md")
	f, err := os.OpenFile(notes, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("step %d by %s at %d\n", step, cfg.Worker, time.Now().Unix())
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	progress := filepath.Join(cfg.Workspace, "progress.txt")
	return os.WriteFile(progress, []byte(strconv.Itoa(step)+"\n"), 0o644)
}

// Run executes the scripted loop, writing all narration to out. It always
// returns 0 (per §4 the harness exits 0 after the loop); transient errors are
// printed and the loop continues.
func Run(cfg Config, out io.Writer) int {
	client, err := cfg.buildClient()
	if err != nil {
		// A misconfigured client is fatal for the outbound call but we still
		// run the workspace-writing loop so checkpoints/forks work.
		fmt.Fprintf(out, "mock-agent: client setup failed: %v\n", err)
		client = &http.Client{Timeout: 10 * time.Second}
	}

	for step := 1; step <= cfg.Steps; step++ {
		fmt.Fprintf(out, "=== mock-agent step %d/%d worker=%s target=%s ===\n",
			step, cfg.Steps, cfg.Worker, cfg.Target)

		received, callErr := cfg.callTarget(client)
		fmt.Fprintf(out, "agent-holds: %s\n", cfg.APIKey)
		if callErr != nil {
			fmt.Fprintf(out, "mock-target error: %v\n", callErr)
		} else {
			fmt.Fprintf(out, "upstream-received: %s\n", received)
		}

		if err := cfg.writeWorkspace(step); err != nil {
			fmt.Fprintf(out, "workspace write error: %v\n", err)
		}

		if step < cfg.Steps {
			time.Sleep(cfg.StepDelay)
		}
	}

	fmt.Fprintf(out, "DONE %s\n", cfg.Worker)
	return 0
}
