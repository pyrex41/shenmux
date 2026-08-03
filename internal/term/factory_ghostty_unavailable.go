//go:build libghostty && (!cgo || (!linux && !darwin))

package term

import (
	"errors"

	"github.com/pyrex41/shenmux/internal/shenguard"
)

func New(shenguard.Dimensions) (Terminal, error) {
	return nil, errors.New("libghostty terminal requires cgo on Linux or macOS")
}
