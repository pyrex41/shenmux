package supervisor

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestStartRejectsInvalidSpecs(t *testing.T) {
	m := New()
	if _, err := m.Start(context.Background(), Spec{Name: "bad name", Binary: "x"}); err == nil {
		t.Fatal("expected invalid name")
	}
	if _, err := m.Start(context.Background(), Spec{Name: "ok"}); err == nil {
		t.Fatal("expected missing binary")
	}
}

func TestStartListAndStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture is unix-only")
	}
	d := t.TempDir()
	script := d + "/session"
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap 'exit 0' INT TERM\nwhile :; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	m := New()
	h, err := m.Start(context.Background(), Spec{Name: "demo", Binary: script})
	if err != nil {
		t.Fatal(err)
	}
	if h.PID <= 0 || len(m.List()) != 1 {
		t.Fatalf("unexpected session: %+v", m.List())
	}
	if _, err := m.Start(context.Background(), Spec{Name: "demo", Binary: script}); err == nil {
		t.Fatal("expected duplicate rejection")
	}
	if err := m.Stop("demo"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("session did not exit")
	}
	if len(m.List()) != 0 {
		t.Fatalf("session remained after exit: %+v", m.List())
	}
}
