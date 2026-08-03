package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEnsurePrivateIPCDirectory(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	if err := ensurePrivateIPCDirectory(private); err != nil {
		t.Fatalf("private directory rejected: %v", err)
	}
	info, err := os.Stat(private)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("mode = %04o, want 0700", got)
	}
}

func TestEnsurePrivateIPCDirectoryRejectsPermissiveParent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateIPCDirectory(path); err == nil {
		t.Fatal("permissive directory was accepted")
	}
}

func TestEnsurePrivateIPCDirectoryRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics require extra privileges on Windows")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateIPCDirectory(link); err == nil {
		t.Fatal("symlink directory was accepted")
	}
}

func TestAcquireIPCLockRejectsSecondOwner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	endpoint := "ipc://" + filepath.Join(dir, "session.sock")
	release, err := acquireIPCLock(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if secondRelease, err := acquireIPCLock(endpoint); err == nil {
		secondRelease()
		t.Fatal("second lock owner was accepted")
	}
}

func TestAcquireIPCLockCanBeReacquiredAfterRelease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	endpoint := "ipc://" + filepath.Join(dir, "session.sock")
	release, err := acquireIPCLock(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	release()
	secondRelease, err := acquireIPCLock(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	secondRelease()
}
