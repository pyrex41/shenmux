package server

import (
	"sync"
	"testing"
	"time"

	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/shenguard"
	"github.com/pyrex41/shenmux/internal/term"
)

// echoPTY is a fakePTY that can also report the line discipline's ECHO bit,
// which is what a real PTY does through termios.
type echoPTY struct {
	fakePTY
	mu      sync.Mutex
	enabled bool
	err     error
}

func (p *echoPTY) EchoEnabled() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enabled, p.err
}

func (p *echoPTY) set(enabled bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.enabled, p.err = enabled, err
}

func newEchoRuntime(t *testing.T) (*Runtime, *echoPTY, *recordPublisher) {
	t.Helper()
	dim, _ := shenguard.NewDimensions(80, 24)
	pty := &echoPTY{}
	pty.fakePTY.dim = dim
	pub := &recordPublisher{}
	runtime, err := NewRuntime(RuntimeConfig{
		Session: "test", Dimensions: dim, PTY: pty, Terminal: term.NewBasic(dim),
		Publisher: pub, StoreLimits: protocol.DefaultStoreLimits(),
		ControlLease: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime, pty, pub
}

// echoOfPublishedFrame reads the echo bit out of the most recent delta a client
// would have received -- the same path the browser learns about it through.
// Reports false when nothing has been published, which is the pre-report state.
func echoOfPublishedFrame(t *testing.T, pub *recordPublisher) bool {
	t.Helper()
	msg, ok := pub.lastDelta()
	if !ok {
		return false
	}
	delta, err := protocol.DecodeDelta(msg.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return delta.Modes.Echo
}

// A client may only predict local echo while the PTY is echoing, so the bit the
// daemon reads from termios has to reach the published frame.
func TestEchoBitReachesPublishedFrames(t *testing.T) {
	runtime, pty, pub := newEchoRuntime(t)

	if echoOfPublishedFrame(t, pub) {
		t.Fatal("echo must start false: nothing has reported it yet")
	}

	pty.set(true, nil)
	if err := runtime.RefreshEcho(); err != nil {
		t.Fatal(err)
	}
	if !waitForCount(pub, 1) || !echoOfPublishedFrame(t, pub) {
		t.Fatal("echo on the PTY must reach the published frame")
	}

	// A password prompt turning echo off must be visible to clients, which is
	// the whole point: predicting past this would paint the password.
	pty.set(false, nil)
	before := pub.count()
	if err := runtime.RefreshEcho(); err != nil {
		t.Fatal(err)
	}
	if !waitForCount(pub, before+1) {
		t.Fatal("echo going false must publish a delta")
	}
	if echoOfPublishedFrame(t, pub) {
		t.Fatal("echo going false must reach the published frame")
	}
}

// The published frame is how clients learn about the change, so a flip has to
// produce a delta even though no PTY output accompanied it.
func TestEchoChangePublishesADelta(t *testing.T) {
	runtime, pty, pub := newEchoRuntime(t)
	before := pub.count()

	pty.set(true, nil)
	if err := runtime.RefreshEcho(); err != nil {
		t.Fatal(err)
	}
	if !waitForCount(pub, before+1) {
		t.Fatal("an echo change published nothing; clients would never learn about it")
	}

	// Polling is on a ticker, so the steady state must be silent.
	steady := pub.count()
	for i := 0; i < 5; i++ {
		if err := runtime.RefreshEcho(); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(20 * time.Millisecond)
	if got := pub.count(); got != steady {
		t.Fatalf("unchanged echo published %d extra messages; the poll must be silent when nothing moved", got-steady)
	}
}

// Failing closed matters more than being right: a PTY that cannot report echo,
// or errors while reporting it, must leave prediction switched off.
func TestEchoFailsClosed(t *testing.T) {
	t.Run("pty cannot report echo", func(t *testing.T) {
		runtime, _, pub := newTestRuntime(t) // fakePTY has no EchoEnabled
		before := pub.count()
		if err := runtime.RefreshEcho(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		if pub.count() != before {
			t.Fatal("a PTY that cannot report echo must not publish anything")
		}

		// Publish through the ordinary output path so there is a real frame to
		// inspect. Reading the echo bit with nothing published would only be
		// reading the helper's zero value, which any implementation passes.
		if err := runtime.HandlePTYOutput([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		if !waitForCount(pub, before+1) {
			t.Fatal("PTY output should have published a delta")
		}
		if echoOfPublishedFrame(t, pub) {
			t.Fatal("echo must stay false in a published frame when the PTY cannot report it")
		}
	})

	t.Run("reporting echo fails", func(t *testing.T) {
		runtime, pty, pub := newEchoRuntime(t)
		pty.set(true, nil)
		if err := runtime.RefreshEcho(); err != nil {
			t.Fatal(err)
		}
		if !waitForCount(pub, 1) || !echoOfPublishedFrame(t, pub) {
			t.Fatal("precondition: echo should be true")
		}
		pty.set(true, errRefused)
		before := pub.count()
		if err := runtime.RefreshEcho(); err != nil {
			t.Fatalf("an unreadable termios must not be fatal: %v", err)
		}
		if !waitForCount(pub, before+1) {
			t.Fatal("echo going unreadable should publish the fallback")
		}
		if echoOfPublishedFrame(t, pub) {
			t.Fatal("an unreadable termios must fall back to not predicting")
		}
	})
}

var errRefused = &refusedError{}

type refusedError struct{}

func (*refusedError) Error() string { return "termios unavailable" }

func (p *recordPublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.messages)
}

func (p *recordPublisher) lastDelta() (protocol.Message, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.messages) - 1; i >= 0; i-- {
		if p.messages[i].Kind == protocol.KindDelta {
			return p.messages[i], true
		}
	}
	return protocol.Message{}, false
}

// waitForCount waits for the queued publisher to catch up. Publication is
// asynchronous, so sampling the count immediately races it.
func waitForCount(pub *recordPublisher, want int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pub.count() >= want {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}
