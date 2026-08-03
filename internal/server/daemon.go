package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/ptyx"
	"github.com/pyrex41/shenmux/internal/shenguard"
	"github.com/pyrex41/shenmux/internal/term"
	"github.com/pyrex41/shenmux/internal/zmqx"
)

type Config struct {
	Session         string
	ControlEndpoint string
	DataEndpoint    string
	Dimensions      shenguard.Dimensions
	Command         []string
	Env             []string
	ExitGrace       time.Duration
	StoreLimits     protocol.StoreLimits
	ControlLease    time.Duration
}

type processResult struct {
	code int
	err  error
}

// Serve owns one command, one authoritative PTY/VT state, and the two ZeroMQ
// planes until the command exits or the context is canceled.
func Serve(ctx context.Context, cfg Config) error {
	if cfg.Session == "" || cfg.ControlEndpoint == "" || cfg.DataEndpoint == "" {
		return errors.New("session and endpoints must not be empty")
	}
	if cfg.ControlEndpoint == cfg.DataEndpoint {
		return errors.New("control and data endpoints must differ")
	}
	if len(cfg.Command) == 0 {
		return errors.New("command must not be empty")
	}
	if cfg.Dimensions.IsZero() {
		return errors.New("dimensions must be positive")
	}
	if cfg.Env == nil {
		cfg.Env = os.Environ()
	}
	if cfg.ExitGrace <= 0 {
		cfg.ExitGrace = 150 * time.Millisecond
	}
	releaseControlLock, err := acquireIPCLock(cfg.ControlEndpoint)
	if err != nil {
		return err
	}
	defer releaseControlLock()
	releaseDataLock, err := acquireIPCLock(cfg.DataEndpoint)
	if err != nil {
		return err
	}
	defer releaseDataLock()

	cleanupControl, err := prepareIPC(cfg.ControlEndpoint)
	if err != nil {
		return err
	}
	defer cleanupControl()
	cleanupData, err := prepareIPC(cfg.DataEndpoint)
	if err != nil {
		return err
	}
	defer cleanupData()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	zctx, err := zmqx.NewContext()
	if err != nil {
		return err
	}
	defer zctx.Close()

	publisher, err := NewZMQPublisher(runCtx, zctx, cfg.DataEndpoint, cfg.Session)
	if err != nil {
		return fmt.Errorf("start publisher: %w", err)
	}
	defer publisher.Close()

	pty, err := ptyx.Start(cfg.Command, cfg.Env, cfg.Dimensions)
	if err != nil {
		return err
	}
	defer pty.Close()
	processDone := make(chan processResult, 1)
	go func() {
		code, err := pty.Wait()
		processDone <- processResult{code: code, err: err}
	}()
	var proc *processResult
	defer func() {
		if proc == nil {
			result := stopAndWaitPTY(pty, processDone)
			proc = &result
		}
	}()

	terminal, err := term.New(cfg.Dimensions)
	if err != nil {
		return fmt.Errorf("create terminal: %w", err)
	}
	defer terminal.Close()
	runtime, err := NewRuntime(RuntimeConfig{
		Session: cfg.Session, Dimensions: cfg.Dimensions, PTY: pty,
		Terminal: terminal, Publisher: publisher,
		StoreLimits: cfg.StoreLimits, ControlLease: cfg.ControlLease,
	})
	if err != nil {
		return err
	}
	defer runtime.Close()
	control, err := NewControlServer(runCtx, zctx, cfg.ControlEndpoint, cfg.Session, runtime, publisher)
	if err != nil {
		return fmt.Errorf("start control server: %w", err)
	}
	defer control.Close()

	readDone := make(chan error, 1)
	go func() { readDone <- copyPTY(runtime, pty) }()

	var readErr error
	readFinished := false
	writerErrors := runtime.WriterErrors()
	publisherErrors := runtime.PublisherErrors()
	fatalErrors := runtime.FatalErrors()
	for proc == nil || !readFinished {
		select {
		case <-ctx.Done():
			result := stopAndWaitPTY(pty, processDone)
			proc = &result
			if !readFinished {
				readErr = <-readDone
				readFinished = true
			}
			return nil
		case result := <-processDone:
			proc = &result
		case err := <-readDone:
			readErr = err
			readFinished = true
			if err != nil {
				return err
			}
		case err, ok := <-control.Errors():
			if ok && err != nil {
				return fmt.Errorf("control server: %w", err)
			}
		case err, ok := <-writerErrors:
			if !ok {
				writerErrors = nil
				continue
			}
			if err != nil {
				return fmt.Errorf("PTY writer: %w", err)
			}
		case err, ok := <-publisherErrors:
			if !ok {
				publisherErrors = nil
				continue
			}
			if err != nil {
				return fmt.Errorf("state publisher: %w", err)
			}
		case err := <-fatalErrors:
			if err != nil {
				return fmt.Errorf("runtime consistency failure: %w", err)
			}
		}
	}
	if proc.err != nil {
		return proc.err
	}
	if readErr != nil {
		return readErr
	}
	if err := runtime.HandleExit(proc.code); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case <-time.After(cfg.ExitGrace):
	}
	return nil
}

func stopAndWaitPTY(pty *ptyx.PTY, done <-chan processResult) processResult {
	signalErr := pty.SignalGroup(syscall.SIGHUP)
	select {
	case result := <-done:
		if result.err == nil && signalErr != nil {
			result.err = signalErr
		}
		return result
	case <-time.After(250 * time.Millisecond):
		if err := pty.SignalGroup(syscall.SIGKILL); signalErr == nil {
			signalErr = err
		}
		result := <-done
		if result.err == nil && signalErr != nil {
			result.err = signalErr
		}
		return result
	}
}

func copyPTY(runtime *Runtime, pty io.Reader) error {
	buffer := make([]byte, 32<<10)
	for {
		n, err := pty.Read(buffer)
		if n > 0 {
			if handleErr := runtime.HandlePTYOutput(buffer[:n]); handleErr != nil {
				return handleErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read PTY: %w", err)
		}
	}
}

func filesystemIPCPath(endpoint string) (string, bool, error) {
	if !strings.HasPrefix(endpoint, "ipc://") {
		return "", false, nil
	}
	path := strings.TrimPrefix(endpoint, "ipc://")
	if path == "" {
		return "", false, errors.New("IPC endpoint path must not be empty")
	}
	if strings.HasPrefix(path, "@") {
		return "", false, nil
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return "", false, fmt.Errorf("IPC endpoint path must be absolute: %q", endpoint)
	}
	return path, true, nil
}

func acquireIPCLock(endpoint string) (func(), error) {
	path, filesystem, err := filesystemIPCPath(endpoint)
	if err != nil || !filesystem {
		return func() {}, err
	}
	parent := filepath.Dir(path)
	if err := ensurePrivateIPCDirectory(parent); err != nil {
		return nil, err
	}
	lockPath := path + ".lock"
	if info, err := os.Lstat(lockPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("IPC lock must not be a symlink: %q", lockPath)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect IPC lock: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open IPC lock %q: %w", lockPath, err)
	}
	closeLock := func() { _ = lock.Close() }
	if err := lock.Chmod(0o600); err != nil {
		closeLock()
		return nil, fmt.Errorf("secure IPC lock %q: %w", lockPath, err)
	}
	info, err := lock.Stat()
	if err != nil {
		closeLock()
		return nil, fmt.Errorf("inspect IPC lock %q: %w", lockPath, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() || !info.Mode().IsRegular() {
		closeLock()
		return nil, fmt.Errorf("IPC lock %q must be a regular file owned by uid %d", lockPath, os.Geteuid())
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeLock()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("IPC endpoint is already owned by another daemon: %q", path)
		}
		return nil, fmt.Errorf("lock IPC endpoint %q: %w", path, err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
		})
	}, nil
}

func prepareIPC(endpoint string) (func(), error) {
	path, filesystem, err := filesystemIPCPath(endpoint)
	if err != nil || !filesystem {
		return func() {}, err
	}
	parent := filepath.Dir(path)
	if err := ensurePrivateIPCDirectory(parent); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale IPC socket: %w", err)
	}
	return func() { _ = os.Remove(path) }, nil
}

func ensurePrivateIPCDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create IPC directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect IPC directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("IPC parent must be a real directory: %q", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect IPC directory ownership: %q", path)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("IPC directory %q is owned by uid %d, want %d", path, stat.Uid, os.Geteuid())
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("IPC directory %q must not grant group/other access (mode %04o)", path, info.Mode().Perm())
	}
	return nil
}
