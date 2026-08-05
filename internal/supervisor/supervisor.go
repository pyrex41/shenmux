// Package supervisor owns the local lifecycle of shenmux session processes.
// It is intentionally small: a controller/agent can use it to create and
// stop sessions without knowing how PTYs or IPC sockets are implemented.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/pyrex41/shenmux/internal/naming"
)

type Spec struct {
	Name       string
	Binary     string
	Command    []string
	Workspace  string // working directory exposed to the child session
	Shell      string
	StateDir   string
	HistoryDir string
	Control    string
	Data       string
}

type Session struct {
	Name      string
	PID       int
	StartedAt time.Time
	Command   []string
}

type Handle struct {
	Session
	cmd  *exec.Cmd
	done chan error
}

func (h *Handle) Done() <-chan error { return h.done }

func (h *Handle) Stop() error {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return nil
	}
	return h.cmd.Process.Signal(os.Interrupt)
}

type Manager struct {
	mu     sync.Mutex
	byName map[string]*Handle
}

func New() *Manager { return &Manager{byName: make(map[string]*Handle)} }

func (m *Manager) Start(ctx context.Context, spec Spec) (*Handle, error) {
	if m == nil {
		return nil, errors.New("nil session manager")
	}
	if err := naming.ValidateSession(spec.Name); err != nil {
		return nil, err
	}
	if spec.Binary == "" {
		return nil, errors.New("session binary must not be empty")
	}
	m.mu.Lock()
	if _, exists := m.byName[spec.Name]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("session %q already exists", spec.Name)
	}
	m.mu.Unlock()
	args := []string{"run", "-session", spec.Name}
	// Keep the wire command deterministic; this also makes audit logs and
	// supervisor tests reproducible.
	for _, option := range []struct{ flag, value string }{{"-state-dir", spec.StateDir}, {"-history-dir", spec.HistoryDir}, {"-control", spec.Control}, {"-data", spec.Data}, {"-shell", spec.Shell}} {
		flag, value := option.flag, option.value
		if value != "" {
			args = append(args, flag, value)
		}
	}
	if len(spec.Command) > 0 {
		args = append(args, "--")
		args = append(args, spec.Command...)
	}
	cmd := exec.CommandContext(ctx, spec.Binary, args...)
	cmd.Env = os.Environ()
	if spec.Workspace != "" {
		cmd.Dir = spec.Workspace
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start session %q: %w", spec.Name, err)
	}
	h := &Handle{Session: Session{Name: spec.Name, PID: cmd.Process.Pid, StartedAt: time.Now().UTC(), Command: append([]string(nil), spec.Command...)}, cmd: cmd, done: make(chan error, 1)}
	m.mu.Lock()
	m.byName[spec.Name] = h
	m.mu.Unlock()
	go func() {
		err := cmd.Wait()
		h.done <- err
		close(h.done)
		m.mu.Lock()
		if m.byName[spec.Name] == h {
			delete(m.byName, spec.Name)
		}
		m.mu.Unlock()
	}()
	return h, nil
}

func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	h := m.byName[name]
	m.mu.Unlock()
	if h == nil {
		return fmt.Errorf("session %q not found", name)
	}
	return h.Stop()
}

func (m *Manager) List() []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Session, 0, len(m.byName))
	for _, h := range m.byName {
		out = append(out, h.Session)
	}
	return out
}
