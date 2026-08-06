package secretproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxCertCacheSize = 1000 // bounded by allowed_hosts in practice

// EnsureCA generates a self-signed P-256 ECDSA CA into dir (ca.crt + ca.key) if
// it does not already exist. It returns the absolute path to ca.crt so callers
// can configure clients to trust it.
func EnsureCA(dir string) (caCrtPath string, err error) {
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	if _, err := os.Stat(certPath); err == nil {
		abs, _ := filepath.Abs(certPath)
		return abs, nil // already present
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("cannot create CA dir: %w", err)
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("cannot generate CA key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", fmt.Errorf("cannot generate serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "shenmux-secret-proxy-ca"},
		NotBefore:             time.Now().Add(-1 * time.Minute),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		return "", fmt.Errorf("cannot create CA cert: %w", err)
	}

	certOut, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("cannot write CA cert: %w", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		certOut.Close()
		return "", fmt.Errorf("cannot encode CA cert: %w", err)
	}
	certOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(caKey)
	if err != nil {
		return "", fmt.Errorf("cannot marshal CA key: %w", err)
	}
	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("cannot write CA key: %w", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		keyOut.Close()
		return "", fmt.Errorf("cannot encode CA key: %w", err)
	}
	keyOut.Close()

	abs, _ := filepath.Abs(certPath)
	return abs, nil
}

// LoadCA loads the CA cert + key from dir.
func LoadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read CA cert: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read CA key: %w", err)
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, nil, fmt.Errorf("cannot decode CA cert PEM")
	}
	caCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot parse CA cert: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("cannot decode CA key PEM")
	}
	// Accept both SEC 1 (what EnsureCA writes) and PKCS#8 (OpenSSL default).
	var caKey *ecdsa.PrivateKey
	if key, err := x509.ParseECPrivateKey(keyBlock.Bytes); err == nil {
		caKey = key
	} else if key, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err == nil {
		ec, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, nil, fmt.Errorf("CA key is not ECDSA")
		}
		caKey = ec
	} else {
		return nil, nil, fmt.Errorf("cannot parse CA key")
	}

	return caCert, caKey, nil
}

// certCache is a bounded cert cache. When full it is cleared entirely — simple
// and sufficient since allowed_hosts is small in practice.
type certCache struct {
	mu     sync.RWMutex
	certs  map[string]*tls.Certificate
	size   int
	logger *log.Logger
}

func newCertCache(maxSize int, logger *log.Logger) *certCache {
	return &certCache{certs: make(map[string]*tls.Certificate), size: maxSize, logger: logger}
}

func (c *certCache) get(host string) (*tls.Certificate, bool) {
	c.mu.RLock()
	cert, ok := c.certs[host]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	// Drop certs past their validity window so we never serve an expired leaf.
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil || time.Now().After(leaf.NotAfter) {
		c.mu.Lock()
		delete(c.certs, host)
		c.mu.Unlock()
		return nil, false
	}
	return cert, true
}

func (c *certCache) put(host string, cert *tls.Certificate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.certs) >= c.size {
		c.certs = make(map[string]*tls.Certificate) // reset; regenerate on demand
		if c.logger != nil {
			c.logger.Printf("cert cache full (%d), cleared", c.size)
		}
	}
	c.certs[host] = cert
}

// generateCert returns a leaf cert for host, signed by the CA, cached by host.
func generateCert(host string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, cache *certCache) (*tls.Certificate, error) {
	if cert, ok := cache.get(host); ok {
		return cert, nil
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-1 * time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// A CONNECT target may be an IP literal (rare) or a DNS name (usual).
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("sign cert: %w", err)
	}

	tlsCert := &tls.Certificate{
		Certificate: [][]byte{leafDER, caCert.Raw},
		PrivateKey:  leafKey,
	}
	cache.put(host, tlsCert)
	return tlsCert, nil
}
