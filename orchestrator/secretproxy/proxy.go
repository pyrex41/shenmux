// Package secretproxy implements a MITM HTTPS proxy that swaps a placeholder
// token for a real secret — but only inside a small allow-list of auth headers,
// and only for allow-listed destination hosts. The request body is never
// scanned. Adapted from the sq-sandbox reference proxy; standard library only.
//
// Three request paths (see CONTRACTS.md §1):
//
//  1. Plain HTTP           -> rewrite auth headers, forward.
//  2. CONNECT, allowed host -> MITM with a per-host cert signed by the CA,
//     rewrite auth headers, forward over TLS.
//  3. CONNECT, other host  -> blind TCP tunnel (no inspection, no swap).
package secretproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// ── Limits ───────────────────────────────────────────────────────────

const (
	maxResponseBody    = 512 * 1024 * 1024 // 512 MB max response relay
	maxRequestBody     = 64 * 1024 * 1024  // 64 MB max request body
	maxConcurrentConns = 512               // max simultaneous proxy connections
	connIdleTimeout    = 2 * time.Minute   // idle timeout in the MITM read loop
	tunnelTimeout      = 10 * time.Minute  // max duration for a blind tunnel
)

// ── Header allow-lists ───────────────────────────────────────────────

// hopByHopHeaders must not be forwarded by a proxy (RFC 2616 §13.5.1).
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHopByHop(h http.Header) {
	for _, k := range hopByHopHeaders {
		h.Del(k)
	}
}

// replaceableHeaders is the ONLY set of headers scanned for placeholders. This
// prevents accidental secret leakage into arbitrary headers or the body.
var replaceableHeaders = map[string]bool{
	"Authorization":       true,
	"X-Api-Key":           true,
	"Api-Key":             true,
	"X-Auth-Token":        true,
	"X-Access-Token":      true,
	"Proxy-Authorization": true, // replaced before the hop-by-hop strip below
}

// ── Proxy handler ────────────────────────────────────────────────────

type proxyHandler struct {
	secrets   *Secrets
	caCert    *x509.Certificate
	caKey     *ecdsa.PrivateKey
	certCache *certCache
	logger    *log.Logger
	active    atomic.Int64

	// Injectable network legs. Both nil in production (defaults used); the
	// in-package test sets them to redirect at synthetic upstreams.
	upstreamTransport http.RoundTripper                                          // MITM leg
	dialTunnel        func(ctx context.Context, network, addr string) (net.Conn, error) // blind-tunnel leg

	sem chan struct{}
}

func (p *proxyHandler) upstream() http.RoundTripper {
	if p.upstreamTransport != nil {
		return p.upstreamTransport
	}
	return http.DefaultTransport
}

func (p *proxyHandler) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if p.dialTunnel != nil {
		return p.dialTunnel(ctx, network, addr)
	}
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
}

func (p *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case p.sem <- struct{}{}:
	default:
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	p.active.Add(1)
	defer func() {
		p.active.Add(-1)
		<-p.sem
	}()

	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
	} else {
		p.handleHTTP(w, r)
	}
}

// handleHTTP forwards plain HTTP requests with header replacement.
func (p *proxyHandler) handleHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Hostname()

	p.replaceHeaders(r, host)
	stripHopByHop(r.Header)

	var body io.Reader
	if r.Body != nil {
		body = io.LimitReader(r.Body, maxRequestBody)
	}
	outReq, err := http.NewRequest(r.Method, r.URL.String(), body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	outReq.Header = r.Header.Clone()

	resp, err := p.upstream().RoundTrip(outReq)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	stripHopByHop(w.Header())
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, maxResponseBody))
}

// handleConnect splits CONNECT into MITM (allowed host) vs blind tunnel.
func (p *proxyHandler) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		p.logger.Printf("hijack error: %v", err)
		return
	}
	defer clientConn.Close()

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	if p.secrets.hostAllowed(host) {
		p.tlsMITM(clientConn, host, r.Host)
	} else {
		p.tcpTunnel(clientConn, r.Host)
	}
}

// tlsMITM terminates TLS with a generated cert, reads plaintext requests,
// rewrites auth headers, and forwards them over TLS to the real upstream.
func (p *proxyHandler) tlsMITM(clientConn net.Conn, host, hostPort string) {
	cert, err := generateCert(host, p.caCert, p.caKey, p.certCache)
	if err != nil {
		p.logger.Printf("cert generation error for %s: %v", host, err)
		return
	}

	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{*cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err := tlsConn.Handshake(); err != nil {
		p.logger.Printf("TLS handshake error for %s: %v", host, err)
		return
	}
	defer tlsConn.Close()

	if _, _, err := net.SplitHostPort(hostPort); err != nil {
		hostPort = hostPort + ":443"
	}

	br := bufio.NewReader(tlsConn)
	for {
		tlsConn.SetReadDeadline(time.Now().Add(connIdleTimeout))

		req, err := http.ReadRequest(br)
		if err != nil {
			if err != io.EOF {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					return // idle close
				}
				p.logger.Printf("read request error for %s: %v", host, err)
			}
			return
		}
		tlsConn.SetReadDeadline(time.Time{})

		req.URL.Scheme = "https"
		req.URL.Host = hostPort
		req.RequestURI = ""

		// Demo host-redirect: forward this intercepted host to a local upstream
		// (secret injection still happens below, against the original host).
		if sc, hp, ok := p.secrets.redirectFor(host); ok {
			req.URL.Scheme = sc
			req.URL.Host = hp
			req.Host = "" // use URL.Host for the outbound Host header
		}

		// Buffer the body so the transport can rewind on a retried connection.
		if req.Body != nil {
			bodyBytes, readErr := io.ReadAll(io.LimitReader(req.Body, maxRequestBody))
			req.Body.Close()
			if readErr != nil {
				p.logger.Printf("read body error for %s: %v", host, readErr)
				tlsConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
				return
			}
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			req.ContentLength = int64(len(bodyBytes))
			req.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(bodyBytes)), nil
			}
		}

		p.replaceHeaders(req, host)
		stripHopByHop(req.Header)

		tlsConn.SetWriteDeadline(time.Now().Add(20 * time.Minute))

		resp, err := p.upstream().RoundTrip(req)
		if err != nil {
			p.logger.Printf("upstream error for %s: %v", host, err)
			tlsConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
			return
		}

		resp.Body = io.NopCloser(io.LimitReader(resp.Body, maxResponseBody))
		// Normalise to HTTP/1.1 so simple clients handle auth-retry correctly.
		resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/1.1", 1, 1

		err = resp.Write(tlsConn)
		resp.Body.Close()
		tlsConn.SetWriteDeadline(time.Time{})
		if err != nil {
			return
		}
		if resp.Close || req.Close {
			return
		}
	}
}

// tcpTunnel does a blind bidirectional copy — no inspection, no header swap.
func (p *proxyHandler) tcpTunnel(clientConn net.Conn, hostPort string) {
	if _, _, err := net.SplitHostPort(hostPort); err != nil {
		hostPort = hostPort + ":443"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream, err := p.dial(ctx, "tcp", hostPort)
	if err != nil {
		p.logger.Printf("tunnel dial error for %s: %v", hostPort, err)
		return
	}
	defer upstream.Close()

	deadline := time.Now().Add(tunnelTimeout)
	clientConn.SetDeadline(deadline)
	upstream.SetDeadline(deadline)

	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, clientConn); done <- struct{}{} }()
	go func() { io.Copy(clientConn, upstream); done <- struct{}{} }()
	<-done
}

// replaceHeaders scans only the credential headers and only swaps a placeholder
// for its real value when the destination host is allowed for that secret.
func (p *proxyHandler) replaceHeaders(r *http.Request, host string) {
	for name, values := range r.Header {
		if !replaceableHeaders[http.CanonicalHeaderKey(name)] {
			continue
		}
		for i, v := range values {
			for secretName, secret := range p.secrets.Secrets {
				newVal, present := replaceInHeader(v, secret, host)
				if !present {
					continue
				}
				if newVal != "" { // present AND host allowed -> replaced
					values[i] = newVal
					v = newVal
					p.logger.Printf("replaced %s for %s", secretName, host)
				} else {
					p.logger.Printf("blocked %s host %s not allowed", secretName, host)
				}
			}
		}
	}
}

// replaceInHeader inspects one header value for secret.Placeholder, handling
// both plain values (Bearer tokens, API keys) and HTTP Basic credentials
// (base64 "user:password"). It returns:
//
//	("<newvalue>", true) — placeholder present AND host allowed: swapped value
//	("",           true) — placeholder present but host NOT allowed: caller passes original through, logs a block
//	("",           false) — placeholder not present: nothing to do
func replaceInHeader(v string, secret Secret, host string) (newVal string, present bool) {
	plainMatch := strings.Contains(v, secret.Placeholder)

	// For Basic auth, decode and look inside the credentials too.
	var basicDecoded string
	if !plainMatch {
		if encoded, ok := strings.CutPrefix(v, "Basic "); ok {
			if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded)); err == nil {
				if strings.Contains(string(decoded), secret.Placeholder) {
					basicDecoded = string(decoded)
				}
			}
		}
	}

	if !plainMatch && basicDecoded == "" {
		return "", false
	}

	// Placeholder is present. Enforce allowed_hosts (suffix + exact matching).
	if !secretAllowsHost(secret, host) {
		return "", true // blocked
	}

	if basicDecoded != "" {
		newDecoded := strings.ReplaceAll(basicDecoded, secret.Placeholder, secret.Value)
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(newDecoded)), true
	}
	return strings.ReplaceAll(v, secret.Placeholder, secret.Value), true
}

// ── Run ──────────────────────────────────────────────────────────────

// Run starts the MITM proxy on listen, injecting secrets from secretsPath using
// a CA in caDir (auto-generated as a P-256 ECDSA CA if ca.crt/ca.key are
// absent). The ca.crt path is written to logw so callers can trust it. Run
// blocks until ctx is cancelled or the server fails.
func Run(ctx context.Context, listen, secretsPath, caDir string, logw io.Writer) error {
	if logw == nil {
		logw = io.Discard
	}
	logger := log.New(logw, "secret-proxy: ", log.LstdFlags)

	caCrtPath, err := EnsureCA(caDir)
	if err != nil {
		return err
	}
	logger.Printf("ca.crt %s", caCrtPath)

	secrets, err := LoadSecrets(secretsPath)
	if err != nil {
		return err
	}
	caCert, caKey, err := LoadCA(caDir)
	if err != nil {
		return err
	}

	handler := &proxyHandler{
		secrets:   secrets,
		caCert:    caCert,
		caKey:     caKey,
		certCache: newCertCache(maxCertCacheSize, logger),
		logger:    logger,
		sem:       make(chan struct{}, maxConcurrentConns),
	}

	server := &http.Server{
		Addr:        listen,
		Handler:     handler,
		ReadTimeout: 30 * time.Second,
		IdleTimeout: connIdleTimeout,
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", listen, err)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logger.Printf("listening on %s (%d secrets configured)", ln.Addr(), len(secrets.Secrets))
	if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
