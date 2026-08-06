//go:build linux || darwin

package zmqx

import (
	"errors"
	"testing"
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
	for _, option := range []int{Linger, RcvTimeout, Immediate} {
		if err := socket.SetInt(option, 0); err != nil {
			t.Fatalf("SetInt(%d) = %v, want nil", option, err)
		}
	}
	if err := socket.SetInt(SndHWM, 10_000); err != nil {
		t.Fatalf("SetInt(SndHWM) = %v, want nil", err)
	}
}
