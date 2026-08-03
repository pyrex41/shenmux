//go:build !cgo || (!linux && !darwin)

package ttyx

import "errors"

var ErrUnsupported = errors.New("terminal raw mode requires cgo on Linux or macOS")

type State struct{}

func IsTerminal(uintptr) bool         { return false }
func MakeRaw(uintptr) (*State, error) { return nil, ErrUnsupported }
func (*State) Restore() error         { return nil }
func Size(uintptr) (int, int, error)  { return 0, 0, ErrUnsupported }
