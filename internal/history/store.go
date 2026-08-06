// Package history persists the latest authoritative session archive.
//
// The live protocol remains responsible for transport. This package gives a
// runtime a crash-safe durable checkpoint that can be inspected or served by a
// higher-level command center.
package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/pyrex41/shenmux/internal/protocol"
)

func validSession(session string) bool {
	return session != "" && filepath.Base(session) == session && session != "." && session != ".." && filepath.Ext(session) == ""
}

type Record struct {
	Session   string    `json:"session"`
	UpdatedAt time.Time `json:"updated_at"`
	Archive   []byte    `json:"archive"`
}

// Save atomically replaces the durable checkpoint for session. Archive is the
// canonical compressed protocol archive, so it can be passed directly to
// protocol.DecodeArchive by history readers.
func Save(root, session string, archive protocol.Archive) error {
	if root == "" || !validSession(session) {
		return fmt.Errorf("history root and session must not be empty")
	}
	payload, err := protocol.EncodeArchive(archive)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create history directory: %w", err)
	}
	record := Record{Session: session, UpdatedAt: time.Now().UTC(), Archive: payload}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode history record: %w", err)
	}
	tmp, err := os.CreateTemp(root, ".history-*")
	if err != nil {
		return fmt.Errorf("create history temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write history record: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync history record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(root, session+".json")); err != nil {
		return fmt.Errorf("install history record: %w", err)
	}
	return nil
}

func Load(root, session string) (Record, error) {
	if root == "" || !validSession(session) {
		return Record{}, fmt.Errorf("invalid history session %q", session)
	}
	data, err := os.ReadFile(filepath.Join(root, session+".json"))
	if err != nil {
		return Record{}, err
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, fmt.Errorf("decode history record: %w", err)
	}
	if record.Session != session {
		return Record{}, fmt.Errorf("history session mismatch: got %q, want %q", record.Session, session)
	}
	if _, err := protocol.DecodeArchive(record.Archive); err != nil {
		return Record{}, fmt.Errorf("validate history archive: %w", err)
	}
	return record, nil
}

// List returns the durable checkpoints present in root, ordered by session
// name. Invalid or unreadable records are reported rather than silently
// omitted so callers cannot mistake a partial history view for a complete one.
func List(root string) ([]Record, error) {
	if root == "" {
		return nil, fmt.Errorf("history root must not be empty")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var records []Record
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		session := entry.Name()[:len(entry.Name())-len(filepath.Ext(entry.Name()))]
		// A name that was never a valid session is not a record we wrote, so
		// skipping it does not hide any history. Only a well-named record that
		// fails to load is worth failing the whole listing for -- that one is
		// ours and is damaged. Without this split, an editor backup dropped in
		// the directory takes the entire history endpoint down.
		if !validSession(session) {
			continue
		}
		record, err := Load(root, session)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Session < records[j].Session })
	return records, nil
}
