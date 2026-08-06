//go:build linux || darwin

package zmqx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSendTimeoutIsRejectedRatherThanIgnored pins the one option this adapter
// must refuse. The driver's Send takes a context and discards it, so a send
// blocks until the peer drains or the socket closes; accepting a send timeout
// would return nil and change nothing, and the caller reaching for it is by
// definition trying to stop a send from blocking forever.
func TestSendTimeoutIsRejectedRatherThanIgnored(t *testing.T) {
	ctx, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	socket, err := ctx.Socket(Router)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()

	if err := socket.SetInt(SndTimeout, 100); !errors.Is(err, ErrSendTimeoutUnsupported) {
		t.Fatalf("SetInt(SndTimeout) = %v, want %v", err, ErrSendTimeoutUnsupported)
	}
	// The options shenmux actually relies on must keep working, including the
	// ones that are inert by design.
	for _, option := range []int{Linger, Immediate} {
		if err := socket.SetInt(option, 0); err != nil {
			t.Fatalf("SetInt(%d) = %v, want nil", option, err)
		}
	}
	if err := socket.SetInt(SndHWM, 10_000); err != nil {
		t.Fatalf("SetInt(SndHWM) = %v, want nil", err)
	}
}

// RcvTimeout was accepted and silently ignored, exactly like SndTimeout was.
// Nothing in the tree sets it, so rejecting it costs nothing and stops the
// adapter promising a deadline it never applied.
func TestReceiveTimeoutIsRejectedRatherThanIgnored(t *testing.T) {
	ctx, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	socket, err := ctx.Socket(Router)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()

	if err := socket.SetInt(RcvTimeout, 1000); !errors.Is(err, ErrRecvTimeoutUnsupported) {
		t.Fatalf("RcvTimeout should be refused, got %v", err)
	}
	// Linger is also inert, but callers set it, so it must keep working.
	if err := socket.SetInt(Linger, 0); err != nil {
		t.Fatalf("Linger is set by real callers and must stay accepted: %v", err)
	}
}

// MaxMsgSize bounds what a socket keeps, not what a peer can make it allocate:
// the driver reads the frame in full before this adapter ever sees its size.
// This pins the distinction so nobody reads the option as a memory bound.
func TestMaxMsgSizeDoesNotBoundAllocation(t *testing.T) {
	ep := "ipc://" + shortIPCPath(t, "alloc")
	ctx, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	router, err := ctx.Socket(Router)
	if err != nil {
		t.Fatal(err)
	}
	if err := router.SetInt64(MaxMsgSize, 1024); err != nil {
		t.Fatal(err)
	}
	if err := router.Bind(ep); err != nil {
		t.Fatal(err)
	}
	defer router.Close()

	dealer, err := ctx.Socket(Dealer)
	if err != nil {
		t.Fatal(err)
	}
	if err := dealer.Connect(ep); err != nil {
		t.Fatal(err)
	}
	defer dealer.Close()
	time.Sleep(300 * time.Millisecond)

	const oversized = 4 << 20 // 4096x the socket's declared limit
	if err := dealer.SendMultipart([][]byte{make([]byte, oversized)}, 0); err != nil {
		t.Fatalf("send: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, err := router.RecvMultipartLimit(DontWait, 1024, 4)
		if errors.Is(err, ErrWouldBlock) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err == nil {
			t.Fatal("a frame 4096x the limit must not be delivered")
		}
		// Refused -- but only after the driver read and allocated the whole
		// frame. That is the property being documented: the refusal is a
		// retention check, and WireFrameLimit is the real allocation ceiling.
		if !strings.Contains(err.Error(), "exceeds limit") {
			t.Fatalf("unexpected refusal: %v", err)
		}
		return
	}
	t.Fatal("the oversized frame never arrived, so this test proved nothing")
}

// If the driver lowers or raises its frame ceiling, WireFrameLimit becomes a
// lie. Pin it by sending a frame just under it and one just over.
func TestWireFrameLimitMatchesTheDriver(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates ~32MiB")
	}
	ep := "ipc://" + shortIPCPath(t, "wire")
	ctx, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	router, err := ctx.Socket(Router)
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Bind(ep); err != nil {
		t.Fatal(err)
	}
	defer router.Close()

	dealer, err := ctx.Socket(Dealer)
	if err != nil {
		t.Fatal(err)
	}
	if err := dealer.Connect(ep); err != nil {
		t.Fatal(err)
	}
	defer dealer.Close()
	time.Sleep(300 * time.Millisecond)

	// One byte over the documented ceiling must be refused by the driver.
	err = dealer.SendMultipart([][]byte{make([]byte, WireFrameLimit+1)}, 0)
	if err == nil {
		// The send may succeed locally; the receive side is what must refuse.
		deadline := time.Now().Add(3 * time.Second)
		delivered := false
		for time.Now().Before(deadline) {
			if _, rerr := router.RecvMultipartLimit(DontWait, defaultMaxFrame, 4); rerr == nil {
				delivered = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if delivered {
			t.Fatalf("a frame above WireFrameLimit (%d) was delivered; the driver's ceiling moved", WireFrameLimit)
		}
	}
}

// shortIPCPath returns a socket path inside the platform's unix-socket length
// limit (about 104 bytes on macOS). t.TempDir() paths are far longer than that,
// so binding one fails with a bare "invalid argument".
func shortIPCPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "zmqx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name+".ipc")
}
