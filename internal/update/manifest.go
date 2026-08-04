// Package update verifies signed release metadata before an agent accepts an
// update. It does not download or install binaries; those are platform policy.
package update

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
)

type Manifest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

type SignedManifest struct {
	Manifest Manifest `json:"manifest"`
	Sig      []byte   `json:"sig"`
}

func (m Manifest) Validate() error {
	if m.Version == "" || m.URL == "" || len(m.SHA256) != 64 || m.Size <= 0 {
		return errors.New("invalid update manifest")
	}
	return nil
}

func (m Manifest) canonical() ([]byte, error) { return json.Marshal(m) }

func Sign(m Manifest, private ed25519.PrivateKey) (SignedManifest, error) {
	if err := m.Validate(); err != nil {
		return SignedManifest{}, err
	}
	if len(private) != ed25519.PrivateKeySize {
		return SignedManifest{}, errors.New("invalid update signing key")
	}
	data, err := m.canonical()
	if err != nil {
		return SignedManifest{}, err
	}
	return SignedManifest{Manifest: m, Sig: ed25519.Sign(private, data)}, nil
}

func (s SignedManifest) Verify(public ed25519.PublicKey) error {
	if err := s.Manifest.Validate(); err != nil {
		return err
	}
	if len(public) != ed25519.PublicKeySize || len(s.Sig) != ed25519.SignatureSize {
		return errors.New("invalid update signature")
	}
	data, err := s.Manifest.canonical()
	if err != nil {
		return fmt.Errorf("canonicalize update manifest: %w", err)
	}
	if !ed25519.Verify(public, data, s.Sig) {
		return errors.New("invalid update signature")
	}
	return nil
}
