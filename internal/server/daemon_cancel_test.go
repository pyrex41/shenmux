//go:build cgo && (linux || darwin)

package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyrex41/shenmux/internal/shenguard"
)

func TestServeCancellationStopsAndReapsPTY(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "smx-server-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	control := "ipc://" + filepath.Join(dir, "control.sock")
	data := "ipc://" + filepath.Join(dir, "data.sock")
	dim, _ := shenguard.NewDimensions(80, 24)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Config{
			Session: "cancel", ControlEndpoint: control, DataEndpoint: data,
			Dimensions: dim, Env: os.Environ(), ExitGrace: 10 * time.Millisecond,
			Command: []string{"/bin/sh", "-c", "trap '' HUP TERM; while :; do sleep 1; done"},
		})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, controlErr := os.Stat(strings.TrimPrefix(control, "ipc://"))
		_, dataErr := os.Stat(strings.TrimPrefix(data, "ipc://"))
		if controlErr == nil && dataErr == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("server exited before cancellation: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not bind IPC endpoints")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop and reap PTY after cancellation")
	}
}
