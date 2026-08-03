package naming

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"
)

func ValidateSession(name string) error {
	if name == "" {
		return errors.New("session name must not be empty")
	}
	if len(name) > 64 {
		return errors.New("session name exceeds 64 bytes")
	}
	if !utf8.ValidString(name) {
		return errors.New("session name must be valid UTF-8")
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("session name contains unsupported character %q", r)
	}
	return nil
}

// DefaultEndpoints returns deterministic IPC addresses inside a per-UID
// directory. The daemon validates that the final directory is owned by the
// effective UID and has no group/other access before binding either socket.
func DefaultEndpoints(session string) (control, data string, err error) {
	if err := ValidateSession(session); err != nil {
		return "", "", err
	}
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("shenmux-%d", os.Geteuid()))
	base := filepath.Join(dir, session)
	return "ipc://" + base + ".ctl", "ipc://" + base + ".pub", nil
}
