//go:build cgo && (linux || darwin)

package ttyx

/*
#include <errno.h>
#include <stdlib.h>
#include <sys/ioctl.h>
#include <termios.h>
#include <unistd.h>

static int shenmux_make_raw(int fd, struct termios *saved) {
    if (tcgetattr(fd, saved) < 0) return errno;
    struct termios raw = *saved;
    cfmakeraw(&raw);
    if (tcsetattr(fd, TCSANOW, &raw) < 0) return errno;
    return 0;
}
static int shenmux_restore(int fd, const struct termios *saved) {
    if (tcsetattr(fd, TCSANOW, saved) < 0) return errno;
    return 0;
}
static int shenmux_get_winsize(int fd, unsigned short *cols, unsigned short *rows) {
    struct winsize ws;
    if (ioctl(fd, TIOCGWINSZ, &ws) < 0) return errno;
    *cols = ws.ws_col;
    *rows = ws.ws_row;
    return 0;
}
static int shenmux_isatty(int fd) { return isatty(fd); }
*/
import "C"

import (
	"fmt"
	"sync"
	"syscall"
)

type State struct {
	mu       sync.Mutex
	fd       int
	termios  C.struct_termios
	restored bool
}

func IsTerminal(fd uintptr) bool { return C.shenmux_isatty(C.int(fd)) == 1 }

func MakeRaw(fd uintptr) (*State, error) {
	state := &State{fd: int(fd)}
	if errno := C.shenmux_make_raw(C.int(fd), &state.termios); errno != 0 {
		return nil, fmt.Errorf("make terminal raw: %w", syscall.Errno(errno))
	}
	return state, nil
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
	if errno := C.shenmux_restore(C.int(s.fd), &s.termios); errno != 0 {
		return fmt.Errorf("restore terminal: %w", syscall.Errno(errno))
	}
	s.restored = true
	return nil
}

func Size(fd uintptr) (cols, rows int, err error) {
	var c, r C.ushort
	if errno := C.shenmux_get_winsize(C.int(fd), &c, &r); errno != 0 {
		return 0, 0, fmt.Errorf("get terminal size: %w", syscall.Errno(errno))
	}
	if c == 0 || r == 0 {
		return 0, 0, fmt.Errorf("terminal reported zero dimensions")
	}
	return int(c), int(r), nil
}
