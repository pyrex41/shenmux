package operator

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// ResolveBin locates a demo binary by name. Resolution order:
//  1. $MUXWORK_BIN/<name>            (explicit override dir; used by demo/tests)
//  2. <dir of muxwork executable>/<name>  (siblings, e.g. orchestrator/bin/)
//  3. ./bin/<name> relative to cwd
//  4. PATH
//
// Returns an absolute path, or "" if not found.
func ResolveBin(name string) string {
	try := func(p string) string {
		if p == "" {
			return ""
		}
		if isExecutable(p) {
			if abs, err := filepath.Abs(p); err == nil {
				return abs
			}
			return p
		}
		return ""
	}
	if d := os.Getenv("MUXWORK_BIN"); d != "" {
		if p := try(filepath.Join(d, name)); p != "" {
			return p
		}
	}
	if exe, err := os.Executable(); err == nil {
		if p := try(filepath.Join(filepath.Dir(exe), name)); p != "" {
			return p
		}
	}
	if p := try(filepath.Join("bin", name)); p != "" {
		return p
	}
	if p, err := exec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	return ""
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode()&0o111 != 0
}

// ProcessAlive reports whether a process with pid is currently alive (signal 0
// probe). pid <= 0 is treated as not alive.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM means the process exists but is owned by someone else.
	return errors.Is(err, syscall.EPERM)
}

// TerminateProcess sends SIGTERM, waits up to grace for the process to exit,
// then SIGKILLs it. It is a no-op for a pid that is already gone.
func TerminateProcess(pid int, grace time.Duration) {
	if pid <= 0 || !ProcessAlive(pid) {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !ProcessAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = proc.Signal(syscall.SIGKILL)
	// Reap briefly in case it is our child (best effort).
	waitDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(waitDeadline) {
		if !ProcessAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// detach returns SysProcAttr that puts the child in its own session so it
// survives the short-lived muxwork process and has no controlling terminal.
func detach() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// startManaged starts cmd and returns a monitor whose done channel closes when
// the process exits. The Wait goroutine prevents zombies while muxwork is still
// running; once muxwork exits the (now-orphaned) child continues under init.
type managedProc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func startManaged(cmd *exec.Cmd) (*managedProc, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	mp := &managedProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(mp.done)
	}()
	return mp, nil
}

// aliveAfter reports whether the process is still running after d. It is used to
// distinguish a backend that launched cleanly from one that died immediately
// (e.g. shenmux failing without a controlling TTY).
func (mp *managedProc) aliveAfter(d time.Duration) bool {
	select {
	case <-mp.done:
		return false
	case <-time.After(d):
		return true
	}
}
