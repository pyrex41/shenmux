package server

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/screen"
	"github.com/pyrex41/shenmux/internal/shenguard"
	"github.com/pyrex41/shenmux/internal/term"
)

type fakePTY struct {
	mu      sync.Mutex
	written bytes.Buffer
	dim     shenguard.Dimensions
}

func (*fakePTY) Read([]byte) (int, error) { return 0, nil }
func (p *fakePTY) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.written.Write(b)
}
func (p *fakePTY) SetSize(dim shenguard.Dimensions) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dim = dim
	return nil
}
func (p *fakePTY) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.written.String()
}

type recordPublisher struct {
	mu       sync.Mutex
	messages []protocol.Message
}

func (p *recordPublisher) Publish(msg protocol.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.messages = append(p.messages, msg)
	return nil
}
func (p *recordPublisher) Messages() []protocol.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]protocol.Message(nil), p.messages...)
}

func newTestRuntime(t *testing.T) (*Runtime, *fakePTY, *recordPublisher) {
	t.Helper()
	return newTestRuntimeWith(t, protocol.DefaultStoreLimits(), 50*time.Millisecond)
}

func newTestRuntimeWith(t *testing.T, limits protocol.StoreLimits, lease time.Duration) (*Runtime, *fakePTY, *recordPublisher) {
	t.Helper()
	dim, _ := shenguard.NewDimensions(80, 24)
	pty := &fakePTY{dim: dim}
	pub := &recordPublisher{}
	runtime, err := NewRuntime(RuntimeConfig{
		Session: "test", Dimensions: dim, PTY: pty, Terminal: term.NewBasic(dim),
		Publisher: pub, StoreLimits: limits, ControlLease: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime, pty, pub
}

func testCID(t *testing.T, id ...string) shenguard.ClientID {
	t.Helper()
	value := "client-1"
	if len(id) != 0 {
		value = id[0]
	}
	cid, err := shenguard.NewClientID(value)
	if err != nil {
		t.Fatal(err)
	}
	return cid
}

func TestRuntimeAttachControlInputOutputResizeAndSnapshot(t *testing.T) {
	runtime, pty, pub := newTestRuntime(t)
	cid := testCID(t)
	if err := runtime.Input(cid, []byte("denied")); !errors.Is(err, shenguard.ErrNotAttached) {
		t.Fatalf("input before attach error=%v", err)
	}
	attached, err := runtime.AttachSnapshot(cid)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := protocol.DecodeArchive(attached.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if archive.LastSeq() != 0 || attached.Meta.Seq != 0 || attached.Meta.HasControl {
		t.Fatalf("initial snapshot meta=%+v archive=%+v", attached.Meta, archive)
	}
	if err := runtime.Input(cid, []byte("still denied")); !errors.Is(err, shenguard.ErrNoControl) {
		t.Fatalf("input before control error=%v", err)
	}
	if err := runtime.AcquireControl(cid); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Input(cid, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return pty.String() == "hello" })

	if err := runtime.HandlePTYOutput([]byte("world")); err != nil {
		t.Fatal(err)
	}
	newDim, _ := shenguard.NewDimensions(100, 40)
	if err := runtime.Resize(cid, newDim); err != nil {
		t.Fatal(err)
	}
	if err := runtime.HandleExit(7); err != nil {
		t.Fatal(err)
	}

	eventually(t, func() bool { return len(pub.Messages()) == 4 })
	messages := pub.Messages()
	if len(messages) != 4 {
		t.Fatalf("got %d published messages: %+v", len(messages), messages)
	}
	wantKinds := []protocol.Kind{protocol.KindControl, protocol.KindDelta, protocol.KindDelta, protocol.KindExit}
	for i, msg := range messages {
		if msg.Meta.Seq != uint64(i+1) || msg.Kind != wantKinds[i] {
			t.Fatalf("message %d=%+v want kind=%s seq=%d", i, msg, wantKinds[i], i+1)
		}
	}

	resync, err := runtime.AttachSnapshot(cid)
	if err != nil {
		t.Fatal(err)
	}
	archive, err = protocol.DecodeArchive(resync.Payload)
	if err != nil {
		t.Fatal(err)
	}
	current, err := archive.Current()
	if err != nil {
		t.Fatal(err)
	}
	if current.Seq != 4 || !current.Exited || current.ExitCode != 7 || current.ControlOwner != cid.String() {
		t.Fatalf("unexpected current checkpoint: %+v", current)
	}
	if current.Screen.Frame.Cols != 100 || current.Screen.Frame.Rows != 40 {
		t.Fatalf("unexpected dimensions: %dx%d", current.Screen.Frame.Cols, current.Screen.Frame.Rows)
	}
	if !strings.Contains(frameText(current.Screen), "world") {
		t.Fatalf("screen does not contain output: %q", frameText(current.Screen))
	}
	status := runtime.Status()
	if status.Seq != 4 || !status.Exited || len(status.Clients) != 1 || status.ExitCode != 7 {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestRuntimeExclusiveControlAndLeaseExpiry(t *testing.T) {
	runtime, _, pub := newTestRuntime(t)
	one := testCID(t, "one")
	two := testCID(t, "two")
	for _, cid := range []shenguard.ClientID{one, two} {
		if _, err := runtime.AttachSnapshot(cid); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.AcquireControl(one); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AcquireControl(two); !errors.Is(err, shenguard.ErrControlOwned) {
		t.Fatalf("second acquisition error=%v", err)
	}
	if err := runtime.Input(two, []byte("x")); !errors.Is(err, shenguard.ErrNoControl) {
		t.Fatalf("observer input error=%v", err)
	}
	if err := runtime.ReapExpired(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := runtime.Status().ControlOwner; got != "" {
		t.Fatalf("expired owner=%q", got)
	}
	if err := runtime.AcquireControl(two); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Detach(two); err != nil {
		t.Fatal(err)
	}
	if got := runtime.Status().ControlOwner; got != "" {
		t.Fatalf("detach retained owner=%q", got)
	}
	eventually(t, func() bool { return len(pub.Messages()) == 4 })
	messages := pub.Messages()
	for i, msg := range messages {
		if msg.Kind != protocol.KindControl || msg.Meta.Seq != uint64(i+1) {
			t.Fatalf("control message %d=%+v", i, msg)
		}
	}
}

func TestTerminalQueryResponseIsWrittenExactlyOnceAndNeverReplayed(t *testing.T) {
	runtime, pty, pub := newTestRuntime(t)
	cid := testCID(t)
	if _, err := runtime.AttachSnapshot(cid); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AcquireControl(cid); err != nil {
		t.Fatal(err)
	}
	if err := runtime.HandlePTYOutput([]byte("\x1b[6n")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return pty.String() == "\x1b[1;1R" })
	before := pty.String()
	if _, err := runtime.AttachSnapshot(cid); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := pty.String(); got != before {
		t.Fatalf("attach replayed a terminal response: before=%q after=%q", before, got)
	}
	// Query-only input changes no canonical cells and therefore emits no data
	// event. The only publication is the explicit control acquisition.
	eventually(t, func() bool { return len(pub.Messages()) >= 1 })
	messages := pub.Messages()
	if len(messages) != 1 || messages[0].Kind != protocol.KindControl {
		t.Fatalf("query leaked into publication stream: %+v", messages)
	}
}

func TestRuntimeCompactsToBoundedCheckpointAndTail(t *testing.T) {
	limits := protocol.StoreLimits{HistoryRows: 20, MaxTailEvents: 2, MaxTailBytes: 1 << 20}
	runtime, _, _ := newTestRuntimeWith(t, limits, time.Second)
	cid := testCID(t)
	if _, err := runtime.AttachSnapshot(cid); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AcquireControl(cid); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if err := runtime.HandlePTYOutput([]byte{byte('a' + i)}); err != nil {
			t.Fatal(err)
		}
	}
	status := runtime.Status()
	if status.Store.Compactions == 0 || status.Store.TailEvents > limits.MaxTailEvents {
		t.Fatalf("store did not remain bounded: %+v", status.Store)
	}
	snapshot, err := runtime.AttachSnapshot(cid)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := protocol.DecodeArchive(snapshot.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.Tail) > limits.MaxTailEvents || archive.Checkpoint.Seq == 0 {
		t.Fatalf("unexpected bounded archive: checkpoint=%d tail=%d", archive.Checkpoint.Seq, len(archive.Tail))
	}
	current, err := archive.Current()
	if err != nil {
		t.Fatal(err)
	}
	if current.Seq != status.Seq {
		t.Fatalf("archive seq=%d runtime seq=%d", current.Seq, status.Seq)
	}
}

type blockingPTY struct {
	started chan struct{}
	unblock chan struct{}
	dim     shenguard.Dimensions
	once    sync.Once
}

func (*blockingPTY) Read([]byte) (int, error) { return 0, nil }
func (p *blockingPTY) Write(b []byte) (int, error) {
	p.once.Do(func() { close(p.started) })
	<-p.unblock
	return len(b), nil
}
func (p *blockingPTY) SetSize(dim shenguard.Dimensions) error { p.dim = dim; return nil }

func TestBlockedPTYWriterDoesNotHoldRuntimeStateLock(t *testing.T) {
	dim, _ := shenguard.NewDimensions(20, 4)
	pty := &blockingPTY{started: make(chan struct{}), unblock: make(chan struct{}), dim: dim}
	pub := &recordPublisher{}
	runtime, err := NewRuntime(RuntimeConfig{
		Session: "test", Dimensions: dim, PTY: pty, Terminal: term.NewBasic(dim),
		Publisher: pub, StoreLimits: protocol.DefaultStoreLimits(), ControlLease: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(pty.unblock); _ = runtime.Close() }()
	cid := testCID(t)
	if _, err := runtime.AttachSnapshot(cid); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AcquireControl(cid); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Input(cid, []byte("blocked")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pty.started:
	case <-time.After(time.Second):
		t.Fatal("PTY writer did not start")
	}
	done := make(chan error, 1)
	go func() { done <- runtime.HandlePTYOutput([]byte("screen")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("blocked PTY write stalled terminal state processing")
	}
}

func frameText(state screen.State) string {
	var out strings.Builder
	for _, row := range state.Frame.Lines {
		for _, cell := range row {
			if cell.Width == 0 {
				continue
			}
			if cell.Text == "" {
				out.WriteByte(' ')
			} else {
				out.WriteString(cell.Text)
			}
		}
		out.WriteByte('\n')
	}
	return out.String()
}

func eventually(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestConcurrentRuntimeCommitsPublishInSequence(t *testing.T) {
	runtime, _, pub := newTestRuntime(t)
	cid := testCID(t)
	if _, err := runtime.AttachSnapshot(cid); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AcquireControl(cid); err != nil {
		t.Fatal(err)
	}

	const writes = 96
	start := make(chan struct{})
	errs := make(chan error, writes)
	var wg sync.WaitGroup
	for i := 0; i < writes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs <- runtime.HandlePTYOutput([]byte{byte('!' + i%80)})
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, func() bool { return len(pub.Messages()) == writes+1 })
	for i, msg := range pub.Messages() {
		if msg.Meta.Seq != uint64(i+1) {
			t.Fatalf("publication %d has seq %d", i, msg.Meta.Seq)
		}
	}
	select {
	case err := <-runtime.PublisherErrors():
		t.Fatalf("ordered actor failed: %v", err)
	default:
	}
}

type resizeFailTerminal struct {
	term.Terminal
	err error
}

func (t *resizeFailTerminal) Resize(shenguard.Dimensions) error { return t.err }

type trackingPTY struct {
	fakePTY
	setSizes []shenguard.Dimensions
	failAt   int
}

func (p *trackingPTY) SetSize(dim shenguard.Dimensions) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setSizes = append(p.setSizes, dim)
	if p.failAt > 0 && len(p.setSizes) == p.failAt {
		return errors.New("set-size failed")
	}
	p.dim = dim
	return nil
}

func TestResizeRollsBackPTYWhenAuthoritativeTerminalRejects(t *testing.T) {
	oldDim, _ := shenguard.NewDimensions(80, 24)
	newDim, _ := shenguard.NewDimensions(100, 40)
	pty := &trackingPTY{fakePTY: fakePTY{dim: oldDim}}
	pub := &recordPublisher{}
	runtime, err := NewRuntime(RuntimeConfig{
		Session: "test", Dimensions: oldDim, PTY: pty,
		Terminal:  &resizeFailTerminal{Terminal: term.NewBasic(oldDim), err: errors.New("terminal resize failed")},
		Publisher: pub, StoreLimits: protocol.DefaultStoreLimits(), ControlLease: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	cid := testCID(t)
	if _, err := runtime.AttachSnapshot(cid); err != nil {
		t.Fatal(err)
	}
	if err := runtime.AcquireControl(cid); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Resize(cid, newDim); err == nil {
		t.Fatal("resize unexpectedly succeeded")
	}
	if got := runtime.Status().Dim; got != oldDim {
		t.Fatalf("model dimension=%v want old=%v", got, oldDim)
	}
	pty.mu.Lock()
	gotDim := pty.dim
	setSizes := append([]shenguard.Dimensions(nil), pty.setSizes...)
	pty.mu.Unlock()
	if gotDim != oldDim || len(setSizes) != 2 || setSizes[0] != newDim || setSizes[1] != oldDim {
		t.Fatalf("PTY rollback dimension=%v calls=%v", gotDim, setSizes)
	}
	// Only control acquisition is sequenced; the rejected resize never commits.
	eventually(t, func() bool { return len(pub.Messages()) == 1 })
}
