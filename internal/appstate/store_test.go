package appstate

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestResolvePathsPrecedence(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("SHENMUX_CONFIG_FILE", "")
	t.Setenv("SHENMUX_STATE_DIR", "")
	paths, err := ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if paths.ConfigFile != filepath.Join(root, "config", "shenmux", "config.json") {
		t.Fatalf("config file = %q", paths.ConfigFile)
	}
	if paths.StateDir != filepath.Join(root, "state", "shenmux") {
		t.Fatalf("state dir = %q", paths.StateDir)
	}

	t.Setenv("SHENMUX_CONFIG_FILE", filepath.Join(root, "custom.json"))
	t.Setenv("SHENMUX_STATE_DIR", filepath.Join(root, "custom-state"))
	paths, err = ResolvePaths()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(paths.ConfigFile, "custom.json") || !strings.HasSuffix(paths.StateDir, "custom-state") {
		t.Fatalf("explicit paths were not honored: %+v", paths)
	}
}

func TestRoundTripAndPrivatePermissions(t *testing.T) {
	root := t.TempDir()
	paths := Paths{ConfigFile: filepath.Join(root, "config", "config.json"), StateDir: filepath.Join(root, "state")}
	config := Config{Controller: "https://controller.example", Transport: "tailscale", DirectEndpoint: "tailscale://100.64.0.2:8788/ws"}
	if err := paths.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	loaded, err := paths.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Controller != config.Controller || loaded.Version != SchemaVersion || loaded.TrustMode != "trusted" || loaded.Transport != "tailscale" || loaded.DirectEndpoint == "" {
		t.Fatalf("loaded config = %+v", loaded)
	}
	if loaded.ReconnectInitial != "1s" || loaded.ReconnectMaximum != "1m" || loaded.ReconnectFactor != 2 || loaded.ReconnectJitter != .2 {
		t.Fatalf("reconnect defaults = %+v", loaded)
	}
	now := time.Now().UTC().Truncate(time.Second)
	state := State{Sessions: map[string]Session{"work": {Name: "work", StartedAt: now, PID: 42}}}
	if err := paths.SaveState(state); err != nil {
		t.Fatal(err)
	}
	loadedState, err := paths.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if loadedState.Sessions["work"].PID != 42 {
		t.Fatalf("loaded state = %+v", loadedState)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{paths.ConfigFile, paths.StateFile()} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("%s permissions = %o", path, info.Mode().Perm())
			}
		}
	}
}

func TestRejectsUnknownAndFutureSchema(t *testing.T) {
	root := t.TempDir()
	paths := Paths{ConfigFile: filepath.Join(root, "config.json"), StateDir: filepath.Join(root, "state")}
	if err := os.WriteFile(paths.ConfigFile, []byte(`{"version":1,"surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := paths.LoadConfig(); err == nil {
		t.Fatal("unknown field was accepted")
	}
	if err := os.WriteFile(paths.ConfigFile, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := paths.LoadConfig(); err == nil || !strings.Contains(err.Error(), "unsupported schema") {
		t.Fatalf("future schema error = %v", err)
	}
}

func TestExplicitZeroReconnectJitterPersists(t *testing.T) {
	root := t.TempDir()
	paths := Paths{ConfigFile: filepath.Join(root, "config.json"), StateDir: filepath.Join(root, "state")}
	config := Config{ReconnectInitial: "10ms", ReconnectMaximum: "1s", ReconnectFactor: 2, ReconnectJitter: 0}
	if err := paths.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	loaded, err := paths.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ReconnectJitter != 0 {
		t.Fatalf("explicit zero jitter was replaced: %+v", loaded)
	}
}
