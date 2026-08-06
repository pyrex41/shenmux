package webgateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
)

const commandTimeout = 5 * time.Second

// Config describes the existing muxd endpoints the browser gateway connects
// to. muxd remains the owner of PTYs and terminal interpretation.
type Config struct {
	Session         string
	ControlEndpoint string
	DataEndpoint    string
	HistoryDir      string
}

type Server struct {
	root context.Context
	cfg  Config
	up   websocket.Upgrader
}

func New(parent context.Context, cfg Config) *Server {
	return &Server{
		root: parent,
		cfg:  cfg,
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

func (s *Server) Handler() http.Handler {
	files := http.FileServer(http.FS(webui.FS))
	muxHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			data, err := webui.FS.ReadFile("index.html")
			if err != nil {
				http.Error(w, "index unavailable", http.StatusInternalServerError)
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
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/ws", s.handleWebSocket)
	mux.Handle("/", muxHandler)
	return mux
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate browser client id: %w", err)
	}
	return "web-" + hex.EncodeToString(b), nil
}
