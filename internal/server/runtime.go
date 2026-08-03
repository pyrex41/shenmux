package server

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pyrex41/shenmux/internal/naming"
	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/screen"
	"github.com/pyrex41/shenmux/internal/shenguard"
	"github.com/pyrex41/shenmux/internal/term"
)

const (
	MaxInputBytes       = 1 << 20
	DefaultControlLease = 8 * time.Second
)

type PTY interface {
	io.Reader
	io.Writer
	SetSize(shenguard.Dimensions) error
}

type Publisher interface{ Publish(protocol.Message) error }

type RuntimeConfig struct {
	Session      string
	Dimensions   shenguard.Dimensions
	PTY          PTY
	Terminal     term.Terminal
	Publisher    Publisher
	StoreLimits  protocol.StoreLimits
	ControlLease time.Duration
}

type Runtime struct {
	mu sync.Mutex

	session string
	model   shenguard.Session
	pty     PTY
	writer  *ptyWriter
	term    term.Terminal
	state   screen.State
	store   *protocol.Store
	events  *statePublisher

	lastSeen     map[shenguard.ClientID]time.Time
	controlLease time.Duration
	exitCode     int
	closed       bool
	closeOnce    sync.Once
	closeErr     error
	fatal        chan error
	fatalOnce    sync.Once
}

func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	if err := naming.ValidateSession(cfg.Session); err != nil {
		return nil, err
	}
	if cfg.PTY == nil || cfg.Terminal == nil || cfg.Publisher == nil {
		return nil, errors.New("runtime dependencies must not be nil")
	}
	if cfg.Dimensions.IsZero() {
		return nil, errors.New("runtime dimensions must be positive")
	}
	if cfg.StoreLimits == (protocol.StoreLimits{}) {
		cfg.StoreLimits = protocol.DefaultStoreLimits()
	}
	if cfg.ControlLease <= 0 {
		cfg.ControlLease = DefaultControlLease
	}
	model, err := shenguard.NewSession(cfg.Dimensions)
	if err != nil {
		return nil, err
	}
	frame, err := cfg.Terminal.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("initial terminal snapshot: %w", err)
	}
	if frame.Cols != cfg.Dimensions.Cols() || frame.Rows != cfg.Dimensions.Rows() {
		return nil, fmt.Errorf("terminal dimensions %dx%d do not match runtime %dx%d", frame.Cols, frame.Rows, cfg.Dimensions.Cols(), cfg.Dimensions.Rows())
	}
	state := screen.State{Frame: frame}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	store, err := protocol.NewStore(protocol.Checkpoint{Screen: state}, cfg.StoreLimits)
	if err != nil {
		return nil, err
	}
	writer, err := newPTYWriter(cfg.PTY)
	if err != nil {
		return nil, err
	}
	events, err := newStatePublisher(cfg.Publisher)
	if err != nil {
		_ = writer.Close()
		return nil, err
	}
	return &Runtime{
		session: cfg.Session, model: model, pty: cfg.PTY, writer: writer,
		term: cfg.Terminal, state: state, store: store, events: events,
		lastSeen: make(map[shenguard.ClientID]time.Time), controlLease: cfg.ControlLease,
		fatal: make(chan error, 1),
	}, nil
}

func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.closeErr = errors.Join(r.writer.Close(), r.events.Close())
	})
	return r.closeErr
}

func (r *Runtime) WriterErrors() <-chan error    { return r.writer.Errors() }
func (r *Runtime) PublisherErrors() <-chan error { return r.events.Errors() }
func (r *Runtime) FatalErrors() <-chan error     { return r.fatal }

// AttachSnapshot freezes a canonical checkpoint-plus-tail archive at one
// sequence boundary. Compression occurs outside the runtime lock; publications
// with higher sequence numbers queue on the already-ready SUB socket.
func (r *Runtime) AttachSnapshot(cid shenguard.ClientID) (protocol.Message, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return protocol.Message{}, errors.New("runtime is closed")
	}
	locked, err := shenguard.BeginSnapshot(r.model)
	if err != nil {
		r.mu.Unlock()
		return protocol.Message{}, err
	}
	archive := r.store.Snapshot()
	if archive.LastSeq() != locked.LastSeq().Uint64() {
		r.mu.Unlock()
		return protocol.Message{}, fmt.Errorf("store/model sequence mismatch: store=%d model=%d", archive.LastSeq(), locked.LastSeq().Uint64())
	}
	stateAtBoundary := r.state.Clone()
	ownerAtBoundary := ownerString(locked)
	exitedAtBoundary := locked.Exited()
	exitCodeAtBoundary := r.exitCode
	cursor := stateAtBoundary.Frame.Cursor
	snapshot, err := shenguard.NewSnapshot(
		locked.LastSeq(), locked.Dim(), int(cursor.X), int(cursor.Y), stateAtBoundary.Frame.AltScreen, nil,
	)
	if err == nil {
		r.model, err = shenguard.EndSnapshot(locked, snapshot)
	}
	r.mu.Unlock()
	if err != nil {
		return protocol.Message{}, err
	}

	// Independently derive the archive before it crosses the wire. This catches
	// any divergence between the bounded checkpoint+tail store and the
	// authoritative runtime state at the frozen sequence boundary.
	currentAtBoundary, err := archive.Current()
	if err != nil {
		err = r.fail(fmt.Errorf("derive attach archive: %w", err))
		return protocol.Message{}, err
	}
	if currentAtBoundary.Seq != archive.LastSeq() ||
		!screen.EqualState(currentAtBoundary.Screen, stateAtBoundary) ||
		currentAtBoundary.ControlOwner != ownerAtBoundary ||
		currentAtBoundary.Exited != exitedAtBoundary ||
		currentAtBoundary.ExitCode != exitCodeAtBoundary {
		err = r.fail(errors.New("attach archive does not equal authoritative state at snapshot boundary"))
		return protocol.Message{}, err
	}
	payload, err := protocol.EncodeArchive(archive)
	if err != nil {
		return protocol.Message{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return protocol.Message{}, errors.New("runtime closed while encoding attach snapshot")
	}
	if !shenguard.IsAttached(r.model, cid) {
		r.model, err = shenguard.Attach(r.model, cid)
	}
	if err != nil {
		return protocol.Message{}, err
	}
	r.lastSeen[cid] = time.Now()

	return protocol.Message{
		Kind: protocol.KindAttached,
		Meta: protocol.Meta{
			Version: protocol.Version, Session: r.session, ClientID: cid.String(),
			Seq: archive.LastSeq(), CheckpointSeq: archive.Checkpoint.Seq,
			Cols: stateAtBoundary.Frame.Cols, Rows: stateAtBoundary.Frame.Rows,
			ExitCode: exitCodeAtBoundary, ControlOwner: ownerAtBoundary,
			HasControl: ownerAtBoundary == cid.String(),
		},
		Payload: payload,
	}, nil
}

func (r *Runtime) Touch(cid shenguard.ClientID, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if shenguard.IsAttached(r.model, cid) {
		r.lastSeen[cid] = now
	}
}

func (r *Runtime) Input(cid shenguard.ClientID, payload []byte) error {
	if len(payload) > MaxInputBytes {
		return fmt.Errorf("input exceeds %d bytes", MaxInputBytes)
	}
	if len(payload) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime is closed")
	}
	if !shenguard.AcceptInput(r.model, cid) {
		return inputRejection(r.model, cid)
	}
	r.lastSeen[cid] = time.Now()
	return r.writer.Enqueue(payload)
}

func (r *Runtime) Resize(cid shenguard.ClientID, dim shenguard.Dimensions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime is closed")
	}
	if !shenguard.AcceptResize(r.model, cid) {
		return inputRejection(r.model, cid)
	}
	r.lastSeen[cid] = time.Now()
	oldDim := r.model.Dim()
	if dim == oldDim {
		return nil
	}

	// Prove the pure transition first. Then change the PTY while this mutex
	// prevents any resulting output from being interpreted, resize the single
	// authoritative emulator, and finally commit the derived canonical delta.
	seq, err := shenguard.NextSeq(r.model.LastSeq())
	if err != nil {
		return err
	}
	nextModel, err := shenguard.ApplyResize(r.model, seq, dim)
	if err != nil {
		return err
	}
	if err := r.pty.SetSize(dim); err != nil {
		return fmt.Errorf("resize PTY: %w", err)
	}
	if err := r.term.Resize(dim); err != nil {
		rollbackErr := r.pty.SetSize(oldDim)
		if rollbackErr != nil {
			return r.failLocked(errors.Join(
				fmt.Errorf("resize authoritative terminal: %w", err),
				fmt.Errorf("rollback PTY resize: %w", rollbackErr),
			))
		}
		return fmt.Errorf("resize authoritative terminal: %w", err)
	}

	// From this point the emulator has performed a potentially non-reversible
	// reflow. Any failure is an internal consistency failure, not a recoverable
	// request error; continuing could publish a geometry different from the PTY.
	frame, err := r.term.Snapshot()
	if err != nil {
		return r.failLocked(fmt.Errorf("snapshot resized terminal: %w", err))
	}
	if frame.Cols != dim.Cols() || frame.Rows != dim.Rows() {
		return r.failLocked(fmt.Errorf("terminal resize produced %dx%d, expected %dx%d", frame.Cols, frame.Rows, dim.Cols(), dim.Rows()))
	}
	nextState, delta, err := screen.Advance(r.state, frame, r.store.HistoryLimit())
	if err != nil {
		return r.failLocked(err)
	}
	msg, err := r.deltaMessageFor(nextModel, seq, delta)
	if err != nil {
		return r.failLocked(err)
	}
	event := protocol.Event{Kind: protocol.EventDelta, Seq: seq.Uint64(), Delta: &delta}
	checkpoint := checkpointFor(nextModel, nextState, r.exitCode)
	if err := r.store.Append(event, checkpoint); err != nil {
		return r.failLocked(err)
	}
	r.model, r.state = nextModel, nextState
	return r.enqueueCommittedLocked(msg)
}

func (r *Runtime) HandlePTYOutput(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.model.Exited() {
		return nil
	}
	effects, err := r.term.Feed(payload)
	if err != nil {
		return err
	}
	for _, response := range effects.PTYWrites {
		if err := r.writer.Enqueue(response); err != nil {
			return fmt.Errorf("queue terminal response: %w", err)
		}
	}
	frame, err := r.term.Snapshot()
	if err != nil {
		return err
	}
	nextState, delta, err := screen.Advance(r.state, frame, r.store.HistoryLimit())
	if err != nil {
		return err
	}
	if screen.EqualState(nextState, r.state) {
		return nil
	}
	seq, err := shenguard.NextSeq(r.model.LastSeq())
	if err != nil {
		return err
	}
	nextModel, err := shenguard.ApplyDelta(r.model, seq)
	if err != nil {
		return err
	}
	msg, err := r.deltaMessageFor(nextModel, seq, delta)
	if err != nil {
		return err
	}
	event := protocol.Event{Kind: protocol.EventDelta, Seq: seq.Uint64(), Delta: &delta}
	checkpoint := checkpointFor(nextModel, nextState, r.exitCode)
	if err := r.store.Append(event, checkpoint); err != nil {
		return err
	}
	r.model, r.state = nextModel, nextState
	return r.enqueueCommittedLocked(msg)
}

func (r *Runtime) AcquireControl(cid shenguard.ClientID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime is closed")
	}
	if shenguard.HasControl(r.model, cid) {
		r.lastSeen[cid] = time.Now()
		return nil
	}
	nextModel, err := shenguard.AcquireControl(r.model, cid)
	if err != nil {
		return err
	}
	msg, err := r.commitControlLocked(nextModel, cid.String())
	if err != nil {
		return err
	}
	r.lastSeen[cid] = time.Now()
	return r.enqueueCommittedLocked(msg)
}

func (r *Runtime) ReleaseControl(cid shenguard.ClientID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime is closed")
	}
	nextModel, err := shenguard.ReleaseControl(r.model, cid)
	if err != nil {
		return err
	}
	msg, err := r.commitControlLocked(nextModel, "")
	if err != nil {
		return err
	}
	return r.enqueueCommittedLocked(msg)
}

func (r *Runtime) Detach(cid shenguard.ClientID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime is closed")
	}
	oldOwner := ownerString(r.model)
	nextModel, err := shenguard.Detach(r.model, cid)
	if err != nil {
		return err
	}
	delete(r.lastSeen, cid)
	newOwner := ownerString(nextModel)
	if oldOwner == newOwner {
		r.model = nextModel
		return nil
	}
	msg, err := r.commitControlLocked(nextModel, newOwner)
	if err != nil {
		return err
	}
	return r.enqueueCommittedLocked(msg)
}

// ReapExpired releases a stale control lease. Observers remain attached; only
// the exclusive mutation capability expires.
func (r *Runtime) ReapExpired(now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	owner, ok := r.model.ControlOwner()
	if !ok {
		return nil
	}
	last := r.lastSeen[owner]
	if !last.IsZero() && now.Sub(last) <= r.controlLease {
		return nil
	}
	nextModel, err := shenguard.ReleaseControl(r.model, owner)
	if err != nil {
		return err
	}
	msg, err := r.commitControlLocked(nextModel, "")
	if err != nil {
		return err
	}
	return r.enqueueCommittedLocked(msg)
}

func (r *Runtime) HandleExit(exitCode int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.model.Exited() {
		return nil
	}
	seq, err := shenguard.NextSeq(r.model.LastSeq())
	if err != nil {
		return err
	}
	nextModel, err := shenguard.ApplyExit(r.model, seq)
	if err != nil {
		return err
	}
	event := protocol.Event{Kind: protocol.EventExit, Seq: seq.Uint64(), ExitCode: exitCode}
	checkpoint := checkpointFor(nextModel, r.state, exitCode)
	if err := r.store.Append(event, checkpoint); err != nil {
		return err
	}
	r.model, r.exitCode = nextModel, exitCode
	msg := protocol.Message{Kind: protocol.KindExit, Meta: protocol.Meta{Version: protocol.Version, Session: r.session, Seq: seq.Uint64(), ExitCode: exitCode, ControlOwner: ownerString(nextModel)}}
	return r.enqueueCommittedLocked(msg)
}

func (r *Runtime) commitControlLocked(nextModel shenguard.Session, owner string) (protocol.Message, error) {
	seq, err := shenguard.NextSeq(r.model.LastSeq())
	if err != nil {
		return protocol.Message{}, err
	}
	nextModel, err = shenguard.ApplyControl(nextModel, seq)
	if err != nil {
		return protocol.Message{}, err
	}
	event := protocol.Event{Kind: protocol.EventControl, Seq: seq.Uint64(), ControlOwner: owner}
	checkpoint := checkpointFor(nextModel, r.state, r.exitCode)
	if err := r.store.Append(event, checkpoint); err != nil {
		return protocol.Message{}, err
	}
	r.model = nextModel
	return protocol.Message{Kind: protocol.KindControl, Meta: protocol.Meta{Version: protocol.Version, Session: r.session, Seq: seq.Uint64(), ControlOwner: owner}}, nil
}

func (r *Runtime) deltaMessageFor(model shenguard.Session, seq shenguard.SeqNo, delta screen.Delta) (protocol.Message, error) {
	payload, err := protocol.EncodeDelta(delta)
	if err != nil {
		return protocol.Message{}, err
	}
	return protocol.Message{Kind: protocol.KindDelta, Meta: protocol.Meta{Version: protocol.Version, Session: r.session, Seq: seq.Uint64(), Cols: delta.Cols, Rows: delta.Rows, ControlOwner: ownerString(model)}, Payload: payload}, nil
}

// enqueueCommittedLocked is called only after model, canonical screen, and
// bounded Store have committed the same sequence. Enqueue is deliberately
// non-blocking and occurs under r.mu so concurrent commits cannot reorder at
// the handoff. Failure is terminal: the missing sequence cannot be repaired by
// later publications, so the runtime rejects all further work.
func (r *Runtime) enqueueCommittedLocked(msg protocol.Message) error {
	if err := r.events.Enqueue(msg); err != nil {
		return r.failLocked(fmt.Errorf("enqueue committed state seq %d: %w", msg.Meta.Seq, err))
	}
	return nil
}

func (r *Runtime) fail(err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failLocked(err)
}

func (r *Runtime) failLocked(err error) error {
	if err == nil {
		return nil
	}
	r.closed = true
	r.fatalOnce.Do(func() {
		r.fatal <- err
	})
	return err
}

type Status struct {
	Session      string
	Seq          uint64
	Dim          shenguard.Dimensions
	Clients      []shenguard.ClientID
	ControlOwner string
	Exited       bool
	ExitCode     int
	Store        protocol.StoreStats
}

func (r *Runtime) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Status{Session: r.session, Seq: r.model.LastSeq().Uint64(), Dim: r.model.Dim(), Clients: r.model.Clients(), ControlOwner: ownerString(r.model), Exited: r.model.Exited(), ExitCode: r.exitCode, Store: r.store.Stats()}
}

func checkpointFor(model shenguard.Session, state screen.State, exitCode int) protocol.Checkpoint {
	return protocol.Checkpoint{Seq: model.LastSeq().Uint64(), Screen: state.Clone(), ControlOwner: ownerString(model), Exited: model.Exited(), ExitCode: exitCode}
}

func ownerString(model shenguard.Session) string {
	owner, ok := model.ControlOwner()
	if !ok {
		return ""
	}
	return owner.String()
}

func inputRejection(model shenguard.Session, cid shenguard.ClientID) error {
	if !shenguard.IsAttached(model, cid) {
		return shenguard.ErrNotAttached
	}
	if model.Exited() {
		return shenguard.ErrExited
	}
	return shenguard.ErrNoControl
}
