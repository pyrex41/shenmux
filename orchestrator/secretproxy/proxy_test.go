package secretproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

const (
	placeholder = "sk-placeholder-anthropic"
	realValue   = "sk-REAL-anthropic-12345"
)

// echoReply is what the in-test upstream reports back.
type echoReply struct {
	ReceivedAuth   string `json:"received_auth"`
	ReceivedAPIKey string `json:"received_apikey"`
	Host           string `json:"host"`
	ReceivedBody   string `json:"received_body"`
}

// startUpstream starts a TLS echo server that reflects the auth header, the
// X-Api-Key header, the Host, and the request body.
func startUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.NewEncoder(w).Encode(echoReply{
			ReceivedAuth:   r.Header.Get("Authorization"),
			ReceivedAPIKey: r.Header.Get("X-Api-Key"),
			Host:           r.Host,
			ReceivedBody:   string(b),
		})
	}))
	t.Cleanup(up.Close)
	return up
}

// startProxy builds a proxyHandler wired to send every upstream request (both
// the MITM leg and the blind-tunnel leg) to upstreamAddr, and serves it. It
// returns the proxy URL and the path to the proxy's CA cert.
func startProxy(t *testing.T, secrets *Secrets, upstreamAddr string) (proxyURL, caCrt string) {
	t.Helper()

	caDir := t.TempDir()
	crt, err := EnsureCA(caDir)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	caCert, caKey, err := LoadCA(caDir)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}

	redirect := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", upstreamAddr)
	}

	h := &proxyHandler{
		secrets:   secrets,
		caCert:    caCert,
		caKey:     caKey,
		certCache: newCertCache(maxCertCacheSize, log.New(io.Discard, "", 0)),
		logger:    log.New(io.Discard, "", 0),
		sem:       make(chan struct{}, maxConcurrentConns),
		// MITM leg: ignore the real destination and hit our upstream. Skip
		// verify because the upstream serves a self-signed httptest cert — this
		// leg is not what the test is exercising.
		upstreamTransport: &http.Transport{
			DialContext:     redirect,
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		// Blind-tunnel leg: connect the raw bytes to our upstream.
		dialTunnel: redirect,
	}

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL, crt
}

// mitmClient returns a client that proxies through proxyURL and trusts the
// proxy CA — the realistic path for an allowed (MITM'd) host.
func mitmClient(t *testing.T, proxyURL, caCrt string) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(caCrt)
	if err != nil {
		t.Fatalf("read ca: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("failed to add proxy CA to pool")
	}
	pu, _ := url.Parse(proxyURL)
	return &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(pu),
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}
}

// tunnelClient returns a client that proxies through proxyURL but skips TLS
// verification — used for the blocked host, where the client speaks TLS
// directly to the (self-signed) upstream through a blind tunnel.
func tunnelClient(t *testing.T, proxyURL string) *http.Client {
	t.Helper()
	pu, _ := url.Parse(proxyURL)
	return &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(pu),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
}

func doGet(t *testing.T, c *http.Client, rawurl string, headers map[string]string) echoReply {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawurl, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", rawurl, err)
	}
	defer resp.Body.Close()
	var reply echoReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return reply
}

func secretsFor(hosts ...string) *Secrets {
	return &Secrets{Secrets: map[string]Secret{
		"ANTHROPIC_API_KEY": {
			Placeholder:  placeholder,
			Value:        realValue,
			AllowedHosts: hosts,
		},
	}}
}

// (i) A request through the proxy to an allowed host arrives upstream with the
// REAL value substituted.
func TestAllowedHostGetsRealSecret(t *testing.T) {
	up := startUpstream(t)
	proxyURL, caCrt := startProxy(t, secretsFor("api.anthropic.com"), up.Listener.Addr().String())
	c := mitmClient(t, proxyURL, caCrt)

	reply := doGet(t, c, "https://api.anthropic.com/", map[string]string{
		"Authorization": "Bearer " + placeholder,
	})

	if !strings.Contains(reply.ReceivedAuth, realValue) {
		t.Fatalf("allowed host did not receive real secret: got %q", reply.ReceivedAuth)
	}
	if strings.Contains(reply.ReceivedAuth, placeholder) {
		t.Fatalf("placeholder leaked through to allowed host: got %q", reply.ReceivedAuth)
	}
}

// (ii) A request to a NON-allowed host arrives with the placeholder unchanged
// (blind tunnel, no swap).
func TestBlockedHostKeepsPlaceholder(t *testing.T) {
	up := startUpstream(t)
	// Allow only api.anthropic.com; the request goes to a different host.
	proxyURL, _ := startProxy(t, secretsFor("api.anthropic.com"), up.Listener.Addr().String())
	c := tunnelClient(t, proxyURL)

	reply := doGet(t, c, "https://evil.example.org/", map[string]string{
		"Authorization": "Bearer " + placeholder,
	})

	if !strings.Contains(reply.ReceivedAuth, placeholder) {
		t.Fatalf("blocked host should receive placeholder unchanged: got %q", reply.ReceivedAuth)
	}
	if strings.Contains(reply.ReceivedAuth, realValue) {
		t.Fatalf("real secret leaked to blocked host: got %q", reply.ReceivedAuth)
	}
}

// (iii) The body is never scanned: a placeholder in the body is not replaced,
// even for an allowed host.
func TestBodyNeverScanned(t *testing.T) {
	up := startUpstream(t)
	proxyURL, caCrt := startProxy(t, secretsFor("api.anthropic.com"), up.Listener.Addr().String())
	c := mitmClient(t, proxyURL, caCrt)

	// POST with the placeholder ONLY in the body and NOT in any auth header.
	body := "the token is " + placeholder + " somewhere in the payload"
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/", strings.NewReader(body))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var reply echoReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !strings.Contains(reply.ReceivedBody, placeholder) {
		t.Fatalf("body placeholder should pass through untouched: got %q", reply.ReceivedBody)
	}
	if strings.Contains(reply.ReceivedBody, realValue) {
		t.Fatalf("body was scanned and secret injected into body: got %q", reply.ReceivedBody)
	}
}

// (iv) Suffix host matching works: ".anthropic.com" matches "api.anthropic.com".
func TestSuffixHostMatching(t *testing.T) {
	up := startUpstream(t)
	proxyURL, caCrt := startProxy(t, secretsFor(".anthropic.com"), up.Listener.Addr().String())
	c := mitmClient(t, proxyURL, caCrt)

	reply := doGet(t, c, "https://api.anthropic.com/", map[string]string{
		"Authorization": "Bearer " + placeholder,
	})

	if !strings.Contains(reply.ReceivedAuth, realValue) {
		t.Fatalf("suffix-matched host did not receive real secret: got %q", reply.ReceivedAuth)
	}
}

// Unit coverage for the suffix/exact matcher (the deliberate divergence).
func TestHostMatches(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"api.example.com", "api.example.com", true},
		{"api.example.com", "other.example.com", false},
		{".example.com", "api.example.com", true},
		{".example.com", "a.b.example.com", true},
		{".example.com", "example.com", true},
		{".example.com", "notexample.com", false},
		{".example.com", "example.com.evil.com", false},
		{"API.EXAMPLE.COM", "api.example.com", true},
		{".example.com", "api.example.com.", true},
	}
	for _, c := range cases {
		if got := hostMatches(c.pattern, c.host); got != c.want {
			t.Errorf("hostMatches(%q,%q)=%v want %v", c.pattern, c.host, got, c.want)
		}
	}
}
