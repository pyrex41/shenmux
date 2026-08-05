//go:build linux || darwin

package ttyx

import (
	"fmt"
	"sync"

	term "golang.org/x/term"
)

type State struct {
	mu       sync.Mutex
	fd       int
	term     *term.State
	restored bool
}

func IsTerminal(fd uintptr) bool { return term.IsTerminal(int(fd)) }

func MakeRaw(fd uintptr) (*State, error) {
	saved, err := term.MakeRaw(int(fd))
	if err != nil {
		return nil, fmt.Errorf("make terminal raw: %w", err)
	}
	return &State{fd: int(fd), term: saved}, nil
}

func (s *State) Restore() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.restored {
		return nil
	}
	if err := term.Restore(s.fd, s.term); err != nil {
		return fmt.Errorf("restore terminal: %w", err)
	}
	s.restored = true
	return nil
}

func Size(fd uintptr) (cols, rows int, err error) {
	cols, rows, err = term.GetSize(int(fd))
	if err != nil {
		return 0, 0, fmt.Errorf("get terminal size: %w", err)
	}
	if cols == 0 || rows == 0 {
		return 0, 0, fmt.Errorf("terminal reported zero dimensions")
	}
	return cols, rows, nil
}
