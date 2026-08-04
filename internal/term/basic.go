package term

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/pyrex41/shenmux/internal/screen"
	"github.com/pyrex41/shenmux/internal/shenguard"
)

const (
	maxCSIBytes    = 4096
	maxControlText = 64 << 10
)

type parserMode uint8

const (
	modeGround parserMode = iota
	modeEscape
	modeEscapeCharset
	modeCSI
	modeOSC
	modeOSCEscape
	modeDiscardString
	modeDiscardEscape
)

type cursorState struct {
	x, y  int
	style screen.Style
}

// Basic is a bounded, dependency-free VT engine used by the default build and
// tests. The libghostty build tag replaces it with Ghostty for full terminal
// fidelity, but both implementations expose the same interpreted-state and
// effect boundary.
type Basic struct {
	mu sync.Mutex

	dim       shenguard.Dimensions
	primary   []screen.Row
	alternate []screen.Row
	alt       bool

	x, y        int
	saved       cursorState
	cursor      screen.Cursor
	style       screen.Style
	modes       screen.InputModes
	wrap        bool
	pendingWrap bool
	scrollTop   int
	scrollBot   int

	mode      parserMode
	csi       []byte
	control   []byte
	utf8Bytes []byte

	title  string
	pwd    string
	closed bool
}

func NewBasic(dim shenguard.Dimensions) *Basic {
	b := &Basic{dim: dim, wrap: true}
	b.cursor = screen.Cursor{Visible: true, Style: screen.CursorBlock, Blink: true}
	b.primary = makeGrid(dim.Cols(), dim.Rows())
	b.alternate = makeGrid(dim.Cols(), dim.Rows())
	b.scrollBot = int(dim.Rows()) - 1
	return b
}

func (b *Basic) Feed(data []byte) (Effects, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return Effects{}, fmt.Errorf("terminal is closed")
	}
	var effects Effects
	for _, ch := range data {
		b.processByte(ch, &effects)
	}
	return effects.Clone(), nil
}

func (b *Basic) Resize(dim shenguard.Dimensions) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return fmt.Errorf("terminal is closed")
	}
	b.primary = resizeGrid(b.primary, dim.Cols(), dim.Rows())
	b.alternate = resizeGrid(b.alternate, dim.Cols(), dim.Rows())
	b.dim = dim
	b.x = clamp(b.x, 0, int(dim.Cols())-1)
	b.y = clamp(b.y, 0, int(dim.Rows())-1)
	b.scrollTop = 0
	b.scrollBot = int(dim.Rows()) - 1
	b.pendingWrap = false
	return nil
}

func (b *Basic) Snapshot() (screen.Frame, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return screen.Frame{}, fmt.Errorf("terminal is closed")
	}
	grid := b.activeGrid()
	frame := screen.Frame{
		Cols: b.dim.Cols(), Rows: b.dim.Rows(), Lines: cloneGrid(grid),
		Cursor: b.cursor, AltScreen: b.alt, Title: b.title,
		WorkingDirectory: b.pwd,
		Modes:            b.modes,
	}
	frame.Cursor.X = uint16(clamp(b.x, 0, int(b.dim.Cols())-1))
	frame.Cursor.Y = uint16(clamp(b.y, 0, int(b.dim.Rows())-1))
	if err := frame.Validate(); err != nil {
		return screen.Frame{}, err
	}
	return frame, nil
}

func (b *Basic) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.primary = nil
	b.alternate = nil
	return nil
}

func (b *Basic) processByte(ch byte, effects *Effects) {
	switch b.mode {
	case modeEscape:
		b.processEscape(ch, effects)
		return
	case modeEscapeCharset:
		b.mode = modeGround
		return
	case modeCSI:
		if ch >= 0x40 && ch <= 0x7e {
			b.applyCSI(ch, string(b.csi), effects)
			b.csi = b.csi[:0]
			b.mode = modeGround
		} else if len(b.csi) < maxCSIBytes {
			b.csi = append(b.csi, ch)
		} else {
			b.csi = b.csi[:0]
			b.mode = modeGround
		}
		return
	case modeOSC:
		switch ch {
		case 0x07:
			b.finishOSC(effects)
			b.mode = modeGround
		case 0x1b:
			b.mode = modeOSCEscape
		default:
			if len(b.control) < maxControlText {
				b.control = append(b.control, ch)
			}
		}
		return
	case modeOSCEscape:
		if ch == '\\' {
			b.finishOSC(effects)
			b.mode = modeGround
		} else {
			if len(b.control)+2 <= maxControlText {
				b.control = append(b.control, 0x1b, ch)
			}
			b.mode = modeOSC
		}
		return
	case modeDiscardString:
		if ch == 0x1b {
			b.mode = modeDiscardEscape
		}
		return
	case modeDiscardEscape:
		if ch == '\\' {
			b.mode = modeGround
		} else if ch != 0x1b {
			b.mode = modeDiscardString
		}
		return
	}

	if len(b.utf8Bytes) != 0 {
		if ch&0xc0 == 0x80 {
			b.utf8Bytes = append(b.utf8Bytes, ch)
			if utf8.FullRune(b.utf8Bytes) {
				r, size := utf8.DecodeRune(b.utf8Bytes)
				if r == utf8.RuneError && size == 1 {
					b.putRune(utf8.RuneError)
				} else {
					b.putRune(r)
				}
				b.utf8Bytes = b.utf8Bytes[:0]
			}
			return
		}
		b.putRune(utf8.RuneError)
		b.utf8Bytes = b.utf8Bytes[:0]
	}

	if ch >= utf8.RuneSelf {
		b.utf8Bytes = append(b.utf8Bytes[:0], ch)
		if utf8.FullRune(b.utf8Bytes) {
			r, _ := utf8.DecodeRune(b.utf8Bytes)
			b.putRune(r)
			b.utf8Bytes = b.utf8Bytes[:0]
		}
		return
	}

	switch ch {
	case 0x00, 0x7f:
		return
	case 0x05: // ENQ; no answerback configured.
		return
	case 0x07:
		effects.Bells++
	case '\b':
		b.pendingWrap = false
		if b.x > 0 {
			b.x--
		}
	case '\t':
		b.pendingWrap = false
		b.x = clamp(((b.x/8)+1)*8, 0, int(b.dim.Cols())-1)
	case '\n', '\v', '\f':
		b.pendingWrap = false
		b.lineFeed()
	case '\r':
		b.pendingWrap = false
		b.x = 0
	case 0x1b:
		b.mode = modeEscape
	default:
		if ch >= 0x20 {
			b.putRune(rune(ch))
		}
	}
}

func (b *Basic) processEscape(ch byte, effects *Effects) {
	b.mode = modeGround
	switch ch {
	case '[':
		b.csi = b.csi[:0]
		b.mode = modeCSI
	case ']':
		b.control = b.control[:0]
		b.mode = modeOSC
	case 'P', '_', '^':
		b.mode = modeDiscardString
	case '(', ')', '*', '+', '-', '.', '/':
		b.mode = modeEscapeCharset
	case '7':
		b.saved = cursorState{x: b.x, y: b.y, style: b.style}
	case '8':
		b.x, b.y, b.style = b.saved.x, b.saved.y, b.saved.style
		b.clampCursor()
	case 'D':
		b.lineFeed()
	case 'E':
		b.x = 0
		b.lineFeed()
	case 'M':
		b.reverseIndex()
	case 'c':
		b.reset()
	case 'Z':
		effects.PTYWrites = append(effects.PTYWrites, []byte("\x1b[?1;2c"))
	}
}

func (b *Basic) putRune(r rune) {
	width := runeWidth(r)
	if width == 0 {
		b.appendCombining(r)
		return
	}
	if width > 2 {
		width = 1
	}
	cols := int(b.dim.Cols())
	if b.pendingWrap && b.wrap {
		b.x = 0
		b.lineFeed()
		b.pendingWrap = false
	}
	if width == 2 && b.x == cols-1 {
		if b.wrap {
			b.x = 0
			b.lineFeed()
		} else {
			width = 1
		}
	}
	grid := b.activeGrid()
	row := grid[b.y]
	b.clearWideAt(row, b.x)
	row[b.x] = screen.Cell{Text: string(r), Width: uint8(width), Style: b.style}
	if width == 2 {
		b.clearWideAt(row, b.x+1)
		row[b.x+1] = screen.Cell{Width: 0, Style: b.style}
	}
	if b.x+width >= cols {
		b.x = cols - 1
		b.pendingWrap = b.wrap
	} else {
		b.x += width
		b.pendingWrap = false
	}
}

func (b *Basic) appendCombining(r rune) {
	grid := b.activeGrid()
	x := b.x - 1
	y := b.y
	if b.pendingWrap {
		x = int(b.dim.Cols()) - 1
	}
	if x < 0 || y < 0 || y >= len(grid) {
		return
	}
	for x >= 0 && grid[y][x].Width == 0 {
		x--
	}
	if x < 0 {
		return
	}
	cell := &grid[y][x]
	if len(cell.Text)+utf8.RuneLen(r) <= screen.MaxCellBytes {
		cell.Text += string(r)
	}
}

func (b *Basic) clearWideAt(row screen.Row, x int) {
	if x < 0 || x >= len(row) {
		return
	}
	if row[x].Width == 0 && x > 0 && row[x-1].Width == 2 {
		row[x-1] = blankCell()
	}
	if row[x].Width == 2 && x+1 < len(row) {
		row[x+1] = blankCell()
	}
	row[x] = blankCell()
}

func (b *Basic) lineFeed() {
	if b.y == b.scrollBot {
		b.scrollUp(1)
		return
	}
	if b.y < int(b.dim.Rows())-1 {
		b.y++
	}
}

func (b *Basic) reverseIndex() {
	if b.y == b.scrollTop {
		b.scrollDown(1)
		return
	}
	if b.y > 0 {
		b.y--
	}
}

func (b *Basic) scrollUp(count int) {
	count = clamp(count, 0, b.scrollBot-b.scrollTop+1)
	if count == 0 {
		return
	}
	grid := b.activeGrid()
	copy(grid[b.scrollTop:b.scrollBot-count+1], grid[b.scrollTop+count:b.scrollBot+1])
	for y := b.scrollBot - count + 1; y <= b.scrollBot; y++ {
		grid[y] = blankRow(b.dim.Cols())
	}
}

func (b *Basic) scrollDown(count int) {
	count = clamp(count, 0, b.scrollBot-b.scrollTop+1)
	if count == 0 {
		return
	}
	grid := b.activeGrid()
	copy(grid[b.scrollTop+count:b.scrollBot+1], grid[b.scrollTop:b.scrollBot-count+1])
	for y := b.scrollTop; y < b.scrollTop+count; y++ {
		grid[y] = blankRow(b.dim.Cols())
	}
}

func (b *Basic) applyCSI(final byte, raw string, effects *Effects) {
	prefix, paramsText, intermediate := splitCSI(raw)
	params := parseCSIParams(paramsText)
	first := param(params, 0, 1)
	b.pendingWrap = false

	switch final {
	case 'A':
		b.y -= first
	case 'B', 'e':
		b.y += first
	case 'C', 'a':
		b.x += first
	case 'D':
		b.x -= first
	case 'E':
		b.y += first
		b.x = 0
	case 'F':
		b.y -= first
		b.x = 0
	case 'G', '`':
		b.x = first - 1
	case 'd':
		b.y = first - 1
	case 'H', 'f':
		b.y = param(params, 0, 1) - 1
		b.x = param(params, 1, 1) - 1
	case 'J':
		b.eraseDisplay(param(params, 0, 0))
	case 'K':
		b.eraseLine(param(params, 0, 0))
	case 'm':
		b.applySGR(params)
	case 'h', 'l':
		b.setModes(prefix == "?", params, final == 'h')
	case 's':
		b.saved = cursorState{x: b.x, y: b.y, style: b.style}
	case 'u':
		b.x, b.y, b.style = b.saved.x, b.saved.y, b.saved.style
	case 'r':
		top := param(params, 0, 1) - 1
		bottom := param(params, 1, int(b.dim.Rows())) - 1
		if top >= 0 && bottom < int(b.dim.Rows()) && top < bottom {
			b.scrollTop, b.scrollBot = top, bottom
			b.x, b.y = 0, 0
		}
	case 'S':
		b.scrollUp(first)
	case 'T':
		b.scrollDown(first)
	case 'L':
		b.insertLines(first)
	case 'M':
		b.deleteLines(first)
	case '@':
		b.insertChars(first)
	case 'P':
		b.deleteChars(first)
	case 'X':
		b.eraseChars(first)
	case 'n':
		b.deviceStatus(prefix == "?", first, effects)
	case 'c':
		if prefix == ">" {
			effects.PTYWrites = append(effects.PTYWrites, []byte("\x1b[>0;95;0c"))
		} else {
			effects.PTYWrites = append(effects.PTYWrites, []byte("\x1b[?1;2c"))
		}
	case 't':
		if first == 18 {
			effects.PTYWrites = append(effects.PTYWrites, []byte(fmt.Sprintf("\x1b[8;%d;%dt", b.dim.Rows(), b.dim.Cols())))
		}
	case 'q':
		if intermediate == " " {
			b.setCursorStyle(first)
		}
	case 'p':
		if prefix == "?" && intermediate == "$" {
			b.reportMode(params, effects)
		}
	}
	b.clampCursor()
}

func splitCSI(raw string) (prefix, params, intermediate string) {
	start := 0
	if len(raw) != 0 && (raw[0] == '?' || raw[0] == '>' || raw[0] == '!') {
		prefix = raw[:1]
		start = 1
	}
	end := len(raw)
	for end > start && raw[end-1] >= 0x20 && raw[end-1] <= 0x2f {
		end--
	}
	params = raw[start:end]
	intermediate = raw[end:]
	return
}

func parseCSIParams(raw string) []int {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ";")
	out := make([]int, len(parts))
	for i, part := range parts {
		if part == "" {
			out[i] = 0
			continue
		}
		// Colon subparameters are accepted by using the first component.
		if colon := strings.IndexByte(part, ':'); colon >= 0 {
			part = part[:colon]
		}
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			out[i] = 0
		} else {
			out[i] = value
		}
	}
	return out
}

func param(params []int, index, defaultValue int) int {
	if index >= len(params) || params[index] == 0 {
		return defaultValue
	}
	return params[index]
}

func (b *Basic) eraseDisplay(mode int) {
	grid := b.activeGrid()
	switch mode {
	case 0:
		b.eraseLine(0)
		for y := b.y + 1; y < len(grid); y++ {
			grid[y] = blankRow(b.dim.Cols())
		}
	case 1:
		for y := 0; y < b.y; y++ {
			grid[y] = blankRow(b.dim.Cols())
		}
		b.eraseLine(1)
	case 2, 3:
		for y := range grid {
			grid[y] = blankRow(b.dim.Cols())
		}
	}
}

func (b *Basic) eraseLine(mode int) {
	row := b.activeGrid()[b.y]
	switch mode {
	case 0:
		for x := b.x; x < len(row); x++ {
			row[x] = blankCell()
		}
	case 1:
		for x := 0; x <= b.x && x < len(row); x++ {
			row[x] = blankCell()
		}
	case 2:
		for x := range row {
			row[x] = blankCell()
		}
	}
}

func (b *Basic) insertLines(count int) {
	if b.y < b.scrollTop || b.y > b.scrollBot {
		return
	}
	count = clamp(count, 1, b.scrollBot-b.y+1)
	grid := b.activeGrid()
	copy(grid[b.y+count:b.scrollBot+1], grid[b.y:b.scrollBot-count+1])
	for y := b.y; y < b.y+count; y++ {
		grid[y] = blankRow(b.dim.Cols())
	}
}

func (b *Basic) deleteLines(count int) {
	if b.y < b.scrollTop || b.y > b.scrollBot {
		return
	}
	count = clamp(count, 1, b.scrollBot-b.y+1)
	grid := b.activeGrid()
	copy(grid[b.y:b.scrollBot-count+1], grid[b.y+count:b.scrollBot+1])
	for y := b.scrollBot - count + 1; y <= b.scrollBot; y++ {
		grid[y] = blankRow(b.dim.Cols())
	}
}

func (b *Basic) insertChars(count int) {
	row := b.activeGrid()[b.y]
	count = clamp(count, 1, len(row)-b.x)
	copy(row[b.x+count:], row[b.x:len(row)-count])
	for x := b.x; x < b.x+count; x++ {
		row[x] = blankCell()
	}
}

func (b *Basic) deleteChars(count int) {
	row := b.activeGrid()[b.y]
	count = clamp(count, 1, len(row)-b.x)
	copy(row[b.x:], row[b.x+count:])
	for x := len(row) - count; x < len(row); x++ {
		row[x] = blankCell()
	}
}

func (b *Basic) eraseChars(count int) {
	row := b.activeGrid()[b.y]
	end := clamp(b.x+count, b.x, len(row))
	for x := b.x; x < end; x++ {
		row[x] = blankCell()
	}
}

func (b *Basic) setModes(private bool, params []int, enabled bool) {
	if !private {
		return
	}
	for _, mode := range params {
		switch mode {
		case 7:
			b.wrap = enabled
		case 25:
			b.cursor.Visible = enabled
		case 47, 1047, 1049:
			if enabled != b.alt {
				if enabled {
					b.saved = cursorState{x: b.x, y: b.y, style: b.style}
					b.alternate = makeGrid(b.dim.Cols(), b.dim.Rows())
					b.x, b.y = 0, 0
				} else {
					b.x, b.y, b.style = b.saved.x, b.saved.y, b.saved.style
				}
				b.alt = enabled
				b.scrollTop, b.scrollBot = 0, int(b.dim.Rows())-1
			}
		case 1000:
			if enabled {
				b.modes.Mouse = screen.MouseClick
			} else if b.modes.Mouse == screen.MouseClick {
				b.modes.Mouse = screen.MouseNone
			}
		case 1002:
			if enabled {
				b.modes.Mouse = screen.MouseButton
			} else if b.modes.Mouse == screen.MouseButton {
				b.modes.Mouse = screen.MouseNone
			}
		case 1003:
			if enabled {
				b.modes.Mouse = screen.MouseAny
			} else if b.modes.Mouse == screen.MouseAny {
				b.modes.Mouse = screen.MouseNone
			}
		case 1004:
			b.modes.FocusEvents = enabled
		case 1006:
			b.modes.MouseSGR = enabled
		case 2004:
			b.modes.BracketedPaste = enabled
		}
	}
}

func (b *Basic) deviceStatus(private bool, code int, effects *Effects) {
	switch code {
	case 5:
		effects.PTYWrites = append(effects.PTYWrites, []byte("\x1b[0n"))
	case 6:
		if private {
			effects.PTYWrites = append(effects.PTYWrites, []byte(fmt.Sprintf("\x1b[?%d;%dR", b.y+1, b.x+1)))
		} else {
			effects.PTYWrites = append(effects.PTYWrites, []byte(fmt.Sprintf("\x1b[%d;%dR", b.y+1, b.x+1)))
		}
	}
}

func (b *Basic) reportMode(params []int, effects *Effects) {
	for _, mode := range params {
		state := 0
		switch mode {
		case 7:
			if b.wrap {
				state = 1
			} else {
				state = 2
			}
		case 25:
			if b.cursor.Visible {
				state = 1
			} else {
				state = 2
			}
		case 47, 1047, 1049:
			if b.alt {
				state = 1
			} else {
				state = 2
			}
		case 1000:
			if b.modes.Mouse == screen.MouseClick {
				state = 1
			} else {
				state = 2
			}
		case 1002:
			if b.modes.Mouse == screen.MouseButton {
				state = 1
			} else {
				state = 2
			}
		case 1003:
			if b.modes.Mouse == screen.MouseAny {
				state = 1
			} else {
				state = 2
			}
		case 1004:
			if b.modes.FocusEvents {
				state = 1
			} else {
				state = 2
			}
		case 1006:
			if b.modes.MouseSGR {
				state = 1
			} else {
				state = 2
			}
		case 2004:
			if b.modes.BracketedPaste {
				state = 1
			} else {
				state = 2
			}
		}
		effects.PTYWrites = append(effects.PTYWrites, []byte(fmt.Sprintf("\x1b[?%d;%d$y", mode, state)))
	}
}

func (b *Basic) setCursorStyle(code int) {
	switch code {
	case 1:
		b.cursor.Style, b.cursor.Blink = screen.CursorBlock, true
	case 2:
		b.cursor.Style, b.cursor.Blink = screen.CursorBlock, false
	case 3:
		b.cursor.Style, b.cursor.Blink = screen.CursorUnderline, true
	case 4:
		b.cursor.Style, b.cursor.Blink = screen.CursorUnderline, false
	case 5:
		b.cursor.Style, b.cursor.Blink = screen.CursorBar, true
	case 6:
		b.cursor.Style, b.cursor.Blink = screen.CursorBar, false
	}
}

func (b *Basic) applySGR(params []int) {
	if len(params) == 0 {
		params = []int{0}
	}
	for i := 0; i < len(params); i++ {
		code := params[i]
		switch {
		case code == 0:
			b.style = screen.Style{}
		case code == 1:
			b.style.Bold = true
		case code == 2:
			b.style.Faint = true
		case code == 3:
			b.style.Italic = true
		case code == 4:
			b.style.Underline, b.style.DoubleUnderline = true, false
		case code == 5 || code == 6:
			b.style.Blink = true
		case code == 7:
			b.style.Inverse = true
		case code == 8:
			b.style.Invisible = true
		case code == 9:
			b.style.Strikethrough = true
		case code == 21:
			b.style.DoubleUnderline, b.style.Underline = true, false
		case code == 22:
			b.style.Bold, b.style.Faint = false, false
		case code == 23:
			b.style.Italic = false
		case code == 24:
			b.style.Underline, b.style.DoubleUnderline = false, false
		case code == 25:
			b.style.Blink = false
		case code == 27:
			b.style.Inverse = false
		case code == 28:
			b.style.Invisible = false
		case code == 29:
			b.style.Strikethrough = false
		case code == 39:
			b.style.FG = screen.Color{}
		case code == 49:
			b.style.BG = screen.Color{}
		case code == 53:
			b.style.Overline = true
		case code == 55:
			b.style.Overline = false
		case code >= 30 && code <= 37:
			b.style.FG = ansiColor(code - 30)
		case code >= 40 && code <= 47:
			b.style.BG = ansiColor(code - 40)
		case code >= 90 && code <= 97:
			b.style.FG = ansiColor(code - 90 + 8)
		case code >= 100 && code <= 107:
			b.style.BG = ansiColor(code - 100 + 8)
		case code == 38 || code == 48:
			color, consumed := parseExtendedColor(params[i+1:])
			if consumed != 0 {
				if code == 38 {
					b.style.FG = color
				} else {
					b.style.BG = color
				}
				i += consumed
			}
		}
	}
}

func parseExtendedColor(params []int) (screen.Color, int) {
	if len(params) >= 4 && params[0] == 2 {
		return screen.Color{Valid: true, R: uint8(clamp(params[1], 0, 255)), G: uint8(clamp(params[2], 0, 255)), B: uint8(clamp(params[3], 0, 255))}, 4
	}
	if len(params) >= 2 && params[0] == 5 {
		return xtermColor(clamp(params[1], 0, 255)), 2
	}
	return screen.Color{}, 0
}

func ansiColor(index int) screen.Color {
	palette := [...]screen.Color{
		{Valid: true, R: 0, G: 0, B: 0}, {Valid: true, R: 205, G: 49, B: 49}, {Valid: true, R: 13, G: 188, B: 121}, {Valid: true, R: 229, G: 229, B: 16},
		{Valid: true, R: 36, G: 114, B: 200}, {Valid: true, R: 188, G: 63, B: 188}, {Valid: true, R: 17, G: 168, B: 205}, {Valid: true, R: 229, G: 229, B: 229},
		{Valid: true, R: 102, G: 102, B: 102}, {Valid: true, R: 241, G: 76, B: 76}, {Valid: true, R: 35, G: 209, B: 139}, {Valid: true, R: 245, G: 245, B: 67},
		{Valid: true, R: 59, G: 142, B: 234}, {Valid: true, R: 214, G: 112, B: 214}, {Valid: true, R: 41, G: 184, B: 219}, {Valid: true, R: 255, G: 255, B: 255},
	}
	return palette[clamp(index, 0, len(palette)-1)]
}

func xtermColor(index int) screen.Color {
	if index < 16 {
		return ansiColor(index)
	}
	if index >= 232 {
		value := uint8(8 + (index-232)*10)
		return screen.Color{Valid: true, R: value, G: value, B: value}
	}
	index -= 16
	component := func(v int) uint8 {
		if v == 0 {
			return 0
		}
		return uint8(55 + v*40)
	}
	return screen.Color{Valid: true, R: component(index / 36), G: component((index / 6) % 6), B: component(index % 6)}
}

func (b *Basic) finishOSC(effects *Effects) {
	raw := string(b.control)
	b.control = b.control[:0]
	command, value, found := strings.Cut(raw, ";")
	if !found {
		return
	}
	switch command {
	case "0", "1", "2":
		title := sanitizeMetadata(value)
		if title != b.title {
			b.title = title
			effects.TitleChanged = true
		}
	case "7":
		pwd := sanitizeMetadata(value)
		if pwd != b.pwd {
			b.pwd = pwd
			effects.PWDChanged = true
		}
	case "52":
		effects.ClipboardWrites++
	case "9", "777":
		effects.Notifications++
	}
}

func sanitizeMetadata(value string) string {
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
	}
	var out strings.Builder
	for _, r := range value {
		if r == 0 || (unicode.IsControl(r) && r != '\t') {
			continue
		}
		if out.Len()+utf8.RuneLen(r) > screen.MaxTitleBytes {
			break
		}
		out.WriteRune(r)
	}
	return out.String()
}

func (b *Basic) reset() {
	b.primary = makeGrid(b.dim.Cols(), b.dim.Rows())
	b.alternate = makeGrid(b.dim.Cols(), b.dim.Rows())
	b.alt = false
	b.x, b.y = 0, 0
	b.saved = cursorState{}
	b.cursor = screen.Cursor{Visible: true, Style: screen.CursorBlock, Blink: true}
	b.style = screen.Style{}
	b.modes = screen.InputModes{}
	b.wrap = true
	b.pendingWrap = false
	b.scrollTop, b.scrollBot = 0, int(b.dim.Rows())-1
	b.title, b.pwd = "", ""
}

func (b *Basic) activeGrid() []screen.Row {
	if b.alt {
		return b.alternate
	}
	return b.primary
}

func (b *Basic) clampCursor() {
	b.x = clamp(b.x, 0, int(b.dim.Cols())-1)
	b.y = clamp(b.y, 0, int(b.dim.Rows())-1)
}

func makeGrid(cols, rows uint16) []screen.Row {
	grid := make([]screen.Row, int(rows))
	for y := range grid {
		grid[y] = blankRow(cols)
	}
	return grid
}

func resizeGrid(grid []screen.Row, cols, rows uint16) []screen.Row {
	out := makeGrid(cols, rows)
	for y := 0; y < len(out) && y < len(grid); y++ {
		copy(out[y], grid[y])
		for x := range out[y] {
			if out[y][x].Width == 0 && (x == 0 || out[y][x-1].Width != 2) {
				out[y][x] = blankCell()
			}
			if out[y][x].Width == 2 && x+1 >= len(out[y]) {
				out[y][x] = blankCell()
			}
		}
	}
	return out
}

func cloneGrid(grid []screen.Row) []screen.Row {
	out := make([]screen.Row, len(grid))
	for i, row := range grid {
		out[i] = append(screen.Row(nil), row...)
	}
	return out
}

func blankRow(cols uint16) screen.Row {
	row := make(screen.Row, int(cols))
	for i := range row {
		row[i] = blankCell()
	}
	return row
}

func blankCell() screen.Cell { return screen.Cell{Width: 1} }

func clamp(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func runeWidth(r rune) int {
	if r == 0 || unicode.IsControl(r) {
		return 0
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200d || (r >= 0xfe00 && r <= 0xfe0f) {
		return 0
	}
	if isWideRune(r) {
		return 2
	}
	return 1
}

func isWideRune(r rune) bool {
	return r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe19) ||
		(r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f300 && r <= 0x1faff) ||
		(r >= 0x20000 && r <= 0x3fffd))
}
