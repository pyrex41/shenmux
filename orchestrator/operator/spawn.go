package operator

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Defaults for the mock harness environment.
const (
	DefaultMockTarget = "https://api.anthropic.com/v1/demo"
	DefaultMockSteps  = "3"
)

// SpawnConfig parameterizes a spawn.
type SpawnConfig struct {
	State      string
	Name       string
	Harness    string
	Prompt     string
	Parent     string
	MockTarget string
	MockSteps  string
}

// harnessCommand maps a harness name to an argv. mock -> bin/mock-agent; known
// CLIs are used if on PATH, else fall back to mock with a warning line.
func harnessCommand(name string) (argv []string, warning string, err error) {
	switch name {
	case "", "mock":
		bin := ResolveBin("mock-agent")
		if bin == "" {
			return nil, "", fmt.Errorf("mock-agent binary not found (looked in $MUXWORK_BIN, exe dir, ./bin, PATH)")
		}
		return []string{bin}, "", nil
	case "codex", "pi", "opencode":
		if p, lookErr := exec.LookPath(name); lookErr == nil {
			return []string{p}, "", nil
		}
		bin := ResolveBin("mock-agent")
		if bin == "" {
			return nil, "", fmt.Errorf("%s not on PATH and mock-agent fallback not found", name)
		}
		return []string{bin}, fmt.Sprintf("harness %q not on PATH; falling back to mock-agent", name), nil
	default:
		bin := ResolveBin("mock-agent")
		if bin == "" {
			return nil, "", fmt.Errorf("unknown harness %q and mock-agent fallback not found", name)
		}
		return []string{bin}, fmt.Sprintf("unknown harness %q; falling back to mock-agent", name), nil
	}
}

// childEnv builds the environment for the worker harness: proxy pointing at the
// secret-proxy, the placeholder key, CA trust vars, workspace/home, and the
// mock-agent knobs.
func childEnv(w Worker, cfg SpawnConfig, proxyPort int) []string {
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", proxyPort)
	ca := w.ProxyCACert()
	target := cfg.MockTarget
	if target == "" {
		target = DefaultMockTarget
	}
	steps := cfg.MockSteps
	if steps == "" {
		steps = DefaultMockSteps
	}
	env := []string{
		"HTTPS_PROXY=" + proxyURL,
		"HTTP_PROXY=" + proxyURL,
		"https_proxy=" + proxyURL,
		"http_proxy=" + proxyURL,
		"ANTHROPIC_API_KEY=" + PlaceholderKey,
		"WORKSPACE=" + w.Workspace(),
		"HOME=" + w.Home(),
		"NODE_EXTRA_CA_CERTS=" + ca,
		"REQUESTS_CA_BUNDLE=" + ca,
		"SSL_CERT_FILE=" + ca,
		"MOCK_TARGET=" + target,
		"MOCK_STEPS=" + steps,
	}
	if cfg.Prompt != "" {
		env = append(env, "PROMPT="+cfg.Prompt)
	}
	// Preserve PATH and a couple of basics so child tooling still works.
	for _, k := range []string{"PATH", "LANG", "TERM", "TZ"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// launchOutcome captures the result of bringing a worker's processes up.
type launchOutcome struct {
	proxyPID  int
	proxyPort int
	backend   string
	workerPID int
	steps     []string
}

// launchWorker starts the secret-proxy then the worker session. It is shared by
// spawn and resume. It does NOT touch the workspace contents.
func launchWorker(w Worker, cfg SpawnConfig) (*launchOutcome, error) {
	oc := &launchOutcome{}
	step := func(format string, a ...interface{}) {
		oc.steps = append(oc.steps, fmt.Sprintf(format, a...))
	}

	harnessArgv, warn, err := harnessCommand(cfg.Harness)
	if err != nil {
		return oc, err
	}
	if warn != "" {
		step("warning: %s", warn)
	}
	step("harness command: %v", harnessArgv)

	// (1) secret-proxy on a free loopback port.
	proxyBin := ResolveBin("secret-proxy")
	if proxyBin == "" {
		return oc, fmt.Errorf("secret-proxy binary not found")
	}
	port, err := PickFreePort()
	if err != nil {
		return oc, fmt.Errorf("pick free port: %w", err)
	}
	step("picked free loopback port %d", port)

	proxyLog, err := os.OpenFile(w.ProxyLog(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return oc, err
	}
	proxyCmd := exec.Command(proxyBin,
		"--listen", fmt.Sprintf("127.0.0.1:%d", port),
		"--secrets", w.SecretsPath(),
		"--ca-dir", w.ProxyCADir(),
		"--log", w.ProxyLog(),
	)
	proxyCmd.Stdout = proxyLog
	proxyCmd.Stderr = proxyLog
	proxyCmd.SysProcAttr = detach()
	proxyMP, err := startManaged(proxyCmd)
	if err != nil {
		proxyLog.Close()
		return oc, fmt.Errorf("start secret-proxy: %w", err)
	}
	oc.proxyPID = proxyCmd.Process.Pid
	oc.proxyPort = port
	step("launched secret-proxy pid=%d", oc.proxyPID)

	// Wait for the proxy to accept connections (poll, not blind sleep).
	if err := waitForPort(port, 5*time.Second, func() bool {
		select {
		case <-proxyMP.done:
			return false
		default:
			return true
		}
	}); err != nil {
		TerminateProcess(oc.proxyPID, 2*time.Second)
		return oc, fmt.Errorf("secret-proxy did not come up: %w", err)
	}
	step("secret-proxy accepting connections on 127.0.0.1:%d", port)

	// (2) worker session with the child env.
	env := childEnv(w, cfg, port)
	spec := launchSpec{
		name:       w.Name,
		worker:     w,
		harnessCmd: harnessArgv,
		env:        env,
		shenmuxBin: ResolveBin("shenmux"),
		tmuxBin:    ResolveBin("tmux"),
	}
	res, sessSteps, err := selectSession(buildLaunchers(spec))
	oc.steps = append(oc.steps, sessSteps...)
	if err != nil {
		// Roll back the proxy so we don't leak it.
		TerminateProcess(oc.proxyPID, 2*time.Second)
		return oc, fmt.Errorf("no session backend could start the worker: %w", err)
	}
	oc.backend = res.backend
	oc.workerPID = res.pid
	step("worker session up via %s backend (pid=%d)", oc.backend, oc.workerPID)
	return oc, nil
}

// Spawn provisions and launches a new worker. It records every step and the
// chosen session backend into status.json.
func Spawn(cfg SpawnConfig) (*Status, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("spawn: --name is required")
	}
	w := NewWorker(cfg.State, cfg.Name)
	if w.Exists() {
		if st, err := w.ReadStatus(); err == nil && st.Phase == PhaseRunning && ProcessAlive(st.PID) {
			return nil, fmt.Errorf("worker %q already running (pid %d)", cfg.Name, st.PID)
		}
	}

	st := &Status{
		Name:       cfg.Name,
		Harness:    orDefault(cfg.Harness, "mock"),
		Phase:      PhasePending,
		Parent:     cfg.Parent,
		CreatedAt:  time.Now().Unix(),
		MockTarget: orDefault(cfg.MockTarget, DefaultMockTarget),
		MockSteps:  orDefault(cfg.MockSteps, DefaultMockSteps),
	}

	// Step 1: dirs + secrets.json.
	if err := w.EnsureDirs(); err != nil {
		return nil, fmt.Errorf("create worker dirs: %w", err)
	}
	st.Steps = append(st.Steps, "created worker directory layout")
	if _, err := os.Stat(w.SecretsPath()); os.IsNotExist(err) {
		if err := WriteSecrets(w.SecretsPath(), DefaultSecrets(cfg.Name)); err != nil {
			return nil, fmt.Errorf("write secrets.json: %w", err)
		}
		st.Steps = append(st.Steps, "wrote secrets.json (ANTHROPIC_API_KEY placeholder + demo value)")
	} else {
		st.Steps = append(st.Steps, "reused existing secrets.json")
	}
	// Persist a Pending status so state is observable even if launch fails.
	_ = w.WriteStatus(st)

	// Steps 2 & 3: proxy + session.
	oc, err := launchWorker(w, cfg)
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

	// Step 4: events.
	_ = w.EmitEvent(EventSpawned, map[string]interface{}{
		"harness":         st.Harness,
		"session_backend": st.SessionBackend,
		"proxy_port":      st.ProxyPort,
		"parent":          st.Parent,
	})
	_ = w.EmitEvent(EventRunning, map[string]interface{}{"pid": st.PID})

	// Step 5: status.json.
	if err := w.WriteStatus(st); err != nil {
		return st, fmt.Errorf("write status.json: %w", err)
	}
	return st, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
