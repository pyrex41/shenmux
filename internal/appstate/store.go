// Package appstate owns the small, versioned files used by the shenmux
// command. Session journals and credentials have their own stores; this
// package deliberately contains only user configuration and operability
// metadata.
package appstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const SchemaVersion = 1

type Paths struct {
	ConfigFile string
	StateDir   string
}

type Config struct {
	Version    int    `json:"version"`
	Controller string `json:"controller,omitempty"`
	TrustMode  string `json:"trust_mode,omitempty"`
	// Transport controls path selection: auto (default), relay, or tailscale.
	Transport      string `json:"transport,omitempty"`
	DirectEndpoint string `json:"direct_endpoint,omitempty"`
	TailscalePeer  string `json:"tailscale_peer,omitempty"`
	TailscalePort  int    `json:"tailscale_port,omitempty"`
	RequireDirect  bool   `json:"tailscale_require_direct,omitempty"`
	// Reconnect settings are persisted so service-manager launches and manual
	// launches use identical outage behavior. Durations are human-editable
	// strings such as "1s" and "1m".
	ReconnectInitial string  `json:"reconnect_initial,omitempty"`
	ReconnectMaximum string  `json:"reconnect_maximum,omitempty"`
	ReconnectFactor  float64 `json:"reconnect_factor,omitempty"`
	ReconnectJitter  float64 `json:"reconnect_jitter,omitempty"`
}

type Session struct {
	Name      string     `json:"name"`
	StartedAt time.Time  `json:"started_at"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
	PID       int        `json:"pid,omitempty"`
}

type State struct {
	Version          int                `json:"version"`
	DeviceID         string             `json:"device_id,omitempty"`
	DevicePublicKey  []byte             `json:"device_public_key,omitempty"`
	DevicePrivateKey []byte             `json:"device_private_key,omitempty"`
	DeviceToken      []byte             `json:"device_token,omitempty"`
	DeviceExpiresAt  *time.Time         `json:"device_expires_at,omitempty"`
	LastSuccess      *time.Time         `json:"last_success,omitempty"`
	LastController   string             `json:"last_controller,omitempty"`
	Sessions         map[string]Session `json:"sessions,omitempty"`
}

// ResolvePaths implements the command's path precedence. Explicit shenmux
// variables win, followed by XDG locations and then portable XDG defaults.
func ResolvePaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home directory: %w", err)
	}
	configFile := os.Getenv("SHENMUX_CONFIG_FILE")
	if configFile == "" {
		root := os.Getenv("XDG_CONFIG_HOME")
		if root == "" {
			root = filepath.Join(home, ".config")
		}
		configFile = filepath.Join(root, "shenmux", "config.json")
	}
	stateDir := os.Getenv("SHENMUX_STATE_DIR")
	if stateDir == "" {
		root := os.Getenv("XDG_STATE_HOME")
		if root == "" {
			root = filepath.Join(home, ".local", "state")
		}
		stateDir = filepath.Join(root, "shenmux")
	}
	configFile, err = filepath.Abs(configFile)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve config path: %w", err)
	}
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve state path: %w", err)
	}
	return Paths{ConfigFile: configFile, StateDir: stateDir}, nil
}

func (p Paths) StateFile() string { return filepath.Join(p.StateDir, "state.json") }

func (p Paths) Ensure() error {
	if err := ensurePrivateDir(filepath.Dir(p.ConfigFile)); err != nil {
		return fmt.Errorf("prepare config directory: %w", err)
	}
	if err := ensurePrivateDir(p.StateDir); err != nil {
		return fmt.Errorf("prepare state directory: %w", err)
	}
	return nil
}

func (p Paths) LoadConfig() (Config, error) {
	var config Config
	found, err := loadJSON(p.ConfigFile, &config)
	if err != nil {
		return Config{}, fmt.Errorf("load config: %w", err)
	}
	if !found {
		return Config{Version: SchemaVersion, TrustMode: "trusted", Transport: "auto", ReconnectInitial: "1s", ReconnectMaximum: "1m", ReconnectFactor: 2, ReconnectJitter: .2}, nil
	}
	if err := checkVersion(config.Version); err != nil {
		return Config{}, fmt.Errorf("load config: %w", err)
	}
	if config.TrustMode == "" {
		config.TrustMode = "trusted"
	}
	if config.Transport == "" {
		config.Transport = "auto"
	}
	setReconnectDefaults(&config)
	return config, nil
}

func (p Paths) SaveConfig(config Config) error {
	config.Version = SchemaVersion
	if config.TrustMode == "" {
		config.TrustMode = "trusted"
	}
	if config.Transport == "" {
		config.Transport = "auto"
	}
	setReconnectDefaults(&config)
	if err := p.Ensure(); err != nil {
		return err
	}
	if err := saveJSON(p.ConfigFile, config); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return nil
}

func setReconnectDefaults(config *Config) {
	// A fully omitted policy gets safe defaults. Once any reconnect field is
	// present, preserve explicit zero jitter (a useful deterministic setting).
	if config.ReconnectInitial != "" || config.ReconnectMaximum != "" || config.ReconnectFactor != 0 {
		if config.ReconnectInitial == "" {
			config.ReconnectInitial = "1s"
		}
		if config.ReconnectMaximum == "" {
			config.ReconnectMaximum = "1m"
		}
		if config.ReconnectFactor == 0 {
			config.ReconnectFactor = 2
		}
		return
	}
	if config.ReconnectInitial == "" {
		config.ReconnectInitial = "1s"
	}
	if config.ReconnectMaximum == "" {
		config.ReconnectMaximum = "1m"
	}
	if config.ReconnectFactor == 0 {
		config.ReconnectFactor = 2
	}
	config.ReconnectJitter = .2
}

func (p Paths) LoadState() (State, error) {
	var state State
	found, err := loadJSON(p.StateFile(), &state)
	if err != nil {
		return State{}, fmt.Errorf("load state: %w", err)
	}
	if !found {
		return State{Version: SchemaVersion, Sessions: make(map[string]Session)}, nil
	}
	if err := checkVersion(state.Version); err != nil {
		return State{}, fmt.Errorf("load state: %w", err)
	}
	if state.Sessions == nil {
		state.Sessions = make(map[string]Session)
	}
	return state, nil
}

func (p Paths) SaveState(state State) error {
	state.Version = SchemaVersion
	if err := p.Ensure(); err != nil {
		return err
	}
	if err := saveJSON(p.StateFile(), state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

func checkVersion(version int) error {
	if version != SchemaVersion {
		return fmt.Errorf("unsupported schema version %d (this binary supports %d)", version, SchemaVersion)
	}
	return nil
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	// Unix credentials and metadata must not inherit a permissive umask. On
	// Windows chmod does not provide the same ACL guarantee, so leave ACL
	// management to the platform installer.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func loadJSON(path string, target any) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return false, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return false, errors.New("multiple JSON values")
		}
		return false, err
	}
	return true, nil
}

func saveJSON(path string, value any) error {
	dir := filepath.Dir(path)
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".shenmux-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
