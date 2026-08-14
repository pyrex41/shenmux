package server

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pyrex41/shenmux/naming"
	"github.com/pyrex41/shenmux/protocol"
	"github.com/pyrex41/shenmux/screen"
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

// echoReporter is the optional half of PTY that can report the line
// discipline's ECHO bit. It is optional so existing PTY fakes keep working,
// and the absence of it fails closed: echo stays false, so clients never
// predict local echo for a PTY whose termios we cannot read.
type echoReporter interface {
	EchoEnabled() (bool, error)
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
	// started anchors the millisecond readings handed to the Shen model. The
	// model compares two numbers and never reads a clock, so the host owes it a
	// monotonic, nonnegative origin.
	started  time.Time
	exitCode int
	closed   bool

	// echoEnabled mirrors the PTY's termios ECHO bit. It is stamped onto every
	// frame the runtime publishes, because the emulator sees only the escape
	// stream and cannot know it. False until proven true, so a PTY that cannot
	// report echo never invites a client to predict.
	echoEnabled bool
	closeOnce   sync.Once
	closeErr    error
	fatal       chan error
	fatalOnce   sync.Once
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
	runtime := &Runtime{
		session: cfg.Session, model: model, pty: cfg.PTY, writer: writer,
		term: cfg.Terminal, state: state, store: store, events: events,
		lastSeen: make(map[shenguard.ClientID]time.Time), controlLease: cfg.ControlLease,
		started: time.Now(), fatal: make(chan error, 1),
	}
	return runtime, nil
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

func reduceError(reason shenguard.Reason) error {
	switch reason {
	case shenguard.ReasonAlreadyAttached:
		return shenguard.ErrAlreadyAttached
	case shenguard.ReasonNotAttached:
		return shenguard.ErrNotAttached
	case shenguard.ReasonWriterLocked:
		return shenguard.ErrWriterLocked
	case shenguard.ReasonWriterUnlocked:
		return shenguard.ErrWriterUnlocked
	case shenguard.ReasonSequence:
		return shenguard.ErrSequence
	case shenguard.ReasonExited:
		return shenguard.ErrExited
	case shenguard.ReasonNoControl:
		return shenguard.ErrNoControl
	case shenguard.ReasonControlOwned:
		return shenguard.ErrControlOwned
	case shenguard.ReasonSnapshotMismatch:
		return shenguard.ErrSnapshotMismatch
	case shenguard.ReasonSnapshotOwnerMismatch:
		return shenguard.ErrSnapshotOwnerMismatch
	case shenguard.ReasonInvalidDimensions:
		return shenguard.ErrInvalidDimensions
	case shenguard.ReasonUnknownCommand:
		return shenguard.ErrUnknownCommand
	default:
		return fmt.Errorf("Shen reducer rejected command: %s", reason)
	}
}

// clockLocked converts host time into the two numbers the Shen model compares.
// Both are floored: a negative reading and a sub-millisecond lease are host
// mistakes, and neither may be allowed to express "every owner is instantly
// stale" -- which the clock datatype refuses to represent anyway.
func (r *Runtime) clockLocked(now time.Time) (shenguard.Clock, error) {
	elapsed := now.Sub(r.started)
	if elapsed < 0 {
		elapsed = 0
	}
	lease := uint64(r.controlLease / time.Millisecond)
	if lease == 0 {
		lease = 1
	}
	return shenguard.NewClock(uint64(elapsed/time.Millisecond), lease)
}

func (r *Runtime) reduceLocked(command shenguard.Command) (shenguard.Result, error) {
	clock, err := r.clockLocked(time.Now())
	if err != nil {
		return shenguard.Result{}, err
	}
	result, err := shenguard.Reduce(r.model, clock, command)
	if err != nil {
		return shenguard.Result{}, err
	}
	if !result.Accepted {
		return shenguard.Result{}, reduceError(result.Reason)
	}
	return result, nil
}

func expectEffect(effects []shenguard.Effect, kind shenguard.EffectKind) (shenguard.Effect, error) {
	for _, effect := range effects {
		if effect.Kind == kind {
			return effect, nil
		}
	}
	return shenguard.Effect{}, fmt.Errorf("Shen reducer omitted %q effect", kind)
}

// AttachSnapshot freezes a canonical checkpoint-plus-tail archive at one
// sequence boundary. Compression occurs outside the runtime lock; publications
// with higher sequence numbers queue on the already-ready SUB socket.
func (r *Runtime) AttachSnapshot(cid shenguard.ClientID) (protocol.Message, error) {
	return r.snapshot(cid, false)
}

// ResyncSnapshot requires existing membership through the explicit Shen
// resync command before running the shared snapshot barrier.
func (r *Runtime) ResyncSnapshot(cid shenguard.ClientID) (protocol.Message, error) {
	return r.snapshot(cid, true)
}

func (r *Runtime) snapshot(cid shenguard.ClientID, requireAttached bool) (protocol.Message, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return protocol.Message{}, errors.New("runtime is closed")
	}
	if requireAttached {
		resync, err := r.reduceLocked(shenguard.Command{Kind: shenguard.CommandResync, Client: cid})
		if err != nil {
			r.mu.Unlock()
			return protocol.Message{}, err
		}
		if len(resync.Effects) != 0 {
			r.mu.Unlock()
			return protocol.Message{}, errors.New("Shen reducer returned effects for resync authorization")
		}
		r.model = resync.State
	}
	// The same Shen-owned barrier handles both first attachment and resync.
	// FinishAttach retains an existing member or adds a new one atomically.
	begin, err := r.reduceLocked(shenguard.Command{Kind: shenguard.CommandBeginAttach, Client: cid})
	if err != nil {
		r.mu.Unlock()
		return protocol.Message{}, err
	}
	if _, err := expectEffect(begin.Effects, shenguard.EffectCaptureSnapshot); err != nil {
		r.mu.Unlock()
		return protocol.Message{}, err
	}
	locked := begin.State
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
		clock, clockErr := r.clockLocked(time.Now())
		finish := shenguard.Result{}
		finishErr := clockErr
		if finishErr == nil {
			finish, finishErr = shenguard.Reduce(locked, clock, shenguard.Command{Kind: shenguard.CommandFinishAttach, Client: cid, Snapshot: snapshot})
		}
		if finishErr == nil && !finish.Accepted {
			finishErr = reduceError(finish.Reason)
		}
		err = finishErr
		if err == nil {
			reply, effectErr := expectEffect(finish.Effects, shenguard.EffectReply)
			if effectErr != nil || reply.Client != cid || reply.Token != 0 {
				if effectErr == nil {
					effectErr = errors.New("Shen reducer returned invalid attach reply effect")
				}
				err = effectErr
			} else {
				r.model = finish.State
			}
		}
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
		return protocol.Message{}, r.fail(fmt.Errorf("encode attach archive: %w", err))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return protocol.Message{}, r.failLocked(errors.New("runtime closed while encoding attach snapshot"))
	}
	if !shenguard.IsAttached(r.model, cid) {
		return protocol.Message{}, errors.New("client detached while encoding attach snapshot")
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
	result, err := r.reduceLocked(shenguard.Command{Kind: shenguard.CommandInput, Client: cid, Token: shenguard.TokenID(1)})
	if err != nil {
		// Preserve the historical input error contract: a non-owner observer
		// receives ErrNoControl even though Shen distinguishes control-owned
		// from no-control rejection reasons for other commands.
		if errors.Is(err, shenguard.ErrControlOwned) {
			return shenguard.ErrNoControl
		}
		return err
	}
	effect, err := expectEffect(result.Effects, shenguard.EffectWritePTY)
	if err != nil || effect.Token != shenguard.TokenID(1) || effect.Client != cid {
		if err == nil {
			err = errors.New("Shen reducer returned invalid input effect")
		}
		return err
	}
	r.lastSeen[cid] = time.Now()
	if err := r.writer.Enqueue(payload); err != nil {
		return err
	}
	r.model = result.State
	return nil
}

func (r *Runtime) Resize(cid shenguard.ClientID, dim shenguard.Dimensions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime is closed")
	}
	result, err := r.reduceLocked(shenguard.Command{Kind: shenguard.CommandResize, Client: cid, Dim: dim})
	if err != nil {
		return err
	}
	r.lastSeen[cid] = time.Now()
	oldDim := r.model.Dim()
	if len(result.Effects) == 0 {
		r.model = result.State
		return nil
	}
	resizeEffect, err := expectEffect(result.Effects, shenguard.EffectResizePTY)
	if err != nil || resizeEffect.Dimensions != dim {
		if err == nil {
			err = errors.New("Shen reducer returned invalid resize effect")
		}
		return err
	}
	publishEffect, err := expectEffect(result.Effects, shenguard.EffectPublish)
	if err != nil || publishEffect.EventKind != "delta" || publishEffect.Seq != result.State.LastSeq() {
		if err == nil {
			err = errors.New("Shen reducer returned invalid resize publication")
		}
		return err
	}
	seq := publishEffect.Seq
	nextModel := result.State

	// Prove the pure transition first. Then change the PTY while this mutex
	// prevents any resulting output from being interpreted, resize the single
	// authoritative emulator, and finally commit the derived canonical delta.
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
	frame, err := r.snapshotLocked()
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
	if r.closed {
		return nil
	}
	if r.model.Exited() {
		_, err := r.reduceLocked(shenguard.Command{Kind: shenguard.CommandPTYOutput, Token: shenguard.TokenID(1)})
		if errors.Is(err, shenguard.ErrExited) {
			return nil
		}
		return err
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
	return r.publishFrameLocked()
}

// snapshotLocked takes the emulator's frame and stamps the PTY-owned echo bit
// onto it. The emulator interprets only the escape stream, so ECHO -- which is
// a termios property of the line discipline -- has to be merged in here, the
// one place that owns both the emulator and the PTY.
func (r *Runtime) snapshotLocked() (screen.Frame, error) {
	frame, err := r.term.Snapshot()
	if err != nil {
		return screen.Frame{}, err
	}
	frame.Modes.Echo = r.echoEnabled
	return frame, nil
}

// publishFrameLocked advances published state to the emulator's current frame
// and emits a delta when anything changed. PTY output is the usual reason to
// call it, but not the only one: the echo bit can flip with no output at all.
func (r *Runtime) publishFrameLocked() error {
	frame, err := r.snapshotLocked()
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
	result, err := r.reduceLocked(shenguard.Command{Kind: shenguard.CommandPTYOutput, Token: shenguard.TokenID(1)})
	if err != nil {
		return err
	}
	publishEffect, err := expectEffect(result.Effects, shenguard.EffectPublish)
	if err != nil || publishEffect.EventKind != "delta" || publishEffect.Token != shenguard.TokenID(1) || publishEffect.Seq != result.State.LastSeq() {
		if err == nil {
			err = errors.New("Shen reducer returned invalid PTY publication")
		}
		return err
	}
	seq := publishEffect.Seq
	nextModel := result.State
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
	result, err := r.reduceLocked(shenguard.Command{Kind: shenguard.CommandAcquireControl, Client: cid})
	if err != nil {
		return err
	}
	if len(result.Effects) == 0 {
		r.model = result.State
		r.lastSeen[cid] = time.Now()
		return nil
	}
	if _, err := expectEffect(result.Effects, shenguard.EffectPublish); err != nil {
		return err
	}
	msg, err := r.controlMessageFromResultLocked(result)
	if err != nil {
		return err
	}
	r.model = result.State
	r.lastSeen[cid] = time.Now()
	return r.enqueueCommittedLocked(msg)
}

func (r *Runtime) ReleaseControl(cid shenguard.ClientID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime is closed")
	}
	result, err := r.reduceLocked(shenguard.Command{Kind: shenguard.CommandReleaseControl, Client: cid})
	if err != nil {
		return err
	}
	msg, err := r.controlMessageFromResultLocked(result)
	if err != nil {
		return err
	}
	r.model = result.State
	return r.enqueueCommittedLocked(msg)
}

func (r *Runtime) Detach(cid shenguard.ClientID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime is closed")
	}
	oldOwner := ownerString(r.model)
	result, err := r.reduceLocked(shenguard.Command{Kind: shenguard.CommandDetach, Client: cid})
	if err != nil {
		return err
	}
	delete(r.lastSeen, cid)
	newOwner := ownerString(result.State)
	if oldOwner == newOwner {
		r.model = result.State
		return nil
	}
	msg, err := r.controlMessageFromResultLocked(result)
	if err != nil {
		return err
	}
	r.model = result.State
	return r.enqueueCommittedLocked(msg)
}

// PeerLost tears a client down after a per-peer delivery failure. Unlike
// Detach it never reports "not attached": the model makes the teardown
// idempotent, so a caller on a failure path does not have to know how far the
// client had got, and cannot get that judgement wrong one call site at a time.
func (r *Runtime) PeerLost(cid shenguard.ClientID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("runtime is closed")
	}
	oldOwner := ownerString(r.model)
	result, err := r.reduceLocked(shenguard.Command{Kind: shenguard.CommandPeerLost, Client: cid})
	if err != nil {
		return err
	}
	delete(r.lastSeen, cid)
	if oldOwner == ownerString(result.State) {
		r.model = result.State
		return nil
	}
	msg, err := r.controlMessageFromResultLocked(result)
	if err != nil {
		return err
	}
	r.model = result.State
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
	result, err := r.reduceLocked(shenguard.Command{Kind: shenguard.CommandLeaseExpired, Client: owner})
	if err != nil {
		return err
	}
	if len(result.Effects) == 0 {
		return nil
	}
	msg, err := r.controlMessageFromResultLocked(result)
	if err != nil {
		return err
	}
	r.model = result.State
	return r.enqueueCommittedLocked(msg)
}

func (r *Runtime) HandleExit(exitCode int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	if r.model.Exited() {
		_, err := r.reduceLocked(shenguard.Command{Kind: shenguard.CommandProcessExit, Code: exitCode})
		if errors.Is(err, shenguard.ErrExited) {
			return nil
		}
		return err
	}
	result, err := r.reduceLocked(shenguard.Command{Kind: shenguard.CommandProcessExit, Code: exitCode})
	if err != nil {
		return err
	}
	pub, err := expectEffect(result.Effects, shenguard.EffectPublish)
	if err != nil || pub.EventKind != "exit" || pub.Code != exitCode {
		if err == nil {
			err = errors.New("Shen reducer returned invalid exit publication")
		}
		return err
	}
	seq := pub.Seq
	nextModel := result.State
	event := protocol.Event{Kind: protocol.EventExit, Seq: seq.Uint64(), ExitCode: exitCode}
	checkpoint := checkpointFor(nextModel, r.state, exitCode)
	if err := r.store.Append(event, checkpoint); err != nil {
		return err
	}
	r.model, r.exitCode = nextModel, exitCode
	msg := protocol.Message{Kind: protocol.KindExit, Meta: protocol.Meta{Version: protocol.Version, Session: r.session, Seq: seq.Uint64(), ExitCode: exitCode, ControlOwner: ownerString(nextModel)}}
	return r.enqueueCommittedLocked(msg)
}

func (r *Runtime) controlMessageFromResultLocked(result shenguard.Result) (protocol.Message, error) {
	pub, err := expectEffect(result.Effects, shenguard.EffectPublish)
	if err != nil || pub.EventKind != "control" || pub.Seq != result.State.LastSeq() {
		if err == nil {
			err = errors.New("Shen reducer returned invalid control publication")
		}
		return protocol.Message{}, err
	}
	owner := ownerString(result.State)
	event := protocol.Event{Kind: protocol.EventControl, Seq: pub.Seq.Uint64(), ControlOwner: owner}
	checkpoint := checkpointFor(result.State, r.state, r.exitCode)
	if err := r.store.Append(event, checkpoint); err != nil {
		return protocol.Message{}, err
	}
	return protocol.Message{Kind: protocol.KindControl, Meta: protocol.Meta{Version: protocol.Version, Session: r.session, Seq: pub.Seq.Uint64(), ControlOwner: owner}}, nil
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

// RefreshEcho re-reads the PTY's termios ECHO bit and publishes a delta when it
// changed. The daemon polls this because echo can flip with no terminal output
// at all -- `read -s` turns it off before printing anything -- and a client
// that predicts local echo must learn about a password prompt promptly.
//
// A PTY that cannot report echo leaves it false, so prediction stays off rather
// than being invited on a guess.
func (r *Runtime) RefreshEcho() error {
	reporter, ok := r.pty.(echoReporter)
	if !ok {
		return nil
	}
	enabled, err := reporter.EchoEnabled()
	if err != nil {
		// The fd can legitimately be gone while the session is winding down.
		// Treat an unreadable termios as "do not predict" rather than fatal.
		enabled = false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.model.Exited() || r.echoEnabled == enabled {
		return nil
	}
	r.echoEnabled = enabled
	return r.publishFrameLocked()
}
