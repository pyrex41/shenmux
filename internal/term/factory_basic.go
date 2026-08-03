//go:build !libghostty

package term

import "github.com/pyrex41/shenmux/internal/shenguard"

func New(dim shenguard.Dimensions) (Terminal, error) { return NewBasic(dim), nil }
