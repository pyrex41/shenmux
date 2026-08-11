// How many terminal cells the browser viewport can hold, and by how much the
// glyphs have to shrink when it cannot hold enough. Kept free of browser
// globals for the same reason as gateway.mjs: the arithmetic that decides what
// a phone gets is worth testing without a phone.
//
// Two separate questions live here, and conflating them is what let the
// terminal draw wider than the window. `fitGrid` answers "what should the PTY
// be", which only a client holding the control lease may act on. `renderScale`
// answers "how do I draw the grid I actually have inside the box I actually
// have", which every client must answer for itself — an observer cannot resize
// someone else's PTY, so its only remedy for a grid too wide for its window is
// to shrink the glyphs.

// Below this many columns a terminal stops being a terminal: prompts and
// tables wrap into unreadable stripes. A narrow window shrinks the font to
// keep this many columns rather than handing the PTY a width no program can
// use.
export const MIN_COLS = 40;
export const MIN_ROWS = 12;
// Shrinking has a floor of its own. Past this the text is too small to read,
// so a coarser grid — or, for an observer, a scrollable one — is the better
// trade.
export const MIN_SCALE = 0.55;
// Sanity bounds, not layout: they exist so a pathological window cannot ask a
// PTY for a grid nothing can render, and are far enough out that an ordinary
// desktop display is fitted, not clipped.
export const MAX_COLS = 300;
export const MAX_ROWS = 120;

const clamp = (value, low, high) => Math.max(low, Math.min(high, value));
// A cell count that survives its own arithmetic: the scale below is derived
// from a division, so an exact fit comes back as 39.999999999999996 and a
// plain floor would silently drop the column it was chosen to keep.
const cells = (available, unit) => Math.floor(available / unit + 1e-9);

// The grid that fills `width`x`height` CSS pixels, plus the glyph scale that
// grid is measured at. `cellWidth`/`lineHeight` are the unscaled cell metrics
// the renderer's bitmap font produces.
export function fitGrid({ width, height, cellWidth, lineHeight }) {
  const usableWidth = Math.max(0, width);
  const usableHeight = Math.max(0, height);
  if (cellWidth <= 0 || lineHeight <= 0) return { cols: MIN_COLS, rows: MIN_ROWS, scale: 1 };
  const scale = clamp(Math.min(
    usableWidth / (MIN_COLS * cellWidth),
    usableHeight / (MIN_ROWS * lineHeight),
  ), MIN_SCALE, 1);
  return {
    cols: clamp(cells(usableWidth, cellWidth * scale), 1, MAX_COLS),
    rows: clamp(cells(usableHeight, lineHeight * scale), 1, MAX_ROWS),
    scale,
  };
}

// The scale at which a grid of `cols`x`rows` fits the box. Never magnifies:
// a grid smaller than the window is drawn at its natural size and the slack is
// left as background. Bottoms out at MIN_SCALE, which is the one case that
// still overflows — the window scrolls to reach the rest, which beats
// illegible text.
export function renderScale({ width, height, cols, rows, cellWidth, lineHeight }) {
  if (cols <= 0 || rows <= 0 || cellWidth <= 0 || lineHeight <= 0) return 1;
  return clamp(Math.min(
    width / (cols * cellWidth),
    height / (rows * lineHeight),
  ), MIN_SCALE, 1);
}
