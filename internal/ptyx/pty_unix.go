//go:build linux || darwin

package ptyx

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty/v2"
	"github.com/pyrex41/shenmux/internal/shenguard"
	"golang.org/x/sys/unix"
)

// PTY owns the master side and the child process attached to the slave side.
// creack/pty uses platform syscalls directly and does not require CGO.
type PTY struct {
	master *os.File
	cmd    *exec.Cmd

	waitOnce sync.Once
	waitCode int
	waitErr  error
}

func Start(argv []string, env []string, dim shenguard.Dimensions) (*PTY, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("PTY command must not be empty")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(dim.Rows()), Cols: uint16(dim.Cols())})
	if err != nil {
		return nil, fmt.Errorf("start PTY command: %w", err)
	}
	return &PTY{master: master, cmd: cmd}, nil
}

func (p *PTY) Read(buf []byte) (int, error) {
	n, err := p.master.Read(buf)
	if errors.Is(err, syscall.EIO) {
		return n, io.EOF
	}
	return n, err
}

func (p *PTY) Write(buf []byte) (int, error) { return p.master.Write(buf) }

func (p *PTY) SetSize(dim shenguard.Dimensions) error {
	if err := pty.Setsize(p.master, &pty.Winsize{Rows: uint16(dim.Rows()), Cols: uint16(dim.Cols())}); err != nil {
		return fmt.Errorf("set PTY size: %w", err)
	}
	return nil
}

func (p *PTY) Wait() (int, error) {
	p.waitOnce.Do(func() {
		err := p.cmd.Wait()
		p.waitErr = err
		if p.cmd.ProcessState != nil {
			p.waitCode = p.cmd.ProcessState.ExitCode()
		} else {
			p.waitCode = -1
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			p.waitErr = nil
		}
	})
	return p.waitCode, p.waitErr
}

func (p *PTY) SignalGroup(signal syscall.Signal) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (p *PTY) Close() error {
	var errs []error
	if p.master != nil {
		if err := p.master.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			errs = append(errs, err)
		}
	}
	if err := p.SignalGroup(syscall.SIGHUP); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// EchoEnabled reports whether the line discipline is currently echoing input,
// by reading the master's termios ECHO bit. A shell echoes; a password prompt
// turns it off. Clients use this to know when predicting local echo is safe,
// so it must reflect the kernel's state rather than anything inferred from the
// escape stream.
func (p *PTY) EchoEnabled() (bool, error) {
	termios, err := unix.IoctlGetTermios(int(p.master.Fd()), getTermiosReq)
	if err != nil {
		return false, fmt.Errorf("read PTY termios: %w", err)
	}
	return termios.Lflag&unix.ECHO != 0, nil
}
