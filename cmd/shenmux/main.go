// shenmux is the whole command surface: one binary, one PTY, and the verbs
// that reach it.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	muxclient "github.com/pyrex41/shenmux/client"
	agentbridge "github.com/pyrex41/shenmux/internal/agent"
	"github.com/pyrex41/shenmux/internal/appstate"
	"github.com/pyrex41/shenmux/naming"
	"github.com/pyrex41/shenmux/internal/policy"
	"github.com/pyrex41/shenmux/internal/relay"
	"github.com/pyrex41/shenmux/screen"
	"github.com/pyrex41/shenmux/internal/server"
	"github.com/pyrex41/shenmux/internal/shenguard"
	transportpkg "github.com/pyrex41/shenmux/internal/transport"
	"github.com/pyrex41/shenmux/internal/ttyx"
	"github.com/pyrex41/shenmux/internal/webgateway"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// exitStatus carries a session's own exit status out of a subcommand. A
// command that ran in the PTY and failed is not a shenmux error, so it is
// reported as the status it had rather than as a message and 1.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("session exited with status %d", int(e)) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	var status exitStatus
	if errors.As(err, &status) {
		os.Exit(int(status))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "shenmux:", err)
		os.Exit(1)
	}
}

func execute(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return errors.New("a command is required")
	}
	switch args[0] {
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	case "version", "--version":
		fmt.Fprintf(stdout, "shenmux %s (commit %s, built %s)\n", version, commit, date)
		return nil
	case "run":
		return runLocal(ctx, args[1:], stdout, stderr)
	case "attach":
		return runAttach(ctx, args[1:], stdout, stderr)
	case "login":
		return runLogin(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "web":
		return runWeb(ctx, args[1:], stdout, stderr)
	case "agent":
		return runAgent(ctx, args[1:], stderr)
	case "controller":
		return runController(ctx, args[1:], stderr)
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `usage: shenmux <command> [flags]

Commands:
  run          run a local PTY session (no account or network required)
  attach       attach this terminal to a local session
  login        select a controller for enrollment
  agent        run the outbound agent with relay/tailnet transport
  controller   run a self-hosted controller
  status       show local configuration and session inventory
  web          serve the local browser gateway
  version      print build version information
`)
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags
}

func runLocal(ctx context.Context, args []string, _ io.Writer, stderr io.Writer) error {
	flags := newFlags("shenmux run", stderr)
	session := flags.String("session", "default", "session name")
	control := flags.String("control", "", "local ZeroMQ control endpoint")
	data := flags.String("data", "", "local ZeroMQ data endpoint")
	cols := flags.Int("cols", 80, "initial columns")
	rows := flags.Int("rows", 24, "initial rows")
	shell := flags.String("shell", "auto", "default login shell (auto, fish, zsh, bash, or a path)")
	keepalive := flags.Bool("keepalive", true, "restart the default login shell after it exits")
	grace := flags.Duration("exit-grace", 150*time.Millisecond, "time to leave sockets open after command exit")
	stateDir := flags.String("state-dir", "", "persistent state directory")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: shenmux run [flags] [-- command [args...]]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := naming.ValidateSession(*session); err != nil {
		return err
	}
	defaultControl, defaultData, err := naming.DefaultEndpoints(*session)
	if err != nil {
		return err
	}
	if *control == "" {
		*control = defaultControl
	}
	if *data == "" {
		*data = defaultData
	}
	dimensions, err := shenguard.NewDimensions(*cols, *rows)
	if err != nil {
		return err
	}
	command := flags.Args()
	defaultShell := len(command) == 0
	if defaultShell {
		command, err = shellCommand(*shell)
		if err != nil {
			return err
		}
		if *keepalive {
			command, err = superviseShell(command)
			if err != nil {
				return err
			}
		}
	}
	paths, err := appstate.ResolvePaths()
	if err != nil {
		return err
	}
	if *stateDir != "" {
		paths.StateDir, err = filepath.Abs(*stateDir)
		if err != nil {
			return fmt.Errorf("resolve state directory: %w", err)
		}
	}
	if err := recordSession(paths, *session, nil); err != nil {
		return err
	}
	defer func() {
		now := time.Now().UTC()
		if stateErr := recordSession(paths, *session, &now); stateErr != nil {
			fmt.Fprintln(stderr, "shenmux: update session state:", stateErr)
		}
	}()
	logger := log.New(stderr, "", 0)
	noteAbandonedHistory(logger, filepath.Join(paths.StateDir, "history"))
	logger.Printf("shenmux session=%s control=%s data=%s command=%q", *session, *control, *data, command)
	return server.Serve(ctx, server.Config{
		Session: *session, ControlEndpoint: *control, DataEndpoint: *data,
		Dimensions: dimensions, Command: command, Env: os.Environ(), ExitGrace: *grace,
	})
}

// noteAbandonedHistory tells the operator about session archives an older
// build wrote under the state directory. shenmux no longer writes them and
// nothing can restore one into a live session, but they are the operator's
// files, so this says where they are and leaves them alone.
func noteAbandonedHistory(logger *log.Logger, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return
	}
	logger.Printf("shenmux: %s holds session archives from an older build; shenmux no longer writes or reads them and the directory can be deleted", dir)
}

func recordSession(paths appstate.Paths, name string, stoppedAt *time.Time) error {
	state, err := paths.LoadState()
	if err != nil {
		return err
	}
	if stoppedAt == nil {
		state.Sessions[name] = appstate.Session{Name: name, StartedAt: time.Now().UTC(), PID: os.Getpid()}
	} else {
		session := state.Sessions[name]
		session.Name = name
		session.StoppedAt = stoppedAt
		session.PID = 0
		state.Sessions[name] = session
	}
	return paths.SaveState(state)
}

func shellCommand(preference string) ([]string, error) {
	preference = strings.TrimSpace(preference)
	if preference == "" {
		preference = "auto"
	}
	candidates := []string{preference}
	if preference == "auto" {
		candidates = nil
		if configured := os.Getenv("SHELL"); configured != "" {
			base := filepath.Base(configured)
			if base == "fish" || base == "zsh" {
				candidates = append(candidates, configured)
			}
		}
		candidates = append(candidates, "zsh", "fish")
		if configured := os.Getenv("SHELL"); configured != "" {
			candidates = append(candidates, configured)
		}
		candidates = append(candidates, "bash", "sh")
	}
	for _, candidate := range candidates {
		path, err := exec.LookPath(candidate)
		if err == nil {
			return []string{path, "-il"}, nil
		}
	}
	return nil, fmt.Errorf("no usable shell found for --shell %q", preference)
}

func superviseShell(command []string) ([]string, error) {
	if len(command) == 0 {
		return nil, errors.New("shell command is empty")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		return nil, fmt.Errorf("find shell supervisor: %w", err)
	}
	result := []string{sh, "-c", `while :; do "$0" "$@"; sleep 0.1; done`, command[0]}
	return append(result, command[1:]...), nil
}

const detachByte = byte(0x1d) // Ctrl-]

func runAttach(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := newFlags("shenmux attach", stderr)
	session := flags.String("session", "default", "session name")
	clientID := flags.String("client", "", "client identity (generated by default)")
	control := flags.String("control", "", "local ZeroMQ control endpoint")
	data := flags.String("data", "", "local ZeroMQ data endpoint")
	noRaw := flags.Bool("no-raw", false, "do not put stdin in raw mode")
	observe := flags.Bool("observe", false, "attach read-only instead of acquiring the exclusive input/resize lease")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: shenmux attach [flags]")
		fmt.Fprintln(flags.Output(), "\nAttach this terminal to a session shenmux run owns. Press Ctrl-] to detach.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("attach does not accept positional arguments")
	}
	if err := naming.ValidateSession(*session); err != nil {
		return err
	}
	defaultControl, defaultData, err := naming.DefaultEndpoints(*session)
	if err != nil {
		return err
	}
	if *control == "" {
		*control = defaultControl
	}
	if *data == "" {
		*data = defaultData
	}
	if *clientID == "" {
		*clientID = generatedClientID()
	}

	client, err := muxclient.New(ctx, muxclient.Config{
		Session: *session, ClientID: *clientID,
		ControlEndpoint: *control, DataEndpoint: *data,
	})
	if err != nil {
		return err
	}
	defer client.Close()

	if !*noRaw && ttyx.IsTerminal(os.Stdin.Fd()) {
		raw, rawErr := ttyx.MakeRaw(os.Stdin.Fd())
		if rawErr != nil {
			return rawErr
		}
		defer raw.Restore()
	}

	attachCtx, cancelAttach := context.WithTimeout(ctx, 5*time.Second)
	snapshot, err := client.Attach(attachCtx)
	cancelAttach()
	if err != nil {
		return err
	}
	view := snapshot.View()
	renderer := &screen.Renderer{}
	if err := snapshot.Render(stdout, renderer); err != nil {
		return err
	}
	if view.Checkpoint.Exited {
		return sessionExit(view.Checkpoint.ExitCode)
	}

	var hasControl atomic.Bool
	if !*observe {
		leaseCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		meta, acquireErr := client.AcquireControl(leaseCtx)
		cancel()
		if acquireErr != nil {
			return fmt.Errorf("acquire exclusive control (use --observe for read-only attachment): %w", acquireErr)
		}
		hasControl.Store(meta.HasControl)
	} else {
		hasControl.Store(view.HasControl(client.ID()))
	}

	if hasControl.Load() {
		if cols, rows, sizeErr := ttyx.Size(os.Stdin.Fd()); sizeErr == nil {
			if err := client.Resize(ctx, cols, rows); err != nil {
				return err
			}
		}
	}
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-winch:
				if !hasControl.Load() {
					continue
				}
				if cols, rows, sizeErr := ttyx.Size(os.Stdin.Fd()); sizeErr == nil {
					if resizeErr := client.Resize(ctx, cols, rows); resizeErr != nil && !errors.Is(resizeErr, context.Canceled) {
						// The event loop also receives ownership changes. Report a
						// failed resize without racing to terminate the main loop.
						fmt.Fprintln(stderr, "shenmux attach: resize:", resizeErr)
					}
				}
			}
		}
	}()

	inputDone := make(chan error, 1)
	detach := make(chan struct{}, 1)
	go copyAttachInput(ctx, client, &hasControl, inputDone, detach)
	events := client.Events()
	errorsCh := client.Errors()

	for {
		select {
		case <-detach:
			detachCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			detachErr := client.Detach(detachCtx)
			cancel()
			return detachErr
		case inputErr := <-inputDone:
			inputDone = nil
			if inputErr != nil && !errors.Is(inputErr, io.EOF) && !errors.Is(inputErr, context.Canceled) {
				return inputErr
			}
		case actorErr, ok := <-errorsCh:
			if !ok {
				errorsCh = nil
				continue
			}
			if actorErr != nil {
				fmt.Fprintln(stderr, "shenmux attach:", actorErr)
			}
		case msg, ok := <-events:
			if !ok {
				return nil
			}
			if msg.Meta.Seq <= view.Checkpoint.Seq {
				continue
			}
			if msg.Meta.Seq != view.Checkpoint.Seq+1 {
				fresh, syncErr := attachResync(ctx, client, stdout, renderer)
				if syncErr != nil {
					return syncErr
				}
				view = fresh.View()
				hasControl.Store(view.HasControl(client.ID()))
				if view.Checkpoint.Exited {
					return sessionExit(view.Checkpoint.ExitCode)
				}
				continue
			}
			delta, applyErr := view.Apply(msg)
			if applyErr != nil {
				return applyErr
			}
			hasControl.Store(view.HasControl(client.ID()))
			if delta != nil {
				if err := renderer.RenderDelta(stdout, view.Checkpoint.Screen, *delta); err != nil {
					return err
				}
			}
			if view.Checkpoint.Exited {
				return sessionExit(view.Checkpoint.ExitCode)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// sessionExit reports the status of a command that ran in the PTY. A clean
// exit is not an error at all; a failure is the session's status, not ours.
func sessionExit(code int) error {
	if code == 0 {
		return nil
	}
	return exitStatus(code)
}

func attachResync(ctx context.Context, client *muxclient.Client, stdout io.Writer, renderer *screen.Renderer) (muxclient.Snapshot, error) {
	resyncCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	fresh, err := client.Resync(resyncCtx)
	cancel()
	if err != nil {
		return muxclient.Snapshot{}, err
	}
	if err := fresh.Render(stdout, renderer); err != nil {
		return muxclient.Snapshot{}, err
	}
	return fresh, nil
}

func copyAttachInput(ctx context.Context, client *muxclient.Client, hasControl *atomic.Bool, done chan<- error, detach chan<- struct{}) {
	buffer := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buffer)
		if n > 0 {
			detachAt := -1
			for i := 0; i < n; i++ {
				if buffer[i] == detachByte {
					detachAt = i
					break
				}
			}
			if detachAt >= 0 {
				if detachAt > 0 && hasControl.Load() {
					if sendErr := client.Input(ctx, buffer[:detachAt]); sendErr != nil {
						done <- sendErr
						return
					}
				}
				select {
				case detach <- struct{}{}:
				default:
				}
				return
			}
			if hasControl.Load() {
				if sendErr := client.Input(ctx, buffer[:n]); sendErr != nil {
					done <- sendErr
					return
				}
			}
		}
		if err != nil {
			done <- err
			return
		}
	}
}

func generatedClientID() string {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Sprintf("shenmux-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return fmt.Sprintf("shenmux-%d-%s", os.Getpid(), hex.EncodeToString(random[:]))
}

func runLogin(args []string, stdout, stderr io.Writer) error {
	paths, err := appstate.ResolvePaths()
	if err != nil {
		return err
	}
	controllerDefault := os.Getenv("SHENMUX_CONTROLLER")
	flags := newFlags("shenmux login", stderr)
	controller := flags.String("controller", controllerDefault, "controller HTTPS URL")
	code := flags.String("code", "", "single-use enrollment code")
	trustMode := flags.String("trust-mode", "", "relay content trust mode (trusted or blind)")
	configFile := flags.String("config", "", "configuration file")
	stateDir := flags.String("state-dir", "", "persistent state directory")
	force := flags.Bool("force", false, "replace an existing device enrollment (destroys its keypair)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: shenmux login --controller URL")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("login does not accept positional arguments")
	}
	if *configFile != "" {
		paths.ConfigFile, err = filepath.Abs(*configFile)
		if err != nil {
			return fmt.Errorf("resolve config file: %w", err)
		}
	}
	if *stateDir != "" {
		paths.StateDir, err = filepath.Abs(*stateDir)
		if err != nil {
			return fmt.Errorf("resolve state directory: %w", err)
		}
	}
	// Enrolling writes a fresh keypair over whatever is in the state file.
	// Refuse before touching anything, so a quickstart cannot silently
	// destroy an enrollment the user cares about.
	state, err := paths.LoadState()
	if err != nil {
		return err
	}
	if *code != "" && state.DeviceID != "" && !*force {
		return fmt.Errorf("device %s is already enrolled in %s; rerun with --force to replace it (this destroys its keypair and the old device stays registered on the controller), or with --state-dir DIR to enroll alongside it",
			state.DeviceID, paths.StateFile())
	}
	config, err := paths.LoadConfig()
	if err != nil {
		return err
	}
	if *controller == "" {
		*controller = config.Controller
	}
	normalized, err := normalizeController(*controller)
	if err != nil {
		return err
	}
	config.Controller = normalized
	if *trustMode != "" {
		mode := relay.TrustMode(strings.ToLower(strings.TrimSpace(*trustMode)))
		if err := mode.Validate(); err != nil {
			return err
		}
		config.TrustMode = string(mode)
	}
	if err := paths.SaveConfig(config); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "controller configured: %s\n", normalized)
	if *code == "" {
		fmt.Fprintln(stdout, "device enrollment pending: rerun with --code CODE")
		return nil
	}
	if err := enrollDevice(normalized, *code, &state); err != nil {
		return err
	}
	if err := paths.SaveState(state); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "device enrolled: %s\n", state.DeviceID)
	return nil
}

func enrollDevice(controller, code string, state *appstate.State) error {
	pub, priv, err := relay.GenerateDeviceKey()
	if err != nil {
		return err
	}
	body, err := json.Marshal(relay.EnrollmentRequest{Code: code, PublicKey: pub})
	if err != nil {
		return err
	}
	resp, err := http.Post(strings.TrimRight(controller, "/")+"/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("enroll device: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("enroll device: controller returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	var credential relay.DeviceCredential
	if err := json.NewDecoder(resp.Body).Decode(&credential); err != nil {
		return fmt.Errorf("decode enrollment response: %w", err)
	}
	if err := credential.Validate(time.Now()); err != nil {
		return err
	}
	state.DeviceID = credential.DeviceID
	state.DevicePublicKey = append([]byte(nil), pub...)
	state.DevicePrivateKey = append([]byte(nil), priv...)
	state.DeviceToken = append([]byte(nil), credential.Token...)
	state.DeviceExpiresAt = &credential.ExpiresAt
	return nil
}

func normalizeController(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid controller URL %q", raw)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("controller URL must not contain credentials, a query, or a fragment")
	}
	if parsed.Scheme != "https" {
		host := parsed.Hostname()
		if parsed.Scheme != "http" || (host != "localhost" && net.ParseIP(host) == nil) {
			return "", errors.New("controller URL must use HTTPS (HTTP is allowed only for localhost or an IP development controller)")
		}
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	return parsed.String(), nil
}

func runStatus(args []string, stdout, stderr io.Writer) error {
	flags := newFlags("shenmux status", stderr)
	jsonOutput := flags.Bool("json", false, "print machine-readable JSON")
	stateDir := flags.String("state-dir", "", "persistent state directory")
	configFile := flags.String("config", "", "configuration file")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("status does not accept positional arguments")
	}
	paths, err := appstate.ResolvePaths()
	if err != nil {
		return err
	}
	if *stateDir != "" {
		paths.StateDir, err = filepath.Abs(*stateDir)
		if err != nil {
			return err
		}
	}
	if *configFile != "" {
		paths.ConfigFile, err = filepath.Abs(*configFile)
		if err != nil {
			return err
		}
	}
	config, err := paths.LoadConfig()
	if err != nil {
		return err
	}
	state, err := paths.LoadState()
	if err != nil {
		return err
	}
	report := struct {
		Version    string `json:"binary_version"`
		Controller string `json:"controller,omitempty"`
		TrustMode  string `json:"trust_mode"`
		Transport  string `json:"transport"`
		Direct     string `json:"direct_endpoint,omitempty"`
		DeviceID   string `json:"device_id,omitempty"`
		StateDir   string `json:"state_dir"`
		Reconnect  struct {
			Initial string  `json:"initial"`
			Maximum string  `json:"maximum"`
			Factor  float64 `json:"factor"`
			Jitter  float64 `json:"jitter"`
		} `json:"reconnect"`
		Sessions []appstate.Session `json:"sessions"`
	}{Version: version, Controller: config.Controller, TrustMode: config.TrustMode, Transport: config.Transport, Direct: config.DirectEndpoint, DeviceID: state.DeviceID, StateDir: paths.StateDir}
	report.Reconnect.Initial, report.Reconnect.Maximum = config.ReconnectInitial, config.ReconnectMaximum
	report.Reconnect.Factor, report.Reconnect.Jitter = config.ReconnectFactor, config.ReconnectJitter
	for _, session := range state.Sessions {
		report.Sessions = append(report.Sessions, session)
	}
	sort.Slice(report.Sessions, func(i, j int) bool { return report.Sessions[i].Name < report.Sessions[j].Name })
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	controller := report.Controller
	if controller == "" {
		controller = "local-only"
	}
	fmt.Fprintf(stdout, "version: %s\ncontroller: %s\ntrust mode: %s\ntransport: %s\nreconnect: %s → %s (×%g, jitter %g)\nstate: %s\n", report.Version, controller, report.TrustMode, report.Transport, report.Reconnect.Initial, report.Reconnect.Maximum, report.Reconnect.Factor, report.Reconnect.Jitter, report.StateDir)
	if report.Direct != "" {
		fmt.Fprintf(stdout, "direct: %s\n", report.Direct)
	}
	if report.DeviceID != "" {
		fmt.Fprintf(stdout, "device: %s\n", report.DeviceID)
	}
	fmt.Fprintf(stdout, "sessions: %d\n", len(report.Sessions))
	for _, session := range report.Sessions {
		status := "running"
		if session.StoppedAt != nil {
			status = "stopped"
		}
		fmt.Fprintf(stdout, "  %s (%s)\n", session.Name, status)
	}
	return nil
}

func runWeb(ctx context.Context, args []string, _ io.Writer, stderr io.Writer) error {
	flags := newFlags("shenmux web", stderr)
	session := flags.String("session", "default", "shenmux session")
	listen := flags.String("listen", "127.0.0.1:8787", "HTTP listen address")
	control := flags.String("control", "", "local ZeroMQ control endpoint")
	data := flags.String("data", "", "local ZeroMQ data endpoint")
	token := flags.String("token", os.Getenv("SHENMUX_WEB_TOKEN"), "access token required to attach (generated when empty)")
	noToken := flags.Bool("no-token", false, "serve without an access token; anything that can reach --listen may attach")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("web does not accept positional arguments")
	}
	controlDefault, dataDefault, err := naming.DefaultEndpoints(*session)
	if err != nil {
		return err
	}
	if *control == "" {
		*control = controlDefault
	}
	if *data == "" {
		*data = dataDefault
	}
	// A loopback listener is not an authorization boundary: every local
	// process shares it. The token is on by default, and the printed URL is
	// the way in. --no-token is the deliberate opt-out.
	accessToken := strings.TrimSpace(*token)
	switch {
	case *noToken:
		if accessToken != "" {
			return errors.New("--no-token and --token are mutually exclusive")
		}
	case accessToken == "":
		accessToken, err = webgateway.NewToken()
		if err != nil {
			return err
		}
	}
	webCfg := webgateway.Config{
		Session: *session, ControlEndpoint: *control, DataEndpoint: *data,
		Token: accessToken,
	}
	gateway := webgateway.New(ctx, webCfg)
	server := &http.Server{Addr: *listen, Handler: gateway.Handler()}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	logger := log.New(stderr, "", 0)
	logger.Printf("shenmux web session=%s listen=http://%s instance=%s", *session, *listen, gateway.Instance())
	logger.Printf("shenmux web url=%s", browserURL(*listen, accessToken))
	if accessToken == "" {
		logger.Printf("shenmux web: no access token (--no-token); any local process can attach to session %s", *session)
	}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// browserURL turns a listen address into something a human can open. A
// wildcard bind is printed as loopback because that is the address the person
// running the command is sitting at.
func browserURL(listen, token string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = listen, ""
	}
	switch strings.Trim(host, "[]") {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	authority := host
	if port != "" {
		authority = net.JoinHostPort(host, port)
	}
	target := &url.URL{Scheme: "http", Host: authority, Path: "/"}
	if token != "" {
		target.RawQuery = url.Values{"token": []string{token}}.Encode()
	}
	return target.String()
}

func runReserved(name string, args []string, stderr io.Writer) error {
	flags := newFlags("shenmux "+name, stderr)
	controller := flags.String("controller", os.Getenv("SHENMUX_CONTROLLER"), "controller URL")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	_ = controller
	return fmt.Errorf("%s command is reserved for the authenticated relay implementation", name)
}

func runAgent(ctx context.Context, args []string, stderr io.Writer) error {
	flags := newFlags("shenmux agent", stderr)
	controller := flags.String("controller", os.Getenv("SHENMUX_CONTROLLER"), "controller URL")
	enrollmentCode := flags.String("enrollment-code", os.Getenv("SHENMUX_ENROLLMENT_CODE"), "single-use enrollment code for first startup")
	stateDir := flags.String("state-dir", "", "persistent state directory")
	controlEndpoint := flags.String("control", "", "local ZeroMQ control endpoint (for sidecar attachment)")
	dataEndpoint := flags.String("data", "", "local ZeroMQ data endpoint (for sidecar attachment)")
	transportMode := flags.String("transport", os.Getenv("SHENMUX_TRANSPORT"), "transport path (auto, relay, tailscale)")
	directEndpoint := flags.String("direct", os.Getenv("SHENMUX_DIRECT_ENDPOINT"), "direct Tailscale/WireGuard WebSocket endpoint")
	tailscalePeer := flags.String("tailscale-peer", os.Getenv("SHENMUX_TAILSCALE_PEER"), "Tailscale peer hostname/IP/ID to discover")
	tailscalePort := flags.Int("tailscale-port", 8788, "Tailscale peer WebSocket port")
	requireDirect := flags.Bool("tailscale-require-direct", false, "fail instead of using DERP/peer-relay when selecting Tailscale")
	reconnectInitial := flags.Duration("reconnect-initial", 0, "initial reconnect delay (persisted)")
	reconnectMaximum := flags.Duration("reconnect-maximum", 0, "maximum reconnect delay (persisted)")
	reconnectFactor := flags.Float64("reconnect-factor", 0, "reconnect exponential factor (persisted)")
	reconnectJitter := flags.Float64("reconnect-jitter", -1, "reconnect jitter fraction, 0 to 1 (persisted)")
	labels := labelFlag{}
	flags.Var(labels, "label", "opaque key=value label advertised to the controller, repeatable")
	session := flags.String("session", os.Getenv("SHENMUX_SESSION"), "discoverable PTY session name")
	sessions := flags.String("sessions", os.Getenv("SHENMUX_SESSIONS"), "comma-separated discoverable session names")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	paths, err := appstate.ResolvePaths()
	if err != nil {
		return err
	}
	if *stateDir != "" {
		paths.StateDir, err = filepath.Abs(*stateDir)
		if err != nil {
			return err
		}
	}
	config, err := paths.LoadConfig()
	if err != nil {
		return err
	}
	if *controller == "" {
		*controller = config.Controller
	}
	if *transportMode == "" {
		*transportMode = config.Transport
	}
	if *transportMode == "" {
		*transportMode = "auto"
	}
	if *directEndpoint == "" {
		*directEndpoint = config.DirectEndpoint
	}
	if *tailscalePeer == "" {
		*tailscalePeer = config.TailscalePeer
	}
	if *tailscalePort == 8788 && config.TailscalePort != 0 {
		*tailscalePort = config.TailscalePort
	}
	if !*requireDirect {
		*requireDirect = config.RequireDirect
	}
	// Empty flag values inherit persisted settings. The tunnel itself applies
	// the values below, while saving them makes restarts deterministic.
	if *reconnectInitial <= 0 {
		if d, parseErr := time.ParseDuration(config.ReconnectInitial); parseErr == nil {
			*reconnectInitial = d
		}
	}
	if *reconnectMaximum <= 0 {
		if d, parseErr := time.ParseDuration(config.ReconnectMaximum); parseErr == nil {
			*reconnectMaximum = d
		}
	}
	if *reconnectFactor == 0 {
		*reconnectFactor = config.ReconnectFactor
	}
	if *reconnectJitter < 0 {
		*reconnectJitter = config.ReconnectJitter
	}
	if *reconnectInitial <= 0 {
		*reconnectInitial = time.Second
	}
	if *reconnectMaximum <= 0 {
		*reconnectMaximum = time.Minute
	}
	if *reconnectFactor < 1 {
		return errors.New("--reconnect-factor must be at least 1")
	}
	if *reconnectJitter < 0 || *reconnectJitter > 1 {
		return errors.New("--reconnect-jitter must be between 0 and 1")
	}
	*transportMode = strings.ToLower(strings.TrimSpace(*transportMode))
	if *transportMode != "auto" && *transportMode != "relay" && *transportMode != "tailscale" {
		return fmt.Errorf("invalid --transport %q (want auto, relay, or tailscale)", *transportMode)
	}
	if *directEndpoint != "" {
		normalized, normalizeErr := normalizeDirectWebSocketURL(*directEndpoint)
		if normalizeErr != nil {
			return normalizeErr
		}
		*directEndpoint = normalized
	}
	if (*transportMode == "auto" || *transportMode == "tailscale") && *directEndpoint == "" && *tailscalePeer != "" {
		resolver := transportpkg.TailscaleResolver{Peer: *tailscalePeer, Port: *tailscalePort, RequireDirect: *requireDirect}
		resolved, resolveErr := resolver.Resolve(ctx)
		if resolveErr != nil {
			if *transportMode == "tailscale" {
				return fmt.Errorf("resolve tailscale peer: %w", resolveErr)
			}
			log.New(stderr, "", 0).Printf("tailscale unavailable; using relay: %v", resolveErr)
		} else {
			*directEndpoint = resolved
		}
	}
	if *transportMode == "tailscale" && strings.TrimSpace(*directEndpoint) == "" {
		return errors.New("--transport tailscale requires --direct endpoint or --tailscale-peer")
	}
	if *controller == "" {
		return errors.New("agent requires --controller or a configured controller")
	}
	// Persist transport preferences so service-manager invocations have the
	// same path selection as the interactive command.
	config.Controller = *controller
	config.Transport = *transportMode
	config.DirectEndpoint = *directEndpoint
	config.TailscalePeer = *tailscalePeer
	config.TailscalePort = *tailscalePort
	config.RequireDirect = *requireDirect
	config.ReconnectInitial = reconnectInitial.String()
	config.ReconnectMaximum = reconnectMaximum.String()
	config.ReconnectFactor = *reconnectFactor
	config.ReconnectJitter = *reconnectJitter
	if saveErr := paths.SaveConfig(config); saveErr != nil {
		return saveErr
	}
	state, err := paths.LoadState()
	if err != nil {
		return err
	}
	if state.DeviceID == "" && *enrollmentCode != "" {
		if err := enrollDevice(*controller, *enrollmentCode, &state); err != nil {
			return err
		}
		if err := paths.SaveState(state); err != nil {
			return err
		}
	}
	if state.DeviceID == "" || len(state.DevicePrivateKey) != ed25519.PrivateKeySize || len(state.DeviceToken) == 0 {
		return errors.New("agent is not enrolled; run shenmux login --controller URL --code CODE")
	}
	metadata := relay.AgentMetadata{}
	if len(labels) > 0 {
		metadata.Labels = labels
	}
	sessionNames := []string{}
	if *session != "" {
		sessionNames = append(sessionNames, *session)
	}
	for _, name := range strings.Split(*sessions, ",") {
		name = strings.TrimSpace(name)
		if name != "" && !contains(sessionNames, name) {
			sessionNames = append(sessionNames, name)
		}
	}
	for _, name := range sessionNames {
		metadata.Sessions = append(metadata.Sessions, relay.SessionDescriptor{ID: name, Name: name})
	}
	wsURL, err := controllerWebSocketURL(*controller)
	if err != nil {
		return err
	}
	var expires time.Time
	if state.DeviceExpiresAt != nil {
		expires = *state.DeviceExpiresAt
	}
	endpoints := []string{wsURL}
	if *transportMode == "tailscale" {
		endpoints = []string{*directEndpoint}
	} else if *transportMode == "auto" && *directEndpoint != "" {
		// Prefer the encrypted tailnet path; relay remains the availability
		// fallback when the peer is offline or unreachable.
		endpoints = []string{*directEndpoint, wsURL}
	}
	tunnel := &relay.Tunnel{URL: wsURL, Endpoints: endpoints, Origin: *controller,
		Credential: relay.DeviceCredential{DeviceID: state.DeviceID, Token: state.DeviceToken, ExpiresAt: expires},
		Metadata:   metadata,
		PrivateKey: ed25519.PrivateKey(state.DevicePrivateKey),
		Policy:     relay.BackoffPolicy{Initial: *reconnectInitial, Maximum: *reconnectMaximum, Factor: *reconnectFactor, Jitter: *reconnectJitter}}
	log.New(stderr, "", 0).Printf("shenmux agent device=%s controller=%s transport=%s direct=%s", state.DeviceID, *controller, *transportMode, *directEndpoint)
	err = tunnel.Run(ctx, func(conn *websocket.Conn) error {
		now := time.Now().UTC()
		state.LastSuccess = &now
		if saveErr := paths.SaveState(state); saveErr != nil {
			return saveErr
		}
		bridge := agentbridge.NewBridge(state.DeviceID, func(session string) (string, string, error) {
			if *controlEndpoint != "" || *dataEndpoint != "" {
				if *controlEndpoint == "" || *dataEndpoint == "" {
					return "", "", errors.New("--control and --data must be provided together")
				}
				return *controlEndpoint, *dataEndpoint, nil
			}
			return naming.DefaultEndpoints(session)
		})
		bridge.TrustMode = relay.TrustMode(config.TrustMode)
		bridge.PrivateKey = ed25519.PrivateKey(state.DevicePrivateKey)
		bridgeErr := bridge.Serve(ctx, conn)
		if bridgeErr != nil && !errors.Is(bridgeErr, context.Canceled) {
			log.New(stderr, "", 0).Printf("agent bridge disconnected: %v", bridgeErr)
		}
		return bridgeErr
	})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// labelFlag collects repeated --label key=value pairs. The values are never
// read by shenmux; they are advertised to the controller and handed back to
// whoever asks for the session list.
type labelFlag map[string]string

func (l labelFlag) String() string {
	keys := make([]string, 0, len(l))
	for key := range l {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+l[key])
	}
	return strings.Join(pairs, ",")
}

func (l labelFlag) Set(value string) error {
	key, label, found := strings.Cut(value, "=")
	key = strings.TrimSpace(key)
	if !found || key == "" {
		return fmt.Errorf("invalid --label %q (want key=value)", value)
	}
	l[key] = label
	return nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func runController(ctx context.Context, args []string, stderr io.Writer) error {
	flags := newFlags("shenmux controller", stderr)
	listen := flags.String("listen", "127.0.0.1:8788", "HTTPS/WSS controller listen address (HTTP for local development)")
	origin := flags.String("origin", "http://localhost", "controller origin bound into agent challenge proofs")
	stateDir := flags.String("state-dir", os.Getenv("SHENMUX_CONTROLLER_STATE_DIR"), "durable controller state directory")
	enrollmentCount := flags.Int("enrollment-count", 1, "number of single-use enrollment codes to print")
	devBrowserSubject := flags.String("dev-browser-subject", "", "enable local workspace browser subject (development only)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("controller does not accept positional arguments")
	}
	if *stateDir == "" {
		root := os.Getenv("XDG_STATE_HOME")
		if root == "" {
			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				return homeErr
			}
			root = filepath.Join(home, ".local", "state")
		}
		*stateDir = filepath.Join(root, "shenmux-controller")
	}
	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		return fmt.Errorf("prepare controller state directory: %w", err)
	}
	store, err := relay.NewPersistentEnrollmentStore(filepath.Join(*stateDir, "enrollment.json"))
	if err != nil {
		return fmt.Errorf("load controller enrollment state: %w", err)
	}
	controller := relay.NewController(store, *origin)
	controller.Policy, err = policy.New(filepath.Join(*stateDir, "policy.json"))
	if err != nil {
		return fmt.Errorf("load controller policy state: %w", err)
	}
	controller.DevBrowserSubject = *devBrowserSubject
	if *devBrowserSubject != "" {
		if _, err := controller.Policy.AddGrant(*devBrowserSubject, "", "*", []policy.Permission{policy.PermissionObserve, policy.PermissionControl}, nil); err != nil {
			return err
		}
	}
	httpServer := &http.Server{Addr: *listen, Handler: controller}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	if *enrollmentCount < 1 || *enrollmentCount > 100 {
		return errors.New("--enrollment-count must be between 1 and 100")
	}
	codes := make([]string, *enrollmentCount)
	for i := range codes {
		code, createErr := store.Create(10 * time.Minute)
		if createErr != nil {
			return createErr
		}
		codes[i] = code
	}
	log.New(stderr, "", 0).Printf("shenmux controller listen=http://%s enrollment_codes=%s", *listen, strings.Join(codes, ","))
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func controllerWebSocketURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if parsed.Scheme == "https" {
		parsed.Scheme = "wss"
	} else if parsed.Scheme == "http" {
		parsed.Scheme = "ws"
	} else {
		return "", fmt.Errorf("controller URL must use http or https")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/ws"
	return parsed.String(), nil
}

// normalizeDirectWebSocketURL accepts tailscale:// and wireguard:// endpoint
// forms (which document the network trust boundary) as well as ws/wss URLs.
// Tailnet links are already encrypted by WireGuard, so they use ws:// for the
// HTTP upgrade unless the caller explicitly requests wss://.
func normalizeDirectWebSocketURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid direct endpoint: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "tailscale", "ts", "wireguard", "wg":
		parsed.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("direct endpoint must use tailscale://, wireguard://, ws://, or wss://")
	}
	if parsed.Host == "" {
		return "", errors.New("direct endpoint host is empty")
	}
	if parsed.Path == "" {
		parsed.Path = "/ws"
	}
	return parsed.String(), nil
}
