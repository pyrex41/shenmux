package term

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pyrex41/shenmux/internal/screen"
	"github.com/pyrex41/shenmux/internal/shenguard"
)

func testDim(t *testing.T, cols, rows int) shenguard.Dimensions {
	t.Helper()
	dim, err := shenguard.NewDimensions(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	return dim
}

func TestBasicInterpretsScreenAndAltState(t *testing.T) {
	terminal := NewBasic(testDim(t, 20, 10))
	if _, err := terminal.Feed([]byte("abc\r\n\x1b[3;5H\x1b[31mZ\x1b[0m")); err != nil {
		t.Fatal(err)
	}
	frame, err := terminal.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Cursor.X != 5 || frame.Cursor.Y != 2 || frame.AltScreen {
		t.Fatalf("unexpected cursor/state: %+v", frame.Cursor)
	}
	cell := frame.Lines[2][4]
	if cell.Text != "Z" || !cell.Style.FG.Valid {
		t.Fatalf("unexpected cell: %+v", cell)
	}
	if _, err := terminal.Feed([]byte("\x1b[?1049hALT")); err != nil {
		t.Fatal(err)
	}
	frame, _ = terminal.Snapshot()
	if !frame.AltScreen || frame.Lines[0][0].Text != "A" {
		t.Fatalf("alternate screen not active: %+v", frame)
	}
}

func TestBasicGeneratesPTYResponsesExactlyAsEffects(t *testing.T) {
	terminal := NewBasic(testDim(t, 80, 24))
	effects, err := terminal.Feed([]byte("\x1b[10;20H\x1b[6n\x1b[c\x1b[?7$p"))
	if err != nil {
		t.Fatal(err)
	}
	joined := bytes.Join(effects.PTYWrites, nil)
	for _, want := range []string{"\x1b[10;20R", "\x1b[?1;2c", "\x1b[?7;1$y"} {
		if !bytes.Contains(joined, []byte(want)) {
			t.Fatalf("response %q missing from %q", want, joined)
		}
	}
	frame, _ := terminal.Snapshot()
	if frame.Lines[0][0].Text != "" {
		t.Fatal("query bytes leaked into screen")
	}
}

func TestBasicSuppressesUntrustedSideEffectsFromSnapshot(t *testing.T) {
	terminal := NewBasic(testDim(t, 20, 4))
	payload := []byte("safe\x07\x1b]2;title\x07\x1b]52;c;SGVsbG8=\x07\x1bPmalicious\x1b\\done")
	effects, err := terminal.Feed(payload)
	if err != nil {
		t.Fatal(err)
	}
	if effects.Bells != 1 || effects.ClipboardWrites != 1 || !effects.TitleChanged {
		t.Fatalf("unexpected effects: %+v", effects)
	}
	frame, err := terminal.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, cell := range frame.Lines[0] {
		text.WriteString(cell.Text)
	}
	if !strings.Contains(text.String(), "safedone") || strings.Contains(text.String(), "malicious") {
		t.Fatalf("unexpected screen text %q", text.String())
	}
	state := screen.State{Frame: frame}
	var rendered bytes.Buffer
	var renderer screen.Renderer
	if err := renderer.RenderFull(&rendered, state); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rendered.Bytes(), []byte("\x1b]52")) || bytes.Contains(rendered.Bytes(), []byte("malicious")) {
		t.Fatalf("renderer leaked side effect: %q", rendered.Bytes())
	}
}

func TestBasicUTF8AcrossFeedBoundaries(t *testing.T) {
	terminal := NewBasic(testDim(t, 10, 2))
	bytesRune := []byte("界")
	if _, err := terminal.Feed(bytesRune[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Feed(bytesRune[1:]); err != nil {
		t.Fatal(err)
	}
	frame, _ := terminal.Snapshot()
	if frame.Lines[0][0].Text != "界" || frame.Lines[0][0].Width != 2 || frame.Lines[0][1].Width != 0 {
		t.Fatalf("wide rune not represented correctly: %+v %+v", frame.Lines[0][0], frame.Lines[0][1])
	}
}
