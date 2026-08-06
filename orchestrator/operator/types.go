// Package operator implements the local "operator" for muxwork: in demo mode it
// manages worker OS processes instead of Kubernetes pods. A worker is a
// harness process fronted by a secret-proxy, with state captured as
// content-addressed overlay checkpoints via the snapshot binary.
//
// This package is stdlib-only. It depends on the other demo components only at
// runtime, by executing the binaries in bin/ (secret-proxy, snapshot, shenmux,
// mock-agent); it never imports their packages.
package operator

import (
	"encoding/json"
	"os"
)

// Phase is a worker lifecycle phase.
const (
	PhasePending   = "Pending"
	PhaseRunning   = "Running"
	PhaseSuspended = "Suspended"
	PhaseCompleted = "Completed"
	PhaseFailed    = "Failed"
)

// Event names emitted to events.ndjson (the demo's stand-in for the CR watch
// stream).
const (
	EventSpawned      = "spawned"
	EventRunning      = "running"
	EventCheckpointed = "checkpointed"
	EventSuspended    = "suspended"
	EventResumed      = "resumed"
	EventForked       = "forked"
	EventCompleted    = "completed"
	EventFailed       = "failed"
)

// Session backends, recorded in status.json.
const (
	BackendShenmux = "shenmux"
	BackendTmux    = "tmux"
	BackendDirect  = "direct"
)

// SecretSpec is one entry in a worker's secrets.json. It mirrors the schema
// consumed by bin/secret-proxy (CONTRACTS.md section 1).
type SecretSpec struct {
	Placeholder  string   `json:"placeholder"`
	Value        string   `json:"value,omitempty"`
	BaoRef       string   `json:"bao_ref,omitempty"`
	AllowedHosts []string `json:"allowed_hosts"`
}

// SecretsFile is the top-level secrets.json document.
type SecretsFile struct {
	Secrets map[string]SecretSpec `json:"secrets"`
}

// Status is the persisted status.json for a worker.
type Status struct {
	Name           string `json:"name"`
	Harness        string `json:"harness"`
	Phase          string `json:"phase"`
	PID            int    `json:"pid"`
	ProxyPID       int    `json:"proxy_pid"`
	ProxyPort      int    `json:"proxy_port"`
	SessionBackend string `json:"session_backend"`
	CreatedAt      int64  `json:"created_at"`
	LastCheckpoint string `json:"last_checkpoint"`
	CompletedAt    int64  `json:"completed_at,omitempty"`
	Parent         string `json:"parent"`
	MockTarget     string `json:"mock_target,omitempty"`
	MockSteps      string `json:"mock_steps,omitempty"`
	// Steps records every spawn/resume step and which fallback path was used,
	// per the spawn contract.
	Steps []string `json:"steps,omitempty"`
}

// Event is one line in events.ndjson.
type Event struct {
	TS     int64       `json:"ts"`
	Worker string      `json:"worker"`
	Event  string      `json:"event"`
	Detail interface{} `json:"detail,omitempty"`
}

// DefaultSecrets builds the demo secrets.json for a worker: a single
// ANTHROPIC_API_KEY whose placeholder the harness holds and whose real value
// the proxy injects, allowed only for the loopback upstream and .anthropic.com.
func DefaultSecrets(name string) SecretsFile {
	return SecretsFile{
		Secrets: map[string]SecretSpec{
			"ANTHROPIC_API_KEY": {
				Placeholder:  PlaceholderKey,
				Value:        "sk-REAL-demo-" + name + "-key",
				AllowedHosts: []string{"127.0.0.1", "localhost", ".anthropic.com"},
			},
		},
	}
}

// PlaceholderKey is the fake token the harness sees; the proxy swaps it for the
// real value on allow-listed hosts only.
const PlaceholderKey = "sk-placeholder-anthropic"

// WriteJSONFile atomically writes v as indented JSON to path.
func WriteJSONFile(path string, v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadJSONFile reads path and unmarshals it into v.
func ReadJSONFile(path string, v interface{}) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// WriteSecrets writes a worker's secrets.json.
func WriteSecrets(path string, s SecretsFile) error {
	return WriteJSONFile(path, s)
}
