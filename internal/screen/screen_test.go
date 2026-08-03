package screen

import (
	"bytes"
	"strings"
	"testing"
)

func frameWithLines(t *testing.T, lines ...string) Frame {
	t.Helper()
	if len(lines) == 0 {
		t.Fatal("need lines")
	}
	cols := len([]rune(lines[0]))
	frame, err := BlankFrame(uint16(cols), uint16(len(lines)))
	if err != nil {
		t.Fatal(err)
	}
	for y, text := range lines {
		if len([]rune(text)) != cols {
			t.Fatalf("line %d width mismatch", y)
		}
		for x, r := range []rune(text) {
			frame.Lines[y][x] = Cell{Text: string(r), Width: 1}
		}
	}
	return frame
}

func TestAdvanceAndApplyExactScroll(t *testing.T) {
	previous := State{Frame: frameWithLines(t, "aaaa", "bbbb", "cccc"), History: []Row{frameWithLines(t, "hhhh").Lines[0]}}
	next := frameWithLines(t, "bbbb", "cccc", "dddd")
	state, delta, err := Advance(previous, next, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.HistoryAppend) != 1 {
		t.Fatalf("history append = %#v", delta.HistoryAppend)
	}
	if got := delta.HistoryAppend[0][0].Text; got != "a" {
		t.Fatalf("history append = %#v", delta.HistoryAppend)
	}
	applied, err := Apply(previous, delta, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !EqualState(state, applied) {
		t.Fatalf("applied state differs\nwant=%#v\ngot=%#v", state, applied)
	}
}

func TestAdvanceBoundsHistory(t *testing.T) {
	previous := State{Frame: frameWithLines(t, "aa", "bb"), History: []Row{frameWithLines(t, "00").Lines[0], frameWithLines(t, "11").Lines[0]}}
	next := frameWithLines(t, "bb", "cc")
	state, delta, err := Advance(previous, next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.History) != 2 || delta.HistoryDrop != 1 {
		t.Fatalf("state history=%d drop=%d", len(state.History), delta.HistoryDrop)
	}
}

func TestRendererNeverForwardsTerminalSideEffects(t *testing.T) {
	frame := frameWithLines(t, "safe")
	frame.Title = "ignored title"
	state := State{Frame: frame}
	var out bytes.Buffer
	var renderer Renderer
	if err := renderer.RenderFull(&out, state); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "\x1b]") || strings.Contains(got, "\x1bP") || strings.Contains(got, "\x1b_") {
		t.Fatalf("renderer emitted side-effect control string: %q", got)
	}
	if !strings.Contains(got, "safe") {
		t.Fatalf("renderer output missing content: %q", got)
	}
}

func TestResizeDropsWidthIncompatibleHistory(t *testing.T) {
	previous := State{Frame: frameWithLines(t, "aa"), History: []Row{frameWithLines(t, "zz").Lines[0]}}
	next := frameWithLines(t, "bbb")
	state, delta, err := Advance(previous, next, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.History) != 0 || !delta.HistoryReset {
		t.Fatalf("history not reset: state=%d delta=%+v", len(state.History), delta)
	}
}

func TestAdvanceDoesNotInventHistoryForUnchangedRepeatedRows(t *testing.T) {
	state, err := BlankState(8, 4)
	if err != nil {
		t.Fatal(err)
	}
	next, delta, err := Advance(state, state.Frame, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.History) != 0 || len(delta.HistoryAppend) != 0 {
		t.Fatalf("unchanged frame invented history: state=%d delta=%d", len(next.History), len(delta.HistoryAppend))
	}
}
