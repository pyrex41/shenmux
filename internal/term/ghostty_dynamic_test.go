//go:build libghostty && cgo && linux

package term

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyrex41/shenmux/internal/screen"
)

func buildFakeGhostty(t *testing.T) string {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler unavailable")
	}
	library := filepath.Join(t.TempDir(), "libghostty-vt.so")
	cmd := exec.Command(cc, "-shared", "-fPIC", "-std=c11", "-O2", "-o", library, filepath.Join("testdata", "fake_ghostty.c"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake libghostty-vt: %v\n%s", err, output)
	}
	return library
}

func TestGhosttyDynamicBridgeEndToEnd(t *testing.T) {
	terminal, err := newGhosttyWithLibrary(testDim(t, 8, 3), buildFakeGhostty(t))
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()

	payload := []byte("Hi\a\x1b[6n\x1b]2;ghost-title\x1b\\\x1b]7;file:///tmp/demo\x1b\\\x1b]52;c;SGVsbG8=\x1b\\\x1b]9;hello\x1b\\\x1b]9;4;1;50\x1b\\\x1b[?1049h")
	effects, err := terminal.Feed(payload)
	if err != nil {
		t.Fatal(err)
	}
	if effects.Bells != 1 || !effects.TitleChanged || !effects.PWDChanged || effects.ClipboardWrites != 1 || effects.Notifications != 1 || effects.ProgressReports != 1 {
		t.Fatalf("unexpected Ghostty effects: %+v", effects)
	}
	if len(effects.PTYWrites) != 1 || !bytes.Equal(effects.PTYWrites[0], []byte("\x1b[1;3R")) {
		t.Fatalf("unexpected Ghostty PTY response: %q", effects.PTYWrites)
	}

	frame, err := terminal.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Cols != 8 || frame.Rows != 3 || frame.Cursor.X != 2 || frame.Cursor.Y != 0 || !frame.Cursor.Visible || !frame.Cursor.Blink || frame.Cursor.Style != screen.CursorBlock || !frame.AltScreen {
		t.Fatalf("unexpected Ghostty frame metadata: %+v", frame)
	}
	if frame.Title != "ghost-title" || frame.WorkingDirectory != "file:///tmp/demo" {
		t.Fatalf("unexpected Ghostty metadata: title=%q pwd=%q", frame.Title, frame.WorkingDirectory)
	}
	first := frame.Lines[0][0]
	if first.Text != "H" || first.Width != 1 || !first.Style.Bold || !first.Style.Italic || !first.Style.Underline || !first.Style.DoubleUnderline {
		t.Fatalf("unexpected first cell: %+v", first)
	}
	if first.Style.FG != (screen.Color{Valid: true, R: 1, G: 2, B: 3}) || first.Style.BG != (screen.Color{Valid: true, R: 4, G: 5, B: 6}) {
		t.Fatalf("unexpected first-cell colors: %+v", first.Style)
	}
	if frame.Lines[0][1].Text != "i" || frame.DefaultFG != (screen.Color{Valid: true, R: 230, G: 231, B: 232}) || frame.DefaultBG != (screen.Color{Valid: true, R: 10, G: 11, B: 12}) {
		t.Fatalf("unexpected remaining frame state: %+v", frame)
	}

	var rendered bytes.Buffer
	var renderer screen.Renderer
	if err := renderer.RenderFull(&rendered, screen.State{Frame: frame}); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"\x1b]52", "\x1b]9;", "SGVsbG8=", "ghost-title"} {
		if strings.Contains(rendered.String(), forbidden) {
			t.Fatalf("safe renderer leaked terminal side effect %q in %q", forbidden, rendered.String())
		}
	}

	if err := terminal.Resize(testDim(t, 10, 4)); err != nil {
		t.Fatal(err)
	}
	resized, err := terminal.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if resized.Cols != 10 || resized.Rows != 4 || resized.Lines[0][0].Text != "H" {
		t.Fatalf("unexpected resized frame: %+v", resized)
	}

	if err := terminal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Feed([]byte("x")); err == nil {
		t.Fatal("Feed succeeded after Close")
	}
}
