//go:build cgo && (linux || darwin)

package ptyx

/*
#cgo linux LDFLAGS: -lutil
#include <errno.h>
#include <signal.h>
#include <stdlib.h>
#include <sys/ioctl.h>
#include <sys/types.h>
#include <termios.h>
#include <unistd.h>
#ifdef __APPLE__
#include <util.h>
#else
#include <pty.h>
#endif

static int shenmux_openpty(int *master, int *slave, unsigned short cols, unsigned short rows) {
    struct winsize ws;
    ws.ws_row = rows;
    ws.ws_col = cols;
    ws.ws_xpixel = 0;
    ws.ws_ypixel = 0;
    if (openpty(master, slave, NULL, NULL, &ws) < 0) return errno;
    return 0;
}

static int shenmux_set_winsize(int fd, unsigned short cols, unsigned short rows) {
    struct winsize ws;
    ws.ws_row = rows;
    ws.ws_col = cols;
    ws.ws_xpixel = 0;
    ws.ws_ypixel = 0;
    if (ioctl(fd, TIOCSWINSZ, &ws) < 0) return errno;
    return 0;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/pyrex41/shenmux/internal/shenguard"
)

// PTY owns the master side and the child process attached to the slave side.
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
	var masterFD, slaveFD C.int
	if errno := C.shenmux_openpty(&masterFD, &slaveFD, C.ushort(dim.Cols()), C.ushort(dim.Rows())); errno != 0 {
		return nil, fmt.Errorf("openpty: %w", syscall.Errno(errno))
	}
	master := os.NewFile(uintptr(masterFD), "shenmux-pty-master")
	slave := os.NewFile(uintptr(slaveFD), "shenmux-pty-slave")
	if master == nil || slave == nil {
		if master != nil {
			_ = master.Close()
		}
		if slave != nil {
			_ = slave.Close()
		}
		return nil, errors.New("could not wrap PTY file descriptors")
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0, // stdin in the child after os/exec remaps descriptors
	}
	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		_ = master.Close()
		return nil, fmt.Errorf("start PTY command: %w", err)
	}
	// The parent must not retain the slave; otherwise EOF on the master is
	// delayed after the child exits.
	if err := slave.Close(); err != nil {
		_ = master.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return nil, fmt.Errorf("close PTY slave in parent: %w", err)
	}
	return &PTY{master: master, cmd: cmd}, nil
}

func (p *PTY) Read(buf []byte) (int, error) {
	n, err := p.master.Read(buf)
	// Linux PTYs commonly signal slave closure as EIO rather than a zero-byte
	// read. Treat it as ordinary EOF for the session loop.
	if errors.Is(err, syscall.EIO) {
		return n, io.EOF
	}
	return n, err
}

func (p *PTY) Write(buf []byte) (int, error) { return p.master.Write(buf) }

func (p *PTY) SetSize(dim shenguard.Dimensions) error {
	if errno := C.shenmux_set_winsize(C.int(p.master.Fd()), C.ushort(dim.Cols()), C.ushort(dim.Rows())); errno != 0 {
		return fmt.Errorf("set PTY size: %w", syscall.Errno(errno))
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
		// A non-zero exit is session state, not an infrastructure error. Keep
		// only errors that aren't ExitError values.
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
	// Start used Setsid, so the child PID is also its process-group ID. Do
	// not inspect Cmd.ProcessState here: Wait writes it concurrently, while a
	// signal to an already-reaped group safely returns ESRCH.
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
