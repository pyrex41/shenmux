//go:build cgo && (linux || darwin)

package ptyx

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/pyrex41/shenmux/internal/shenguard"
)

func TestPTYRunsCommandAndCapturesOutput(t *testing.T) {
	dim, err := shenguard.NewDimensions(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	pty, err := Start([]string{"/bin/sh", "-c", "printf 'hello\\n'; exit 7"}, os.Environ(), dim)
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Close()
	output, err := io.ReadAll(pty)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output, []byte("hello")) {
		t.Fatalf("unexpected PTY output %q", output)
	}
	code, err := pty.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 {
		t.Fatalf("exit code %d, want 7", code)
	}
}

func TestPTYInputAndResize(t *testing.T) {
	dim, _ := shenguard.NewDimensions(80, 24)
	pty, err := Start([]string{"/bin/sh", "-c", "IFS= read -r line; printf 'got:%s\\n' \"$line\""}, os.Environ(), dim)
	if err != nil {
		t.Fatal(err)
	}
	defer pty.Close()
	newDim, _ := shenguard.NewDimensions(100, 40)
	if err := pty.SetSize(newDim); err != nil {
		t.Fatal(err)
	}
	if _, err := pty.Write([]byte("world\n")); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(pty)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output, []byte("got:world")) {
		t.Fatalf("unexpected PTY output %q", output)
	}
	if _, err := pty.Wait(); err != nil {
		t.Fatal(err)
	}
}
