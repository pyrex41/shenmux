package operator

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPickFreePort(t *testing.T) {
	port, err := PickFreePort()
	if err != nil {
		t.Fatalf("PickFreePort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("port out of range: %d", port)
	}
	// The port should be immediately bindable (it was released before return).
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatalf("picked port %d not bindable: %v", port, err)
	}
	l.Close()
}

func itoa(i int) string {
	// tiny local helper to avoid importing strconv just for the test
	if i == 0 {
		return "0"
	}
	var buf [12]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func TestDefaultSecretsGeneration(t *testing.T) {
	s := DefaultSecrets("alpha")
	entry, ok := s.Secrets["ANTHROPIC_API_KEY"]
	if !ok {
		t.Fatal("missing ANTHROPIC_API_KEY entry")
	}
	if entry.Placeholder != PlaceholderKey {
		t.Errorf("placeholder = %q, want %q", entry.Placeholder, PlaceholderKey)
	}
	if entry.Value != "sk-REAL-demo-alpha-key" {
		t.Errorf("value = %q, want sk-REAL-demo-alpha-key", entry.Value)
	}
	wantHosts := []string{"127.0.0.1", "localhost", "api.anthropic.com", ".anthropic.com"}
	if !reflect.DeepEqual(entry.AllowedHosts, wantHosts) {
		t.Errorf("allowed_hosts = %v, want %v", entry.AllowedHosts, wantHosts)
	}

	// Round-trip through disk.
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	if err := WriteSecrets(path, s); err != nil {
		t.Fatalf("WriteSecrets: %v", err)
	}
	var back SecretsFile
	if err := ReadJSONFile(path, &back); err != nil {
		t.Fatalf("ReadJSONFile: %v", err)
	}
	if !reflect.DeepEqual(back, s) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", back, s)
	}
	// bao_ref must be omitted from the demo secrets (omitempty).
	b, _ := os.ReadFile(path)
	if contains(string(b), "bao_ref") {
		t.Errorf("secrets.json unexpectedly contains bao_ref: %s", b)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	state := t.TempDir()
	w := NewWorker(state, "w1")
	if err := w.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	st := &Status{
		Name:           "w1",
		Harness:        "mock",
		Phase:          PhaseRunning,
		PID:            4242,
		ProxyPID:       4243,
		ProxyPort:      51000,
		SessionBackend: BackendTmux,
		CreatedAt:      time.Now().Unix(),
		LastCheckpoint: "/x/manifests/1.sexpr",
		Parent:         "base",
		Steps:          []string{"a", "b"},
	}
	if err := w.WriteStatus(st); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}
	got, err := w.ReadStatus()
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if !reflect.DeepEqual(got, st) {
		t.Errorf("status round-trip mismatch:\n got %+v\nwant %+v", got, st)
	}
}

func TestEventsAppendRead(t *testing.T) {
	state := t.TempDir()
	w := NewWorker(state, "w1")
	if err := w.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	seq := []struct {
		event  string
		detail interface{}
	}{
		{EventSpawned, map[string]interface{}{"proxy_port": 5000}},
		{EventRunning, nil},
		{EventSuspended, "/m/1.sexpr"},
	}
	for _, e := range seq {
		if err := w.EmitEvent(e.event, e.detail); err != nil {
			t.Fatalf("EmitEvent(%s): %v", e.event, err)
		}
	}
	evs, err := ReadEvents(w.EventsPath())
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(evs) != len(seq) {
		t.Fatalf("read %d events, want %d", len(evs), len(seq))
	}
	for i, e := range seq {
		if evs[i].Event != e.event {
			t.Errorf("event[%d] = %q, want %q", i, evs[i].Event, e.event)
		}
		if evs[i].Worker != "w1" {
			t.Errorf("event[%d] worker = %q, want w1", i, evs[i].Worker)
		}
		if evs[i].TS == 0 {
			t.Errorf("event[%d] has zero timestamp", i)
		}
	}
}

// TestSelectSessionOrdering exercises the pure backend-selection/fallback logic
// with fake launchers, without needing bin/shenmux.
func TestSelectSessionOrdering(t *testing.T) {
	cases := []struct {
		name        string
		launchers   []sessionLauncher
		wantBackend string
		wantErr     bool
	}{
		{
			name: "shenmux unavailable falls through to tmux",
			launchers: []sessionLauncher{
				{name: BackendShenmux, launch: func() (int, error) { return 0, errSkipBackend }},
				{name: BackendTmux, launch: func() (int, error) { return 111, nil }},
				{name: BackendDirect, launch: func() (int, error) { return 222, nil }},
			},
			wantBackend: BackendTmux,
		},
		{
			name: "shenmux errors, tmux unavailable, direct wins",
			launchers: []sessionLauncher{
				{name: BackendShenmux, launch: func() (int, error) { return 0, errors.New("no tty") }},
				{name: BackendTmux, launch: func() (int, error) { return 0, errSkipBackend }},
				{name: BackendDirect, launch: func() (int, error) { return 333, nil }},
			},
			wantBackend: BackendDirect,
		},
		{
			name: "shenmux wins when it starts",
			launchers: []sessionLauncher{
				{name: BackendShenmux, launch: func() (int, error) { return 999, nil }},
				{name: BackendTmux, launch: func() (int, error) { return 111, nil }},
			},
			wantBackend: BackendShenmux,
		},
		{
			name: "all unavailable is an error",
			launchers: []sessionLauncher{
				{name: BackendShenmux, launch: func() (int, error) { return 0, errSkipBackend }},
				{name: BackendTmux, launch: func() (int, error) { return 0, errSkipBackend }},
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, steps, err := selectSession(tc.launchers)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got backend %q", res.backend)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.backend != tc.wantBackend {
				t.Errorf("backend = %q, want %q", res.backend, tc.wantBackend)
			}
			if len(steps) == 0 {
				t.Error("expected recorded steps")
			}
		})
	}
}

// TestLaunchDirectRealProcess verifies the direct-launch path actually starts a
// process by pointing the harness at a real short-lived command, and that the
// selector records it as the "direct" backend when shenmux/tmux are absent.
func TestLaunchDirectRealProcess(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	state := t.TempDir()
	w := NewWorker(state, "d1")
	if err := w.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	spec := launchSpec{
		name:       "d1",
		worker:     w,
		harnessCmd: []string{sh, "-c", "echo hello; sleep 2"},
		env:        append(os.Environ(), "FOO=bar"),
		shenmuxBin: "", // force skip
		tmuxBin:    "", // force skip
	}
	res, steps, err := selectSession(buildLaunchers(spec))
	if err != nil {
		t.Fatalf("selectSession: %v", err)
	}
	if res.backend != BackendDirect {
		t.Fatalf("backend = %q, want direct", res.backend)
	}
	if res.pid <= 0 || !ProcessAlive(res.pid) {
		t.Fatalf("expected a live pid, got %d (alive=%v)", res.pid, ProcessAlive(res.pid))
	}
	if len(steps) == 0 {
		t.Error("expected steps recorded")
	}
	// The direct launcher writes combined output to session.log.
	TerminateProcess(res.pid, time.Second)
	data, _ := os.ReadFile(w.SessionLog())
	if !contains(string(data), "hello") {
		t.Errorf("session.log = %q, want to contain 'hello'", data)
	}
}

func TestResolveBinOverride(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "snapshot")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUXWORK_BIN", dir)
	got := ResolveBin("snapshot")
	if got != fake {
		t.Errorf("ResolveBin = %q, want %q", got, fake)
	}
	if ResolveBin("definitely-not-a-real-binary-xyz") != "" {
		t.Error("expected empty result for missing binary")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
