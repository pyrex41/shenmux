package term

import (
	"github.com/pyrex41/shenmux/internal/screen"
	"github.com/pyrex41/shenmux/internal/shenguard"
)

// Effects are terminal-side effects interpreted exactly once by the
// authoritative server terminal. PTYWrites are protocol responses destined for
// the child PTY, never for attached host terminals.
type Effects struct {
	PTYWrites       [][]byte
	Bells           uint32
	TitleChanged    bool
	PWDChanged      bool
	ClipboardWrites uint32
	Notifications   uint32
	ProgressReports uint32
}

func (e Effects) Clone() Effects {
	out := e
	out.PTYWrites = make([][]byte, len(e.PTYWrites))
	for i, payload := range e.PTYWrites {
		out.PTYWrites[i] = append([]byte(nil), payload...)
	}
	return out
}

// Terminal is the single authoritative VT interpretation boundary. Snapshot
// returns already-interpreted cells and metadata; consumers never replay PTY
// bytes to reconstruct state.
type Terminal interface {
	Feed([]byte) (Effects, error)
	Resize(shenguard.Dimensions) error
	Snapshot() (screen.Frame, error)
	Close() error
}
