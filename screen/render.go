package screen

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

const esc = "\x1b["

// Renderer emits only a small, generated ANSI rendering vocabulary. It never
// forwards OSC, DCS, APC, or application-provided CSI sequences.
type Renderer struct {
	initialized bool
}

func (r *Renderer) RenderFull(w io.Writer, state State) error {
	if w == nil {
		return fmt.Errorf("nil renderer writer")
	}
	if err := state.Validate(); err != nil {
		return err
	}
	var b strings.Builder
	b.Grow(int(state.Frame.Cols)*int(state.Frame.Rows) + 256)
	b.WriteString(esc + "?25l")
	b.WriteString(esc + "H")
	b.WriteString(esc + "2J")
	for y, row := range state.Frame.Lines {
		writePosition(&b, uint16(y), 0)
		writeRow(&b, row)
		b.WriteString(esc + "K")
	}
	writeCursor(&b, state.Frame.Cursor)
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	r.initialized = true
	return nil
}

func (r *Renderer) RenderDelta(w io.Writer, state State, delta Delta) error {
	if !r.initialized || delta.Full {
		return r.RenderFull(w, state)
	}
	if w == nil {
		return fmt.Errorf("nil renderer writer")
	}
	if err := state.Validate(); err != nil {
		return err
	}
	if err := delta.Validate(); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(esc + "?25l")
	for _, line := range delta.Lines {
		writePosition(&b, line.Y, 0)
		writeRow(&b, line.Cells)
		b.WriteString(esc + "K")
	}
	writeCursor(&b, state.Frame.Cursor)
	_, err := io.WriteString(w, b.String())
	return err
}

func writePosition(b *strings.Builder, y, x uint16) {
	b.WriteString(esc)
	b.WriteString(strconv.Itoa(int(y) + 1))
	b.WriteByte(';')
	b.WriteString(strconv.Itoa(int(x) + 1))
	b.WriteByte('H')
}

func writeCursor(b *strings.Builder, cursor Cursor) {
	writePosition(b, cursor.Y, cursor.X)
	switch cursor.Style {
	case CursorBar:
		if cursor.Blink {
			b.WriteString(esc + "5 q")
		} else {
			b.WriteString(esc + "6 q")
		}
	case CursorUnderline:
		if cursor.Blink {
			b.WriteString(esc + "3 q")
		} else {
			b.WriteString(esc + "4 q")
		}
	case CursorHollowBlock:
		b.WriteString(esc + "2 q")
	default:
		if cursor.Blink {
			b.WriteString(esc + "1 q")
		} else {
			b.WriteString(esc + "2 q")
		}
	}
	if cursor.Visible {
		b.WriteString(esc + "?25h")
	} else {
		b.WriteString(esc + "?25l")
	}
}

func writeRow(b *strings.Builder, row Row) {
	var current Style
	haveStyle := false
	for _, cell := range row {
		if cell.Width == 0 {
			continue
		}
		if !haveStyle || current != cell.Style {
			writeStyle(b, cell.Style)
			current = cell.Style
			haveStyle = true
		}
		if cell.Text == "" {
			b.WriteByte(' ')
		} else {
			b.WriteString(cell.Text)
		}
	}
	b.WriteString(esc + "0m")
}

func writeStyle(b *strings.Builder, style Style) {
	params := []string{"0"}
	if style.Bold {
		params = append(params, "1")
	}
	if style.Faint {
		params = append(params, "2")
	}
	if style.Italic {
		params = append(params, "3")
	}
	if style.DoubleUnderline {
		params = append(params, "21")
	} else if style.Underline {
		params = append(params, "4")
	}
	if style.Blink {
		params = append(params, "5")
	}
	if style.Inverse {
		params = append(params, "7")
	}
	if style.Invisible {
		params = append(params, "8")
	}
	if style.Strikethrough {
		params = append(params, "9")
	}
	if style.Overline {
		params = append(params, "53")
	}
	if style.FG.Valid {
		params = append(params, "38", "2", strconv.Itoa(int(style.FG.R)), strconv.Itoa(int(style.FG.G)), strconv.Itoa(int(style.FG.B)))
	}
	if style.BG.Valid {
		params = append(params, "48", "2", strconv.Itoa(int(style.BG.R)), strconv.Itoa(int(style.BG.G)), strconv.Itoa(int(style.BG.B)))
	}
	b.WriteString(esc)
	b.WriteString(strings.Join(params, ";"))
	b.WriteByte('m')
}
