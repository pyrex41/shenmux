package snapshot

import (
	"crypto/rand"
	"fmt"
	"time"
)

// NewUUID returns a random RFC 4122 version-4 UUID string, sourced from
// crypto/rand.
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// time0 is the fixed epoch used to zero mtimes inside deterministic archives.
func time0() time.Time { return time.Unix(0, 0).UTC() }
