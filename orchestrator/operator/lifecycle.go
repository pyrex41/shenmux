package operator

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// checkpoint invokes `snapshot checkpoint` for a worker's workspace, chaining
// onto its previous manifest if any. It returns the new manifest path.
func checkpoint(w Worker) (string, error) {
	snapBin := ResolveBin("snapshot")
	if snapBin == "" {
		return "", fmt.Errorf("snapshot binary not found")
	}
	if err := os.MkdirAll(StoreDir(w.State), 0o755); err != nil {
		return "", err
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	manifest := filepath.Join(w.ManifestsDir(), ts+".sexpr")

	args := []string{
		"checkpoint",
		"--workspace", w.Workspace(),
		"--store", StoreDir(w.State),
		"--manifest", manifest,
	}
	if parent, err := w.LatestManifest(); err == nil && parent != "" {
		args = append(args, "--parent", parent)
	}
	cmd := exec.Command(snapBin, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("snapshot checkpoint: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return manifest, nil
}

// Suspend checkpoints a worker, stops its processes, and keeps all state.
func Suspend(state, name string) (*Status, error) {
	w := NewWorker(state, name)
	st, err := w.ReadStatus()
	if err != nil {
		return nil, fmt.Errorf("worker %q: %w", name, err)
	}

	manifest, err := checkpoint(w)
	if err != nil {
		return st, err
	}
	st.LastCheckpoint = manifest
	st.Steps = append(st.Steps, "checkpoint: "+manifest)
	_ = w.EmitEvent(EventCheckpointed, manifest)

	// Stop worker + proxy.
	TerminateProcess(st.PID, 3*time.Second)
	TerminateProcess(st.ProxyPID, 3*time.Second)
	st.Steps = append(st.Steps, fmt.Sprintf("terminated worker pid=%d and proxy pid=%d", st.PID, st.ProxyPID))
	st.Phase = PhaseSuspended
	st.PID = 0
	st.ProxyPID = 0

	if err := w.WriteStatus(st); err != nil {
		return st, err
	}
	_ = w.EmitEvent(EventSuspended, manifest)
	return st, nil
}

// Resume re-runs the launch steps for a suspended worker (new proxy port),
// leaving the workspace intact.
func Resume(state, name string) (*Status, error) {
	w := NewWorker(state, name)
	st, err := w.ReadStatus()
	if err != nil {
		return nil, fmt.Errorf("worker %q: %w", name, err)
	}
	if st.Phase == PhaseRunning && ProcessAlive(st.PID) {
		return st, fmt.Errorf("worker %q is already running (pid %d)", name, st.PID)
	}

	cfg := SpawnConfig{
		State:      state,
		Name:       name,
		Harness:    st.Harness,
		MockTarget: st.MockTarget,
		MockSteps:  st.MockSteps,
	}
	oc, err := launchWorker(w, cfg)
	st.Steps = append(st.Steps, "resume:")
	st.Steps = append(st.Steps, oc.steps...)
	if err != nil {
		st.Phase = PhaseFailed
		_ = w.WriteStatus(st)
		_ = w.EmitEvent(EventFailed, err.Error())
		return st, err
	}
	st.ProxyPID = oc.proxyPID
	st.ProxyPort = oc.proxyPort
	st.SessionBackend = oc.backend
	st.PID = oc.workerPID
	st.Phase = PhaseRunning
	st.CompletedAt = 0

	if err := w.WriteStatus(st); err != nil {
		return st, err
	}
	_ = w.EmitEvent(EventResumed, map[string]interface{}{
		"pid":             st.PID,
		"proxy_port":      st.ProxyPort,
		"session_backend": st.SessionBackend,
	})
	return st, nil
}

// Fork creates a new worker whose workspace is an O(delta) restore of SRC's
// latest checkpoint, then spawns it. If SRC is running it is checkpointed first.
// mockTarget/mockSteps override the source's harness settings when non-empty,
// so forks can diverge (e.g. different step counts). Empty inherits from SRC.
func Fork(state, newName, srcName, prompt, mockTarget, mockSteps string) (*Status, error) {
	if newName == "" || srcName == "" {
		return nil, fmt.Errorf("fork requires --name and --from")
	}
	src := NewWorker(state, srcName)
	if !src.Exists() {
		return nil, fmt.Errorf("source worker %q does not exist", srcName)
	}
	srcSt, err := src.ReadStatus()
	if err != nil {
		return nil, fmt.Errorf("read source status: %w", err)
	}

	// Ensure SRC has at least one checkpoint; if running, checkpoint it now.
	manifest, err := src.LatestManifest()
	if err != nil {
		return nil, err
	}
	if manifest == "" {
		if srcSt.Phase == PhaseRunning && ProcessAlive(srcSt.PID) {
			m, cerr := checkpoint(src)
			if cerr != nil {
				return nil, fmt.Errorf("checkpoint running source: %w", cerr)
			}
			manifest = m
			srcSt.LastCheckpoint = m
			_ = src.WriteStatus(srcSt)
			_ = src.EmitEvent(EventCheckpointed, m)
		} else {
			return nil, fmt.Errorf("source worker %q has no checkpoint to fork (suspend it first)", srcName)
		}
	}

	dst := NewWorker(state, newName)
	if dst.Exists() {
		return nil, fmt.Errorf("worker %q already exists", newName)
	}
	if err := dst.EnsureDirs(); err != nil {
		return nil, err
	}

	// snapshot fork: restore SRC's manifest into the new workspace under a new id.
	snapBin := ResolveBin("snapshot")
	if snapBin == "" {
		return nil, fmt.Errorf("snapshot binary not found")
	}
	if err := os.MkdirAll(StoreDir(state), 0o755); err != nil {
		return nil, err
	}
	cmd := exec.Command(snapBin, "fork",
		"--manifest", manifest,
		"--store", StoreDir(state),
		"--into", dst.Workspace(),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("snapshot fork: %v: %s", err, strings.TrimSpace(string(out)))
	}

	// Spawn the new worker with its parent recorded.
	st, err := Spawn(SpawnConfig{
		State:      state,
		Name:       newName,
		Harness:    srcSt.Harness,
		Prompt:     prompt,
		Parent:     srcName,
		MockTarget: orDefault(mockTarget, srcSt.MockTarget),
		MockSteps:  orDefault(mockSteps, srcSt.MockSteps),
	})
	if err != nil {
		return st, err
	}
	_ = dst.EmitEvent(EventForked, map[string]interface{}{
		"from":         srcName,
		"src_manifest": manifest,
	})
	return st, nil
}

// RefreshPhase reconciles a worker's recorded phase with process liveness. A
// Running worker whose pid has exited becomes Completed (emitting a completed
// event once). Returns the possibly-updated status.
func RefreshPhase(w Worker) (*Status, error) {
	st, err := w.ReadStatus()
	if err != nil {
		return nil, err
	}
	if st.Phase == PhaseRunning && !ProcessAlive(st.PID) {
		st.Phase = PhaseCompleted
		st.CompletedAt = time.Now().Unix()
		_ = w.WriteStatus(st)
		_ = w.EmitEvent(EventCompleted, map[string]interface{}{"pid": st.PID})
	}
	return st, nil
}

// GC removes workers whose phase is Completed and whose completion (or creation,
// if unknown) is older than ttl.
func GC(state string, ttl time.Duration) ([]string, error) {
	names, err := ListWorkers(state)
	if err != nil {
		return nil, err
	}
	var removed []string
	now := time.Now()
	for _, name := range names {
		w := NewWorker(state, name)
		st, err := RefreshPhase(w)
		if err != nil {
			continue
		}
		if st.Phase != PhaseCompleted {
			continue
		}
		ref := st.CompletedAt
		if ref == 0 {
			ref = st.CreatedAt
		}
		age := now.Sub(time.Unix(ref, 0))
		if age < ttl {
			continue
		}
		if err := os.RemoveAll(w.Dir()); err != nil {
			return removed, fmt.Errorf("remove %s: %w", name, err)
		}
		removed = append(removed, name)
	}
	return removed, nil
}
