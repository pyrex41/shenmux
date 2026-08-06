package webgateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pyrex41/shenmux/client"
	"github.com/pyrex41/shenmux/internal/history"
	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/webui"
	workspacebackend "github.com/pyrex41/shenmux/internal/workspace"
)

const commandTimeout = 5 * time.Second

// The served page carries this process's identity so a tab left open from a
// previous gateway cannot silently attach to a new one. The placeholders are
// substituted per request; the file on disk never contains a real value.
const (
	instancePlaceholder = "__SHENMUX_INSTANCE__"
	tokenPlaceholder    = "__SHENMUX_TOKEN__"
)

// Refusal reasons reported on the X-Shenmux-Refusal header and by
// /api/instance, so the client can distinguish "this page is dead" from
// "the server is restarting" instead of reconnect-looping forever.
const (
	RefusalStaleInstance = "stale_instance"
	RefusalBadToken      = "bad_token"
)

// Config describes the existing muxd endpoints the browser gateway connects
// to. muxd remains the owner of PTYs and terminal interpretation.
type Config struct {
	Session         string
	ControlEndpoint string
	DataEndpoint    string
	HistoryDir      string
	// Instance identifies this gateway process. An empty value is replaced by
	// a freshly generated id, so every process always has one.
	Instance string
	// Token, when set, must accompany every attach and every history read.
	// An empty token means the gateway is open to anything that can reach the
	// listen address.
	Token string
	// WorkspaceStore and WorkspaceAuthorize are optional. When configured,
	// /workspace-objects exposes the capability-scoped browser object API.
	// Cloud credentials stay behind this handler and never reach the browser.
	WorkspaceStore     workspacebackend.Store
	WorkspaceAuthorize workspacebackend.Authorize
}

type Server struct {
	root     context.Context
	cfg      Config
	instance string
	up       websocket.Upgrader
}

func New(parent context.Context, cfg Config) *Server {
	instance := strings.TrimSpace(cfg.Instance)
	if instance == "" {
		generated, err := randomID(8)
		if err != nil {
			// A gateway without an identity would silently re-enable the
			// stale-tab takeover, so fall back to a process-unique value
			// rather than to the empty string.
			generated = fmt.Sprintf("t%d", time.Now().UnixNano())
		}
		instance = generated
	}
	return &Server{
		root:     parent,
		cfg:      cfg,
		instance: instance,
		up: websocket.Upgrader{
			ReadBufferSize:  4 << 10,
			WriteBufferSize: 16 << 10,
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" { // native clients and curl-like smoke tests
					return true
				}
				u, err := url.Parse(origin)
				return err == nil && u.Host == r.Host
			},
		},
	}
}

// Instance returns the identity this process embeds in the page it serves and
// requires back on the websocket handshake.
func (s *Server) Instance() string { return s.instance }

func (s *Server) Handler() http.Handler {
	files := http.FileServer(http.FS(webui.FS))
	muxHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			s.serveIndex(w, r)
			return
		}
		if r.URL.Path == "/workspace" || r.URL.Path == "/workspace/" {
			data, err := webui.FS.ReadFile("workspace.html")
			if err != nil {
				http.Error(w, "workspace unavailable", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(data)
			return
		}
		files.ServeHTTP(w, r)
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/instance", s.handleInstance)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/ws", s.handleWebSocket)
	if s.cfg.WorkspaceStore != nil {
		objects := workspacebackend.Handler(workspacebackend.HTTPConfig{
			Store: s.cfg.WorkspaceStore, Authorize: s.cfg.WorkspaceAuthorize,
		})
		mux.Handle("/workspace-objects/", http.StripPrefix("/workspace-objects", objects))
	}
	mux.Handle("/", muxHandler)
	return mux
}

// serveIndex stamps this process's instance id, and the token when the caller
// already presented a valid one, into the page. A page served without a valid
// token still renders: it reports the missing token instead of failing with an
// opaque connection error.
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	data, err := webui.FS.ReadFile("index.html")
	if err != nil {
		http.Error(w, "index unavailable", http.StatusInternalServerError)
		return
	}
	token := ""
	if s.cfg.Token != "" && s.tokenAccepted(r) {
		token = s.cfg.Token
	}
	page := strings.NewReplacer(
		instancePlaceholder, html.EscapeString(s.instance),
		tokenPlaceholder, html.EscapeString(token),
	).Replace(string(data))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A cached page would reintroduce exactly the staleness this identity is
	// meant to catch.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(page))
}

// handleInstance lets a page that just lost its socket ask why. It is
// deliberately unauthenticated: it reveals only this process's identity and
// whether the token the caller already holds is the right one.
func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		Instance      string `json:"instance"`
		TokenRequired bool   `json:"token_required"`
		Authorized    bool   `json:"authorized"`
	}{Instance: s.instance, TokenRequired: s.cfg.Token != "", Authorized: s.tokenAccepted(r)})
}

// tokenAccepted reports whether the request carries this instance's token. A
// gateway configured without a token accepts everything.
func (s *Server) tokenAccepted(r *http.Request) bool {
	if s.cfg.Token == "" {
		return true
	}
	presented := r.URL.Query().Get("token")
	if presented == "" {
		presented = r.Header.Get("X-Shenmux-Token")
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.cfg.Token)) == 1
}

type refusal struct {
	Reason  string
	Message string
	Status  int
}

// checkHandshake gates attachment. Instance identity is checked first because
// it names the specific failure a human hits: a tab from a dead gateway.
func (s *Server) checkHandshake(r *http.Request) *refusal {
	presented := r.URL.Query().Get("instance")
	if presented != s.instance {
		message := fmt.Sprintf("this page belongs to a previous shenmux web session (page instance %q, this gateway is %q); reload the page", presented, s.instance)
		if presented == "" {
			message = "this page carried no shenmux web instance id, so it belongs to a previous session or an older build; reload the page"
		}
		return &refusal{Reason: RefusalStaleInstance, Message: message, Status: http.StatusConflict}
	}
	if !s.tokenAccepted(r) {
		return &refusal{Reason: RefusalBadToken, Message: "this gateway requires its access token; open the URL printed by shenmux web", Status: http.StatusUnauthorized}
	}
	return nil
}

func writeRefusal(w http.ResponseWriter, ref *refusal) {
	w.Header().Set("X-Shenmux-Refusal", ref.Reason)
	http.Error(w, ref.Message, ref.Status)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Scrollback is session content; it gets the same gate as attaching.
	if !s.tokenAccepted(r) {
		writeRefusal(w, &refusal{Reason: RefusalBadToken, Message: "this gateway requires its access token; open the URL printed by shenmux web", Status: http.StatusUnauthorized})
		return
	}
	if s.cfg.HistoryDir == "" {
		http.Error(w, "history is not configured", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if session := r.URL.Query().Get("session"); session != "" {
		record, err := history.Load(s.cfg.HistoryDir, session)
		if os.IsNotExist(err) {
			http.Error(w, "history not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "invalid history", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(record)
		return
	}
	records, err := history.List(s.cfg.HistoryDir)
	if err != nil {
		http.Error(w, "unable to list history", http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(records)
}

type command struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

type envelope struct {
	Type         string        `json:"type"`
	Command      string        `json:"command,omitempty"`
	Session      string        `json:"session,omitempty"`
	ClientID     string        `json:"client_id,omitempty"`
	Meta         protocol.Meta `json:"meta,omitempty"`
	Current      any           `json:"current,omitempty"`
	HistoryLimit int           `json:"history_limit,omitempty"`
	Seq          uint64        `json:"seq,omitempty"`
	Delta        any           `json:"delta,omitempty"`
	ControlOwner string        `json:"control_owner,omitempty"`
	ExitCode     int           `json:"exit_code,omitempty"`
	Error        string        `json:"error,omitempty"`
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Refuse before the upgrade. A stale tab must never reach the attach and
	// control-lease path, not even briefly.
	if ref := s.checkHandshake(r); ref != nil {
		writeRefusal(w, ref)
		return
	}
	conn, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(s.root)
	defer cancel()

	id, err := newClientID()
	if err != nil {
		s.writeError(conn, err)
		return
	}
	mux, err := client.New(ctx, client.Config{
		Session: s.cfg.Session, ClientID: id,
		ControlEndpoint: s.cfg.ControlEndpoint, DataEndpoint: s.cfg.DataEndpoint,
		EventBuffer: 4096,
	})
	if err != nil {
		s.writeError(conn, err)
		return
	}

	callCtx, callCancel := context.WithTimeout(ctx, commandTimeout)
	snapshot, err := mux.Attach(callCtx)
	callCancel()
	if err != nil {
		s.writeError(conn, err)
		return
	}
	// A browser tab is generally the active operator. If another client owns
	// the lease, the control button in the UI can retry acquisition.
	callCtx, callCancel = context.WithTimeout(ctx, commandTimeout)
	_, _ = mux.AcquireControl(callCtx)
	callCancel()

	var writeMu sync.Mutex
	write := func(msg envelope) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(commandTimeout))
		return conn.WriteJSON(msg)
	}
	if err := write(snapshotEnvelope(s.cfg.Session, mux.ID(), snapshot)); err != nil {
		return
	}

	pubDone := make(chan struct{})
	go func() {
		defer close(pubDone)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-mux.Events():
				if !ok {
					return
				}
				if err := write(publicationEnvelope(msg)); err != nil {
					cancel()
					return
				}
			case err, ok := <-mux.Errors():
				if !ok {
					return
				}
				if err != nil {
					_ = write(envelope{Type: "error", Error: err.Error()})
				}
			}
		}
	}()

	defer func() {
		// Close the client while its context is still live so its normal detach
		// handshake has a chance to release the control lease.
		_ = mux.Close()
		cancel()
		_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		<-pubDone
	}()
	for {
		var cmd command
		if err := conn.ReadJSON(&cmd); err != nil {
			return
		}
		if err := s.handleCommand(ctx, mux, cmd, write); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			if write(envelope{Type: "error", Error: err.Error()}) != nil {
				return
			}
		}
	}
}

func (s *Server) handleCommand(ctx context.Context, mux *client.Client, cmd command, write func(envelope) error) error {
	callCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	switch strings.ToLower(cmd.Type) {
	case "input":
		return mux.SendInput(callCtx, []byte(cmd.Data))
	case "resize":
		return mux.Resize(callCtx, cmd.Cols, cmd.Rows)
	case "acquire":
		meta, err := mux.AcquireControl(callCtx)
		if err != nil {
			return err
		}
		return write(envelope{Type: "command", Command: "acquire", Meta: meta})
	case "release":
		meta, err := mux.ReleaseControl(callCtx)
		if err != nil {
			return err
		}
		return write(envelope{Type: "command", Command: "release", Meta: meta})
	case "ping":
		meta, err := mux.Ping(callCtx)
		if err != nil {
			return err
		}
		return write(envelope{Type: "command", Command: "ping", Meta: meta})
	case "resync":
		snapshot, err := mux.Resync(callCtx)
		if err != nil {
			return err
		}
		return write(snapshotEnvelope(s.cfg.Session, mux.ID(), snapshot))
	case "detach":
		return mux.Detach(callCtx)
	default:
		return fmt.Errorf("unknown browser command %q", cmd.Type)
	}
}

func snapshotEnvelope(session, id string, snapshot client.Snapshot) envelope {
	return envelope{Type: "snapshot", Session: session, ClientID: id,
		Meta: snapshot.Meta, Current: snapshot.Current, HistoryLimit: snapshot.Archive.HistoryLimit}
}

func publicationEnvelope(msg protocol.Message) envelope {
	switch msg.Kind {
	case protocol.KindDelta:
		delta, err := protocol.DecodeDelta(msg.Payload)
		if err != nil {
			return envelope{Type: "error", Error: err.Error()}
		}
		return envelope{Type: "delta", Seq: msg.Meta.Seq, Delta: delta}
	case protocol.KindControl:
		return envelope{Type: "control", Seq: msg.Meta.Seq, ControlOwner: msg.Meta.ControlOwner}
	case protocol.KindExit:
		return envelope{Type: "exit", Seq: msg.Meta.Seq, ExitCode: msg.Meta.ExitCode}
	default:
		return envelope{Type: "error", Error: fmt.Sprintf("unexpected publication %q", msg.Kind)}
	}
}

func (s *Server) writeError(conn *websocket.Conn, err error) {
	_ = conn.SetWriteDeadline(time.Now().Add(commandTimeout))
	_ = conn.WriteJSON(envelope{Type: "error", Error: err.Error()})
}

func newClientID() (string, error) {
	id, err := randomID(12)
	if err != nil {
		return "", fmt.Errorf("generate browser client id: %w", err)
	}
	return "web-" + id, nil
}

func randomID(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NewToken returns an access token for one gateway process. It lives here so
// the command and the gateway agree on the shape of the secret in the URL.
func NewToken() (string, error) {
	token, err := randomID(16)
	if err != nil {
		return "", fmt.Errorf("generate gateway access token: %w", err)
	}
	return token, nil
}
