package operator

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// healthWindow is how long a foreground session backend must stay alive after
// Start before we accept it. Short enough to keep spawn snappy, long enough to
// catch an immediate "no TTY" style failure.
const healthWindow = 400 * time.Millisecond

// sessionResult describes a successfully launched worker session.
type sessionResult struct {
	backend string // BackendShenmux | BackendTmux | BackendDirect
	pid     int    // the worker process pid (pane pid under tmux)
}

// errSkipBackend signals that a backend is unavailable and the selector should
// move on without treating it as a failure.
var errSkipBackend = errors.New("backend unavailable")

// sessionLauncher is one candidate way to run the worker session.
type sessionLauncher struct {
	name   string
	launch func() (int, error) // returns pid, or errSkipBackend to skip, or a real error to fall through
}

// selectSession tries launchers in order and returns the first that starts a
// session. Launchers returning errSkipBackend are skipped; launchers returning
// any other error are recorded and the next is tried. This pure ordering logic
// is what the unit tests exercise with fake launchers.
func selectSession(launchers []sessionLauncher) (sessionResult, []string, error) {
	var steps []string
	var lastErr error
	for _, l := range launchers {
		pid, err := l.launch()
		switch {
		case errors.Is(err, errSkipBackend):
			steps = append(steps, fmt.Sprintf("session backend %s: skipped (unavailable)", l.name))
		case err != nil:
			steps = append(steps, fmt.Sprintf("session backend %s: failed (%v)", l.name, err))
			lastErr = err
		default:
			steps = append(steps, fmt.Sprintf("session backend %s: started pid=%d", l.name, pid))
			return sessionResult{backend: l.name, pid: pid}, steps, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no session backend available")
	}
	return sessionResult{}, steps, lastErr
}

// launchSpec carries everything the concrete launchers need.
type launchSpec struct {
	name       string
	worker     Worker
	harnessCmd []string // resolved harness argv
	env        []string // full child environment
	shenmuxBin string   // "" if unavailable
	tmuxBin    string   // "" if unavailable
}

// buildLaunchers wires the three real backends in priority order:
// shenmux (preferred) -> tmux -> direct.
func buildLaunchers(spec launchSpec) []sessionLauncher {
	return []sessionLauncher{
		{name: BackendShenmux, launch: func() (int, error) { return launchShenmux(spec) }},
		{name: BackendTmux, launch: func() (int, error) { return launchTmux(spec) }},
		{name: BackendDirect, launch: func() (int, error) { return launchDirect(spec) }},
	}
}

// openSessionLog opens (truncating) the worker's session.log for combined output.
func openSessionLog(w Worker) (*os.File, error) {
	return os.OpenFile(w.SessionLog(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
}

// launchShenmux runs the harness under a shenmux PTY session. shenmux owns a
// PTY; in a headless context it may fail or die immediately, in which case we
// return a real error so the selector falls through to tmux.
func launchShenmux(spec launchSpec) (int, error) {
	if spec.shenmuxBin == "" {
		return 0, errSkipBackend
	}
	logf, err := openSessionLog(spec.worker)
	if err != nil {
		return 0, err
	}
	args := []string{
		"run",
		"--session", spec.name,
		"--history-dir", spec.worker.HistoryDir(),
		"--keepalive=false",
		"--",
	}
	args = append(args, spec.harnessCmd...)
	cmd := exec.Command(spec.shenmuxBin, args...)
	cmd.Env = spec.env
	cmd.Dir = spec.worker.Workspace()
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = detach()
	mp, err := startManaged(cmd)
	if err != nil {
		logf.Close()
		return 0, err
	}
	if !mp.aliveAfter(healthWindow) {
		logf.Close()
		return 0, fmt.Errorf("shenmux session exited immediately (likely no controlling TTY)")
	}
	// Leak logf intentionally: the child holds the fd for its lifetime.
	return cmd.Process.Pid, nil
}

// launchTmux runs the harness inside a detached tmux session. tmux
// new-session -d returns immediately once the session exists, so success is
// verified via has-session and the worker pid is read from the pane.
func launchTmux(spec launchSpec) (int, error) {
	if spec.tmuxBin == "" {
		return 0, errSkipBackend
	}
	sessName := "mux-" + spec.name
	// Kill any stale session with the same name first (best effort).
	_ = exec.Command(spec.tmuxBin, "kill-session", "-t", sessName).Run()

	shellCmd := fmt.Sprintf("exec %s >> %s 2>&1",
		shellJoin(spec.harnessCmd), shellQuote(spec.worker.SessionLog()))
	cmd := exec.Command(spec.tmuxBin, "new-session", "-d", "-s", sessName, "sh", "-c", shellCmd)
	cmd.Env = spec.env
	cmd.Dir = spec.worker.Workspace()
	cmd.SysProcAttr = detach()
	if out, err := cmd.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("tmux new-session: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// Confirm the session came up.
	if err := exec.Command(spec.tmuxBin, "has-session", "-t", sessName).Run(); err != nil {
		return 0, fmt.Errorf("tmux session %s did not start", sessName)
	}
	pid := tmuxPanePID(spec.tmuxBin, sessName)
	return pid, nil
}

// launchDirect runs the harness process directly with output to session.log.
// This is the always-available last resort.
func launchDirect(spec launchSpec) (int, error) {
	if len(spec.harnessCmd) == 0 {
		return 0, errors.New("empty harness command")
	}
	logf, err := openSessionLog(spec.worker)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(spec.harnessCmd[0], spec.harnessCmd[1:]...)
	cmd.Env = spec.env
	cmd.Dir = spec.worker.Workspace()
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = detach()
	mp, err := startManaged(cmd)
	if err != nil {
		logf.Close()
		return 0, err
	}
	if !mp.aliveAfter(healthWindow) {
		logf.Close()
		return 0, fmt.Errorf("harness exited immediately")
	}
	return cmd.Process.Pid, nil
}

// tmuxPanePID returns the pid of the process running in the session's first
// pane, or 0 if it cannot be determined.
func tmuxPanePID(tmuxBin, sess string) int {
	out, err := exec.Command(tmuxBin, "list-panes", "-t", sess, "-F", "#{pane_pid}").Output()
	if err != nil {
		return 0
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	var pid int
	if _, err := fmt.Sscan(line, &pid); err != nil {
		return 0
	}
	return pid
}

// shellQuote single-quotes s for safe use in an sh -c string.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellJoin quotes and space-joins an argv for an sh -c string.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}
