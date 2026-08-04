//go:build cgo && (linux || darwin)

package client

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/screen"
	"github.com/pyrex41/shenmux/internal/server"
	"github.com/pyrex41/shenmux/internal/shenguard"
)

func TestEndToEndCanonicalStateAndExclusiveControlOverZMQ(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "smx-client-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	controlEndpoint := "ipc://" + filepath.Join(dir, "control.sock")
	dataEndpoint := "ipc://" + filepath.Join(dir, "data.sock")
	dim, _ := shenguard.NewDimensions(80, 24)
	serverCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Serve(serverCtx, server.Config{
			Session: "test", ControlEndpoint: controlEndpoint, DataEndpoint: dataEndpoint,
			Dimensions: dim, Env: os.Environ(), ExitGrace: 300 * time.Millisecond,
			Command:     []string{"/bin/sh", "-c", "IFS= read -r line; printf 'seen:%s\\n' \"$line\""},
			StoreLimits: protocol.StoreLimits{HistoryRows: 100, MaxTailEvents: 32, MaxTailBytes: 1 << 20},
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	waitForEndpoints(t, ctx, serverDone, controlEndpoint, dataEndpoint)

	one, err := New(ctx, Config{Session: "test", ClientID: fmt.Sprintf("client-one-%d", time.Now().UnixNano()), ControlEndpoint: controlEndpoint, DataEndpoint: dataEndpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, err := New(ctx, Config{Session: "test", ClientID: fmt.Sprintf("client-two-%d", time.Now().UnixNano()), ControlEndpoint: controlEndpoint, DataEndpoint: dataEndpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()

	snapshot, err := one.Attach(ctx)
	if err != nil {
		t.Fatal(err)
	}
	observerSnapshot, err := two.Attach(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Current.Seq != 0 || observerSnapshot.Current.Seq != 0 {
		t.Fatalf("unexpected initial snapshots: %d/%d", snapshot.Current.Seq, observerSnapshot.Current.Seq)
	}
	meta, err := one.AcquireControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.HasControl || meta.ControlOwner != one.ID() {
		t.Fatalf("unexpected ownership acknowledgement: %+v", meta)
	}
	if _, err := two.AcquireControl(ctx); err == nil {
		t.Fatal("observer acquired already-owned control")
	}
	if err := two.Input(ctx, []byte("bad\n")); err == nil {
		t.Fatal("observer input was accepted")
	}
	if err := one.Input(ctx, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}

	view := snapshot.View()
	for !view.Checkpoint.Exited {
		select {
		case msg, ok := <-one.Events():
			if !ok {
				t.Fatal("event stream closed before exit")
			}
			if msg.Meta.Seq <= view.Checkpoint.Seq {
				continue
			}
			if _, err := view.Apply(msg); err != nil {
				t.Fatal(err)
			}
		case err := <-one.Errors():
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if !strings.Contains(stateText(view.Checkpoint.Screen), "seen:hello") {
		t.Fatalf("screen %q does not contain command response", stateText(view.Checkpoint.Screen))
	}
	if view.Checkpoint.ControlOwner != one.ID() {
		t.Fatalf("unexpected final control owner %q", view.Checkpoint.ControlOwner)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRenderCannotEmitApplicationOSCOrDCS(t *testing.T) {
	state, err := screen.BlankState(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	state.Frame.Title = "\x1b]52;c;SGVsbG8=\x07"
	// Metadata validation rejects control characters before rendering.
	checkpoint := protocol.Checkpoint{Screen: state}
	archive := protocol.Archive{Format: 2, HistoryLimit: 0, Checkpoint: checkpoint}
	if _, err := protocol.EncodeArchive(archive); err == nil {
		t.Fatal("unsafe control metadata was accepted into a checkpoint")
	}
}

func waitForEndpoints(t *testing.T, ctx context.Context, serverDone <-chan error, endpoints ...string) {
	t.Helper()
	for {
		ready := true
		var lastErr error
		for _, endpoint := range endpoints {
			_, err := os.Stat(strings.TrimPrefix(endpoint, "ipc://"))
			if err != nil {
				ready = false
				lastErr = err
			}
		}
		if ready {
			return
		}
		select {
		case err := <-serverDone:
			t.Fatalf("server exited before binding endpoints: %v", err)
		case <-ctx.Done():
			t.Fatalf("server did not bind endpoints: %v (%v)", ctx.Err(), lastErr)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func stateText(state screen.State) string {
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
