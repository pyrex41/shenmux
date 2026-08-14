package screen

import (
	"fmt"
	"slices"
)

// Advance converts a fresh authoritative frame into a bounded canonical state
// and a side-effect-free delta. History is derived only from exact visible
// upward scrolls, so it is conservative: uncertain changes never invent rows.
func Advance(previous State, next Frame, historyLimit int) (State, Delta, error) {
	if err := next.Validate(); err != nil {
		return State{}, Delta{}, err
	}
	if historyLimit < 0 || historyLimit > MaxHistoryRows {
		return State{}, Delta{}, fmt.Errorf("history limit %d outside 0..%d", historyLimit, MaxHistoryRows)
	}

	first := previous.Frame.Cols == 0 || previous.Frame.Rows == 0
	if !first {
		if err := previous.Validate(); err != nil {
			return State{}, Delta{}, fmt.Errorf("previous state: %w", err)
		}
	}

	state := State{Frame: next.Clone(), History: cloneRows(previous.History)}
	if !first && previous.Frame.Cols != next.Cols {
		state.History = nil
	}
	delta := Delta{
		Cols: next.Cols, Rows: next.Rows,
		Cursor: next.Cursor, AltScreen: next.AltScreen,
		Title: next.Title, WorkingDirectory: next.WorkingDirectory,
		DefaultFG: next.DefaultFG, DefaultBG: next.DefaultBG,
		Modes: next.Modes,
	}

	full := first || previous.Frame.Cols != next.Cols || previous.Frame.Rows != next.Rows || previous.Frame.AltScreen != next.AltScreen
	delta.Full = full
	if !first && previous.Frame.Cols != next.Cols && len(previous.History) != 0 {
		delta.HistoryReset = true
	}

	if !first && !full && !next.AltScreen && historyLimit > 0 {
		if scrolled := exactScrollUp(previous.Frame, next); scrolled > 0 {
			appended := cloneRows(previous.Frame.Lines[:scrolled])
			state.History = append(state.History, appended...)
			delta.HistoryAppend = appended
		}
	}

	if historyLimit == 0 {
		if len(state.History) != 0 {
			delta.HistoryReset = true
		}
		state.History = nil
	} else if len(state.History) > historyLimit {
		drop := len(state.History) - historyLimit
		state.History = cloneRows(state.History[drop:])
		if drop > int(^uint32(0)) {
			return State{}, Delta{}, fmt.Errorf("history drop overflow: %d", drop)
		}
		delta.HistoryDrop = uint32(drop)
	}

	if full {
		delta.Lines = make([]LineDelta, len(next.Lines))
		for y, row := range next.Lines {
			delta.Lines[y] = LineDelta{Y: uint16(y), Cells: cloneRow(row)}
		}
	} else {
		for y, row := range next.Lines {
			if !slices.Equal(previous.Frame.Lines[y], row) {
				delta.Lines = append(delta.Lines, LineDelta{Y: uint16(y), Cells: cloneRow(row)})
			}
		}
	}
	if err := state.Validate(); err != nil {
		return State{}, Delta{}, err
	}
	if err := delta.Validate(); err != nil {
		return State{}, Delta{}, err
	}
	return state, delta, nil
}

// Apply applies a validated delta without interpreting terminal bytes.
func Apply(previous State, delta Delta, historyLimit int) (State, error) {
	if err := delta.Validate(); err != nil {
		return State{}, err
	}
	if historyLimit < 0 || historyLimit > MaxHistoryRows {
		return State{}, fmt.Errorf("history limit %d outside 0..%d", historyLimit, MaxHistoryRows)
	}

	var state State
	if delta.Full || previous.Frame.Cols != delta.Cols || previous.Frame.Rows != delta.Rows {
		frame, err := BlankFrame(delta.Cols, delta.Rows)
		if err != nil {
			return State{}, err
		}
		state = State{Frame: frame, History: cloneRows(previous.History)}
	} else {
		if err := previous.Validate(); err != nil {
			return State{}, fmt.Errorf("previous state: %w", err)
		}
		state = previous.Clone()
	}

	state.Frame.Cols = delta.Cols
	state.Frame.Rows = delta.Rows
	state.Frame.Cursor = delta.Cursor
	state.Frame.AltScreen = delta.AltScreen
	state.Frame.Title = delta.Title
	state.Frame.WorkingDirectory = delta.WorkingDirectory
	state.Frame.DefaultFG = delta.DefaultFG
	state.Frame.DefaultBG = delta.DefaultBG
	state.Frame.Modes = delta.Modes

	for _, line := range delta.Lines {
		state.Frame.Lines[line.Y] = cloneRow(line.Cells)
	}

	if delta.HistoryReset {
		state.History = nil
	}
	if int(delta.HistoryDrop) > len(state.History) {
		return State{}, fmt.Errorf("delta drops %d history rows from %d", delta.HistoryDrop, len(state.History))
	}
	if delta.HistoryDrop != 0 {
		state.History = cloneRows(state.History[int(delta.HistoryDrop):])
	}
	state.History = append(state.History, cloneRows(delta.HistoryAppend)...)
	if historyLimit == 0 {
		state.History = nil
	} else if len(state.History) > historyLimit {
		state.History = cloneRows(state.History[len(state.History)-historyLimit:])
	}
	if err := state.Validate(); err != nil {
		return State{}, err
	}
	return state, nil
}

func exactScrollUp(previous, next Frame) int {
	if previous.Cols != next.Cols || previous.Rows != next.Rows || previous.AltScreen != next.AltScreen {
		return 0
	}
	// Repeated rows (especially a blank screen) satisfy every shifted suffix.
	// An unchanged frame is not evidence that a scroll occurred.
	if equalRows(previous.Lines, next.Lines) {
		return 0
	}
	rows := int(previous.Rows)
	for shift := 1; shift < rows; shift++ {
		match := true
		for y := 0; y < rows-shift; y++ {
			if !slices.Equal(previous.Lines[y+shift], next.Lines[y]) {
				match = false
				break
			}
		}
		if match {
			return shift
		}
	}
	return 0
}
