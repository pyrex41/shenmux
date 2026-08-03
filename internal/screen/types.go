package screen

import (
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxCells bounds allocations at every wire and terminal-engine boundary.
	MaxCells       = 1 << 20
	MaxCellBytes   = 64
	MaxTitleBytes  = 4096
	MaxHistoryRows = 10_000
)

type Color struct {
	Valid bool
	R     uint8
	G     uint8
	B     uint8
}

type Style struct {
	FG              Color
	BG              Color
	Bold            bool
	Faint           bool
	Italic          bool
	Underline       bool
	DoubleUnderline bool
	Blink           bool
	Inverse         bool
	Invisible       bool
	Strikethrough   bool
	Overline        bool
}

type Cell struct {
	// Text is one printable grapheme cluster. Empty text denotes a blank cell.
	Text string
	// Width is 0 for the trailing half of a wide cell, 1 for a normal cell,
	// and 2 for the leading half of a wide cell.
	Width uint8
	Style Style
}

type Row []Cell

type CursorStyle uint8

const (
	CursorBlock CursorStyle = iota
	CursorBar
	CursorUnderline
	CursorHollowBlock
)

type Cursor struct {
	X       uint16
	Y       uint16
	Visible bool
	Blink   bool
	Style   CursorStyle
}

// Frame is the complete authoritative visible terminal state. It contains no
// untrusted escape stream: cells and metadata are already interpreted.
type Frame struct {
	Cols             uint16
	Rows             uint16
	Lines            []Row
	Cursor           Cursor
	AltScreen        bool
	Title            string
	WorkingDirectory string
	DefaultFG        Color
	DefaultBG        Color
}

// State adds bounded client-visible history to the active frame. History is
// canonical row data, not raw terminal bytes, so restoring it cannot replay
// bells, clipboard requests, device queries, or other side effects.
type State struct {
	Frame   Frame
	History []Row
}

type LineDelta struct {
	Y     uint16
	Cells Row
}

// Delta is an interpreted, side-effect-free state transition. Metadata is
// carried on every delta so clients can recover from partial renderer state.
type Delta struct {
	Cols             uint16
	Rows             uint16
	Full             bool
	Lines            []LineDelta
	Cursor           Cursor
	AltScreen        bool
	Title            string
	WorkingDirectory string
	DefaultFG        Color
	DefaultBG        Color
	HistoryReset     bool
	HistoryDrop      uint32
	HistoryAppend    []Row
}

func BlankFrame(cols, rows uint16) (Frame, error) {
	if err := validateDimensions(cols, rows); err != nil {
		return Frame{}, err
	}
	lines := make([]Row, int(rows))
	for y := range lines {
		lines[y] = blankRow(cols)
	}
	return Frame{
		Cols: cols, Rows: rows, Lines: lines,
		Cursor: Cursor{Visible: true, Style: CursorBlock},
	}, nil
}

func BlankState(cols, rows uint16) (State, error) {
	frame, err := BlankFrame(cols, rows)
	if err != nil {
		return State{}, err
	}
	return State{Frame: frame}, nil
}

func (f Frame) Clone() Frame {
	out := f
	out.Lines = cloneRows(f.Lines)
	return out
}

func (s State) Clone() State {
	return State{Frame: s.Frame.Clone(), History: cloneRows(s.History)}
}

func (d Delta) Clone() Delta {
	out := d
	out.Lines = make([]LineDelta, len(d.Lines))
	for i, line := range d.Lines {
		out.Lines[i] = LineDelta{Y: line.Y, Cells: cloneRow(line.Cells)}
	}
	out.HistoryAppend = cloneRows(d.HistoryAppend)
	return out
}

func (f Frame) Validate() error {
	if err := validateDimensions(f.Cols, f.Rows); err != nil {
		return err
	}
	if len(f.Lines) != int(f.Rows) {
		return fmt.Errorf("frame has %d lines, want %d", len(f.Lines), f.Rows)
	}
	for y, row := range f.Lines {
		if err := validateRow(row, f.Cols); err != nil {
			return fmt.Errorf("frame row %d: %w", y, err)
		}
	}
	if f.Cursor.X >= f.Cols || f.Cursor.Y >= f.Rows {
		return fmt.Errorf("cursor (%d,%d) outside %dx%d frame", f.Cursor.X, f.Cursor.Y, f.Cols, f.Rows)
	}
	if f.Cursor.Style > CursorHollowBlock {
		return fmt.Errorf("unknown cursor style %d", f.Cursor.Style)
	}
	if err := validateMetadata(f.Title, "title"); err != nil {
		return err
	}
	if err := validateMetadata(f.WorkingDirectory, "working directory"); err != nil {
		return err
	}
	return nil
}

func (s State) Validate() error {
	if err := s.Frame.Validate(); err != nil {
		return err
	}
	if len(s.History) > MaxHistoryRows {
		return fmt.Errorf("history has %d rows, max %d", len(s.History), MaxHistoryRows)
	}
	for i, row := range s.History {
		if err := validateRow(row, s.Frame.Cols); err != nil {
			return fmt.Errorf("history row %d: %w", i, err)
		}
	}
	return nil
}

func (d Delta) Validate() error {
	if err := validateDimensions(d.Cols, d.Rows); err != nil {
		return err
	}
	if d.Cursor.X >= d.Cols || d.Cursor.Y >= d.Rows {
		return fmt.Errorf("delta cursor (%d,%d) outside %dx%d", d.Cursor.X, d.Cursor.Y, d.Cols, d.Rows)
	}
	if d.Cursor.Style > CursorHollowBlock {
		return fmt.Errorf("unknown cursor style %d", d.Cursor.Style)
	}
	if err := validateMetadata(d.Title, "title"); err != nil {
		return err
	}
	if err := validateMetadata(d.WorkingDirectory, "working directory"); err != nil {
		return err
	}
	seen := make(map[uint16]struct{}, len(d.Lines))
	for i, line := range d.Lines {
		if line.Y >= d.Rows {
			return fmt.Errorf("delta line %d has y=%d outside rows=%d", i, line.Y, d.Rows)
		}
		if _, ok := seen[line.Y]; ok {
			return fmt.Errorf("delta contains duplicate row %d", line.Y)
		}
		seen[line.Y] = struct{}{}
		if err := validateRow(line.Cells, d.Cols); err != nil {
			return fmt.Errorf("delta row %d: %w", line.Y, err)
		}
	}
	if d.Full && len(d.Lines) != int(d.Rows) {
		return fmt.Errorf("full delta has %d lines, want %d", len(d.Lines), d.Rows)
	}
	if len(d.HistoryAppend) > MaxHistoryRows {
		return fmt.Errorf("delta appends too many history rows: %d", len(d.HistoryAppend))
	}
	for i, row := range d.HistoryAppend {
		if err := validateRow(row, d.Cols); err != nil {
			return fmt.Errorf("delta history row %d: %w", i, err)
		}
	}
	return nil
}

func validateDimensions(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return errors.New("screen dimensions must be positive")
	}
	if uint64(cols)*uint64(rows) > MaxCells {
		return fmt.Errorf("screen dimensions %dx%d exceed %d cells", cols, rows, MaxCells)
	}
	return nil
}

func validateRow(row Row, cols uint16) error {
	if len(row) != int(cols) {
		return fmt.Errorf("row has %d cells, want %d", len(row), cols)
	}
	for x, cell := range row {
		if cell.Width > 2 {
			return fmt.Errorf("cell %d has invalid width %d", x, cell.Width)
		}
		if len(cell.Text) > MaxCellBytes {
			return fmt.Errorf("cell %d text exceeds %d bytes", x, MaxCellBytes)
		}
		if !utf8.ValidString(cell.Text) {
			return fmt.Errorf("cell %d text is not UTF-8", x)
		}
		for _, r := range cell.Text {
			if unicode.IsControl(r) {
				return fmt.Errorf("cell %d contains control rune U+%04X", x, r)
			}
		}
		if cell.Width == 0 && cell.Text != "" {
			return fmt.Errorf("wide-tail cell %d contains text", x)
		}
	}
	return nil
}

func validateMetadata(value, label string) error {
	if len(value) > MaxTitleBytes {
		return fmt.Errorf("%s exceeds %d bytes", label, MaxTitleBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not UTF-8", label)
	}
	for _, r := range value {
		if r == 0 || (unicode.IsControl(r) && r != '\t') {
			return fmt.Errorf("%s contains control rune U+%04X", label, r)
		}
	}
	return nil
}

func blankRow(cols uint16) Row {
	row := make(Row, int(cols))
	for i := range row {
		row[i].Width = 1
	}
	return row
}

func cloneRows(rows []Row) []Row {
	out := make([]Row, len(rows))
	for i, row := range rows {
		out[i] = cloneRow(row)
	}
	return out
}

func cloneRow(row Row) Row {
	return append(Row(nil), row...)
}

// EqualState reports canonical screen-state equality.
func EqualState(a, b State) bool {
	return a.Frame.Cols == b.Frame.Cols &&
		a.Frame.Rows == b.Frame.Rows &&
		a.Frame.Cursor == b.Frame.Cursor &&
		a.Frame.AltScreen == b.Frame.AltScreen &&
		a.Frame.Title == b.Frame.Title &&
		a.Frame.WorkingDirectory == b.Frame.WorkingDirectory &&
		a.Frame.DefaultFG == b.Frame.DefaultFG &&
		a.Frame.DefaultBG == b.Frame.DefaultBG &&
		equalRows(a.Frame.Lines, b.Frame.Lines) &&
		equalRows(a.History, b.History)
}

func equalRows(a, b []Row) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for x := range a[i] {
			if a[i][x] != b[i][x] {
				return false
			}
		}
	}
	return true
}
