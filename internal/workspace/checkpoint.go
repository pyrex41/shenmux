// Package workspace provides durable, user-facing checkpoints for the files
// a shenmux session is operating on. Checkpoints are metadata plus content
// hashes; the live terminal history remains owned by internal/history.
package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Checkpoint is an immutable description of a workspace at a point in time.
// Git repositories store the committed state and a binary working-tree patch;
// untracked files are captured in a deterministic tar archive.
type Checkpoint struct {
	SessionID       string    `json:"session_id"`
	WorkspaceID     string    `json:"workspace_id"`
	CheckpointID    string    `json:"checkpoint_id"`
	Parent          string    `json:"parent,omitempty"`
	Root            string    `json:"root"`
	GitCommit       string    `json:"git_commit,omitempty"`
	WorkingTreeHash string    `json:"working_tree_patch,omitempty"`
	UntrackedHash   string    `json:"untracked_archive,omitempty"`
	ChangedFiles    int       `json:"changed_files"`
	CreatedAt       time.Time `json:"created_at"`
	Reason          string    `json:"reason"`
}

// Capture inspects root and returns a content-addressed checkpoint. It does
// not mutate the worktree or run hooks. The caller may persist it with Save.
func Capture(ctx context.Context, root, sessionID, workspaceID, parent, reason string) (Checkpoint, error) {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(workspaceID) == "" {
		return Checkpoint{}, errors.New("workspace root and workspace ID are required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Checkpoint{}, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return Checkpoint{}, fmt.Errorf("workspace root: %w", err)
	}
	gitRoot, err := git(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		gitRoot = abs
	} else {
		gitRoot = strings.TrimSpace(gitRoot)
	}
	commit, _ := git(ctx, gitRoot, "rev-parse", "HEAD")
	commit = strings.TrimSpace(commit)
	diff, _ := gitBytes(ctx, gitRoot, "diff", "--binary", "HEAD", "--")
	workingHash := hashBytes(diff)
	files, _ := git(ctx, gitRoot, "status", "--porcelain=v1", "--untracked-files=all")
	changed := countStatus(files)
	untracked, _ := untrackedArchive(ctx, gitRoot)
	untHash := hashBytes(untracked)
	cp := Checkpoint{SessionID: sessionID, WorkspaceID: workspaceID, Root: gitRoot, Parent: parent,
		GitCommit: commit, WorkingTreeHash: workingHash, UntrackedHash: untHash,
		ChangedFiles: changed, CreatedAt: time.Now().UTC(), Reason: reason}
	canonical, _ := json.Marshal(cp)
	cp.CheckpointID = hashBytes(canonical)
	return cp, nil
}

// Save writes a checkpoint manifest atomically below dir/workspaceID.
func Save(dir string, cp Checkpoint) error {
	if cp.CheckpointID == "" || cp.WorkspaceID == "" {
		return errors.New("checkpoint ID and workspace ID are required")
	}
	path := filepath.Join(dir, cp.WorkspaceID, cp.CheckpointID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".checkpoint-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(b)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func hashBytes(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }

func git(ctx context.Context, dir string, args ...string) (string, error) {
	b, err := gitBytes(ctx, dir, args...)
	return string(b), err
}
func gitBytes(ctx context.Context, dir string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = dir
	return c.Output()
}
func countStatus(s string) int {
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func untrackedArchive(ctx context.Context, dir string) ([]byte, error) {
	out, err := git(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	paths := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	sort.Strings(paths)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, rel := range paths {
		if rel == "" {
			continue
		}
		full := filepath.Join(dir, filepath.FromSlash(rel))
		f, e := os.Open(full)
		if e != nil {
			return nil, e
		}
		st, e := f.Stat()
		if e != nil {
			f.Close()
			return nil, e
		}
		h := &tar.Header{Name: rel, Mode: int64(st.Mode().Perm()), Size: st.Size(), ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}
		if e = tw.WriteHeader(h); e == nil {
			_, e = io.Copy(tw, f)
		}
		f.Close()
		if e != nil {
			return nil, e
		}
	}
	if err = tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
