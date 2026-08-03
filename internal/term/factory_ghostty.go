//go:build libghostty && cgo && (linux || darwin)

package term

import "github.com/pyrex41/shenmux/internal/shenguard"

func New(dim shenguard.Dimensions) (Terminal, error) { return NewGhostty(dim) }
