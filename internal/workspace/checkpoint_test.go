package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

func TestCaptureAndSave(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-qm", "initial")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cp, err := Capture(context.Background(), dir, "sess-1", "ws-1", "", "before retry")
	if err != nil {
		t.Fatal(err)
	}
	if cp.CheckpointID == "" || cp.GitCommit == "" || cp.WorkingTreeHash == "" || cp.UntrackedHash == "" || cp.ChangedFiles != 2 {
		t.Fatalf("unexpected checkpoint: %+v", cp)
	}
	store := t.TempDir()
	if err := Save(store, cp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store, "ws-1", cp.CheckpointID+".json")); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureOutsideGit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cp, err := Capture(context.Background(), dir, "", "ws", "", "start")
	if err != nil {
		t.Fatal(err)
	}
	if cp.Root != dir || cp.GitCommit != "" {
		t.Fatalf("unexpected non-git checkpoint: %+v", cp)
	}
}
