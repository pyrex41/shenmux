package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyrex41/shenmux/internal/appstate"
	"github.com/pyrex41/shenmux/internal/relay"
)

func TestCommandHelpIncludesV1Surface(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := execute(context.Background(), []string{"help"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"run", "login", "agent", "controller", "status", "web"} {
		if !strings.Contains(stdout.String(), command) {
			t.Fatalf("help does not contain %q:\n%s", command, stdout.String())
		}
	}
}

func TestLoginThenStatusJSON(t *testing.T) {
	root := t.TempDir()
	configFile := filepath.Join(root, "config", "config.json")
	stateDir := filepath.Join(root, "state")
	t.Setenv("SHENMUX_CONFIG_FILE", configFile)
	t.Setenv("SHENMUX_STATE_DIR", stateDir)
	var stdout, stderr bytes.Buffer
	if err := execute(context.Background(), []string{"login", "--controller", "https://controller.example/"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	paths := appstate.Paths{ConfigFile: configFile, StateDir: stateDir}
	config, err := paths.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.Controller != "https://controller.example" {
		t.Fatalf("controller = %q", config.Controller)
	}
	stdout.Reset()
	if err := execute(context.Background(), []string{"status", "--json"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Controller string `json:"controller"`
		TrustMode  string `json:"trust_mode"`
		StateDir   string `json:"state_dir"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Controller != "https://controller.example" || report.TrustMode != "trusted" || report.StateDir != stateDir {
		t.Fatalf("status report = %+v", report)
	}
}

func TestStatusReportsDirectTransportConfig(t *testing.T) {
	root := t.TempDir()
	configFile := filepath.Join(root, "config", "config.json")
	stateDir := filepath.Join(root, "state")
	t.Setenv("SHENMUX_CONFIG_FILE", configFile)
	t.Setenv("SHENMUX_STATE_DIR", stateDir)
	paths := appstate.Paths{ConfigFile: configFile, StateDir: stateDir}
	if err := paths.SaveConfig(appstate.Config{
		Controller:     "https://controller.example",
		Transport:      "auto",
		DirectEndpoint: "tailscale://100.64.0.2:8788",
		TailscalePeer:  "agent",
		TailscalePort:  8788,
		RequireDirect:  true,
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := execute(context.Background(), []string{"status", "--json"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Transport string `json:"transport"`
		Direct    string `json:"direct_endpoint"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Transport != "auto" || report.Direct != "tailscale://100.64.0.2:8788" {
		t.Fatalf("status report = %+v", report)
	}
}

func TestControllerURLPolicy(t *testing.T) {
	for _, invalid := range []string{"", "controller.example", "http://controller.example", "https://user:pass@controller.example", "https://controller.example?q=secret"} {
		if _, err := normalizeController(invalid); err == nil {
			t.Errorf("normalizeController(%q) succeeded", invalid)
		}
	}
	for _, valid := range []string{"https://controller.example", "http://localhost:8080", "http://127.0.0.1:8080"} {
		if _, err := normalizeController(valid); err != nil {
			t.Errorf("normalizeController(%q): %v", valid, err)
		}
	}
}

func TestNormalizeDirectWebSocketURL(t *testing.T) {
	tests := map[string]string{
		"tailscale://100.64.0.2:8788":       "ws://100.64.0.2:8788/ws",
		"wireguard://peer.internal:8788/ws": "ws://peer.internal:8788/ws",
		"wss://host.example/custom":         "wss://host.example/custom",
	}
	for input, want := range tests {
		got, err := normalizeDirectWebSocketURL(input)
		if err != nil || got != want {
			t.Errorf("normalizeDirectWebSocketURL(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "host:8788", "http://host:8788"} {
		if _, err := normalizeDirectWebSocketURL(input); err == nil {
			t.Errorf("normalizeDirectWebSocketURL(%q) unexpectedly succeeded", input)
		}
	}
}

func TestSubcommandHelpReturnsSuccess(t *testing.T) {
	for _, command := range []string{"run", "login", "agent", "controller", "status", "web"} {
		var stdout, stderr bytes.Buffer
		if err := execute(context.Background(), []string{command, "--help"}, &stdout, &stderr); err != nil {
			t.Errorf("%s --help: %v", command, err)
		}
	}
}

func TestLoginEnrollsDeviceAgainstController(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SHENMUX_CONFIG_FILE", filepath.Join(root, "config", "config.json"))
	t.Setenv("SHENMUX_STATE_DIR", filepath.Join(root, "state"))
	store := relay.NewEnrollmentStore()
	code, err := store.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(relay.NewController(store, "http://controller.test"))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	if err := execute(context.Background(), []string{"login", "--controller", server.URL, "--code", code}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	paths := appstate.Paths{ConfigFile: filepath.Join(root, "config", "config.json"), StateDir: filepath.Join(root, "state")}
	state, err := paths.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.DeviceID == "" || len(state.DevicePrivateKey) == 0 || len(state.DeviceToken) == 0 {
		t.Fatalf("device credentials were not persisted: %+v", state)
	}
}
