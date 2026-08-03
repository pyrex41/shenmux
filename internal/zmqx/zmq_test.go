//go:build cgo && (linux || darwin)

package zmqx

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestMultipartRoundTrip(t *testing.T) {
	ctx, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()

	a, err := ctx.Socket(Pair)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := ctx.Socket(Pair)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, socket := range []*Socket{a, b} {
		if err := socket.SetInt(Linger, 0); err != nil {
			t.Fatal(err)
		}
	}
	endpoint := fmt.Sprintf("inproc://shenmux-test-%d", time.Now().UnixNano())
	if err := a.Bind(endpoint); err != nil {
		t.Fatal(err)
	}
	if err := b.Connect(endpoint); err != nil {
		t.Fatal(err)
	}
	want := [][]byte{[]byte("kind"), {}, {0, 1, 0xff, 0}}
	if err := b.SendMultipart(want, 0); err != nil {
		t.Fatal(err)
	}
	got, err := a.RecvMultipart(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("frame %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestNonblockingReceive(t *testing.T) {
	ctx, err := NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.Close()
	socket, err := ctx.Socket(Pair)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if _, err := socket.RecvMultipart(DontWait); !errors.Is(err, ErrWouldBlock) {
		t.Fatalf("got %v, want ErrWouldBlock", err)
	}
}
