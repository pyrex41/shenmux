package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func TestSignedManifest(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	m := Manifest{Version: "1.2.3", URL: "https://example.test/shenmux", SHA256: strings.Repeat("a", 64), Size: 10}
	signed, err := Sign(m, priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := signed.Verify(pub); err != nil {
		t.Fatal(err)
	}
	signed.Manifest.Version = "9.9.9"
	if err := signed.Verify(pub); err == nil {
		t.Fatal("tampered manifest verified")
	}
}
