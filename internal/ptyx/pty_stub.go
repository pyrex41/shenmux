//go:build !linux && !darwin

package ptyx

import (
	"errors"
	"syscall"

	"github.com/pyrex41/shenmux/internal/shenguard"
)

var ErrUnsupported = errors.New("PTY support is unavailable on this operating system")

type PTY struct{}

func Start([]string, []string, shenguard.Dimensions) (*PTY, error) { return nil, ErrUnsupported }
func (*PTY) Read([]byte) (int, error)                              { return 0, ErrUnsupported }
func (*PTY) Write([]byte) (int, error)                             { return 0, ErrUnsupported }
func (*PTY) SetSize(shenguard.Dimensions) error                    { return ErrUnsupported }
func (*PTY) Wait() (int, error)                                    { return -1, ErrUnsupported }
func (*PTY) SignalGroup(syscall.Signal) error                      { return ErrUnsupported }
func (*PTY) Close() error                                          { return nil }
