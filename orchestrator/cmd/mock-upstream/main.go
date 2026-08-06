// Command mock-upstream is a tiny echo server for the demo. On any request it
// replies with JSON reporting the Authorization header it received and the Host
// it saw, so the demo can prove the real secret reached an allowed host while a
// blocked host received only the placeholder.
//
//	mock-upstream --http ADDR --https ADDR [--cert FILE --key FILE]
//
// If --cert/--key are omitted, a self-signed cert is generated and its path is
// printed. The received Authorization value is NEVER logged — it appears only
// in the JSON response body.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func echoHandler(w http.ResponseWriter, r *http.Request) {
	// NOTE: deliberately do NOT log the Authorization value anywhere; it is
	// only reflected in the response body below.
	resp := map[string]string{
		"received_auth": r.Header.Get("Authorization"),
		"host":          r.Host,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// generateSelfSigned writes a self-signed cert+key into dir and returns paths.
func generateSelfSigned(dir string) (certPath, keyPath string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "mock-upstream"},
		NotBefore:    time.Now().Add(-1 * time.Minute),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", "mock-upstream"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}

	certPath = filepath.Join(dir, "mock-upstream.crt")
	keyPath = filepath.Join(dir, "mock-upstream.key")

	cf, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", "", err
	}
	pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	cf.Close()

	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}
	kf, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", "", err
	}
	pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	kf.Close()

	certPath, _ = filepath.Abs(certPath)
	keyPath, _ = filepath.Abs(keyPath)
	return certPath, keyPath, nil
}

func main() {
	httpAddr := flag.String("http", "", "plain HTTP listen address host:port (optional)")
	httpsAddr := flag.String("https", "", "HTTPS listen address host:port (optional)")
	certFile := flag.String("cert", "", "TLS certificate file (generated if absent)")
	keyFile := flag.String("key", "", "TLS key file (generated if absent)")
	flag.Parse()

	if *httpAddr == "" && *httpsAddr == "" {
		log.Fatal("mock-upstream: at least one of --http or --https is required")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", echoHandler)

	var wg sync.WaitGroup

	if *httpAddr != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("mock-upstream: HTTP on %s", *httpAddr)
			srv := &http.Server{Addr: *httpAddr, Handler: mux}
			if err := srv.ListenAndServe(); err != nil {
				log.Fatalf("mock-upstream: http server error: %v", err)
			}
		}()
	}

	if *httpsAddr != "" {
		cert, key := *certFile, *keyFile
		if cert == "" || key == "" {
			dir, err := os.MkdirTemp("", "mock-upstream-cert-")
			if err != nil {
				log.Fatalf("mock-upstream: cannot create cert dir: %v", err)
			}
			cert, key, err = generateSelfSigned(dir)
			if err != nil {
				log.Fatalf("mock-upstream: cannot generate self-signed cert: %v", err)
			}
			log.Printf("mock-upstream: generated self-signed cert %s (key %s)", cert, key)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("mock-upstream: HTTPS on %s", *httpsAddr)
			srv := &http.Server{Addr: *httpsAddr, Handler: mux}
			if err := srv.ListenAndServeTLS(cert, key); err != nil {
				log.Fatalf("mock-upstream: https server error: %v", err)
			}
		}()
	}

	wg.Wait()
}
