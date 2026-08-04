package relay

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

const transcriptDomain = "shenmux-relay-agent-challenge-v1"

// NewEnrollmentCode returns a URL-safe, single-use bootstrap code. The
// controller should hash this value with HashEnrollmentCode before storing it.
func NewEnrollmentCode() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate enrollment code: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func HashEnrollmentCode(code string) [32]byte { return sha256.Sum256([]byte(code)) }

// GenerateDeviceKey creates the long-lived key held by an agent.  Callers are
// responsible for persisting the private key with owner-only permissions.
func GenerateDeviceKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate device key: %w", err)
	}
	return pub, priv, nil
}

// ChallengeTranscript is deterministic and domain separated so a signature
// cannot be replayed as a signature for another protocol.  Origin, device ID,
// and both nonces are included as required by the relay handshake.
func ChallengeTranscript(version uint16, origin, deviceID string, agentNonce, controllerNonce []byte) []byte {
	var out bytes.Buffer
	out.WriteString(transcriptDomain)
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], version)
	out.Write(b[:])
	writeField := func(v []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(v)))
		out.Write(n[:])
		out.Write(v)
	}
	writeField([]byte(origin))
	writeField([]byte(deviceID))
	writeField(agentNonce)
	writeField(controllerNonce)
	return out.Bytes()
}

func SignChallenge(priv ed25519.PrivateKey, version uint16, origin, deviceID string, agentNonce, controllerNonce []byte) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid Ed25519 private key")
	}
	return ed25519.Sign(priv, ChallengeTranscript(version, origin, deviceID, agentNonce, controllerNonce)), nil
}

func VerifyChallenge(pub ed25519.PublicKey, signature []byte, version uint16, origin, deviceID string, agentNonce, controllerNonce []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("invalid Ed25519 public key")
	}
	if len(signature) != ed25519.SignatureSize {
		return errors.New("invalid Ed25519 signature")
	}
	if !ed25519.Verify(pub, ChallengeTranscript(version, origin, deviceID, agentNonce, controllerNonce), signature) {
		return errors.New("invalid challenge signature")
	}
	return nil
}

// EnrollmentRequest is the HTTPS bootstrap payload.  Code is single-use and
// short-lived; the controller must consume it atomically before issuing a
// credential.  PublicKey is copied by callers before storing it.
type EnrollmentRequest struct {
	Code      string            `json:"code"`
	PublicKey ed25519.PublicKey `json:"public_key"`
}

// DeviceCredential is the opaque credential returned by enrollment.  The
// controller may rotate Token before ExpiresAt; the agent never sends its
// private key over the network.
type DeviceCredential struct {
	DeviceID  string    `json:"device_id"`
	Token     []byte    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c DeviceCredential) Validate(now time.Time) error {
	if c.DeviceID == "" {
		return errors.New("device credential has no device ID")
	}
	if len(c.Token) == 0 {
		return errors.New("device credential has no token")
	}
	if !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt) {
		return errors.New("device credential is expired")
	}
	return nil
}
