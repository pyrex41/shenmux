// Headless tests for the predictive echo state machine. No renderer, no
// browser: keystrokes and synthetic deltas in, overlay contents out.
//
//   node --test internal/webui/

import test from "node:test";
import assert from "node:assert/strict";

import { createPredictor, classifyKey, normalizeText } from "./predictive-echo.mjs";

const COLS = 20;
const ROWS = 5;

const cell = (text) => ({ Text: text, Width: 1, Style: {} });
const blankRow = (cols = COLS) => Array.from({ length: cols }, () => cell(""));

function makeFrame(overrides = {}) {
  const { cols = COLS, rows = ROWS, alt = false, cursor = { X: 0, Y: 0 }, lines } = overrides;
  // `echo: undefined` must mean "the server published no Echo field at all",
  // so it cannot go through a default parameter.
  const echo = Object.prototype.hasOwnProperty.call(overrides, "echo") ? overrides.echo : true;
  return {
    Cols: cols,
    Rows: rows,
    Lines: lines || Array.from({ length: rows }, () => blankRow(cols)),
    Cursor: { X: cursor.X, Y: cursor.Y, Visible: true, Style: 0 },
    AltScreen: alt,
    Modes: echo === undefined ? {} : { Echo: echo },
    DefaultFG: {},
    DefaultBG: {},
  };
}

/** A delta that repaints row `y` with `text` starting at column 0. */
function makeDelta(y, text, overrides = {}) {
  const cells = blankRow(overrides.cols || COLS);
  [...text].forEach((character, index) => { cells[index] = cell(character); });
  return {
    Cols: overrides.cols || COLS,
    Rows: overrides.rows || ROWS,
    Full: overrides.full || false,
    Lines: [{ Y: y, Cells: cells }],
    Cursor: { X: text.length, Y: y, Visible: true, Style: 0 },
    AltScreen: overrides.alt || false,
    Modes: overrides.echo === undefined ? { Echo: true } : { Echo: overrides.echo },
    ...overrides.extra,
  };
}

/** A predictor already past the RTT floor, as a live remote link would be. */
function ready(options = {}) {
  const predictor = createPredictor({ clock: () => 0, ...options });
  predictor.noteRTT(options.rtt === undefined ? 40 : options.rtt);
  return predictor;
}

const context = (overrides = {}) => ({
  frame: overrides.frame || makeFrame(),
  seq: overrides.seq === undefined ? 10 : overrides.seq,
  hasControl: overrides.hasControl === undefined ? true : overrides.hasControl,
  now: overrides.now === undefined ? 0 : overrides.now,
});

const key = (value, modifiers = {}) => ({ key: value, ctrlKey: false, altKey: false, metaKey: false, ...modifiers });

const overlayText = (predictor, y, cols = COLS) => {
  let out = "";
  for (let x = 0; x < cols; x++) {
    const found = predictor.cellAt(x, y);
    out += found ? (found.text || "_") : ".";
  }
  return out.replace(/\.+$/, "");
};

test("classifyKey admits printable characters and Backspace only", () => {
  assert.equal(classifyKey(key("a")).kind, "char");
  assert.equal(classifyKey(key(" ")).kind, "char");
  assert.equal(classifyKey(key("Backspace")).kind, "backspace");
  assert.equal(classifyKey(key("Enter")).kind, "other");
  assert.equal(classifyKey(key("ArrowLeft")).kind, "other");
  assert.equal(classifyKey(key("Tab")).kind, "other");
  assert.equal(classifyKey(key("Escape")).kind, "other");
  assert.equal(classifyKey(key("c", { ctrlKey: true })).kind, "other");
  assert.equal(classifyKey(key("v", { metaKey: true })).kind, "other");
  assert.equal(classifyKey(key("日")).kind, "other", "wide glyphs are not predicted");
  assert.equal(classifyKey(key("🙂")).kind, "other", "astral glyphs are not predicted");
  assert.equal(normalizeText(" "), "");
});

test("printable keys paint immediately and advance the predicted cursor", () => {
  const predictor = ready();
  assert.equal(predictor.onKey(key("l"), context()), "predicted");
  assert.equal(predictor.onKey(key("s"), context({ seq: 11 })), "predicted");
  assert.equal(overlayText(predictor, 0), "ls");
  assert.deepEqual(predictor.cursorPosition(), { x: 2, y: 0 });
  assert.deepEqual(predictor.activeRows(), [0]);
  assert.equal(predictor.size(), 2);
});

test("a matching delta confirms predictions and drops them from the overlay", () => {
  const predictor = ready();
  predictor.onKey(key("l"), context());
  predictor.onKey(key("s"), context({ seq: 11 }));
  assert.equal(predictor.size(), 2);

  const changed = predictor.onDelta(makeDelta(0, "ls"), 12, 20);
  assert.equal(changed, true);
  assert.equal(predictor.active(), false, "overlay is empty once the server agrees");
  assert.equal(predictor.cursorPosition(), null);
  assert.equal(predictor.stats().confirmed, 2);
  assert.equal(predictor.stats().mismatched, 0);
});

test("a delta that has not echoed yet leaves the prediction standing", () => {
  const predictor = ready();
  predictor.onKey(key("l"), context());
  // Unrelated repaint of the same row: the cell is still blank, which is the
  // value we predicted against, so this is "not yet" and not a mispredict.
  predictor.onDelta(makeDelta(0, ""), 11, 5);
  assert.equal(overlayText(predictor, 0), "l");
  assert.equal(predictor.stats().mismatched, 0);
});

test("a mismatching delta discards the whole epoch", () => {
  const predictor = ready();
  predictor.onKey(key("a"), context());
  predictor.onKey(key("b"), context({ seq: 11 }));
  predictor.onKey(key("c"), context({ seq: 12 }));
  assert.equal(predictor.size(), 3);

  predictor.onDelta(makeDelta(0, "X"), 13, 20);
  assert.equal(predictor.active(), false, "one wrong cell drops every prediction");
  assert.equal(predictor.cursorPosition(), null);
  assert.equal(predictor.stats().mismatched, 1);
});

test("older deltas cannot confirm or contradict a newer prediction", () => {
  const predictor = ready();
  predictor.onKey(key("a"), context({ seq: 10 }));
  // seq 10 is not newer than the prediction's seq: it cannot be a verdict.
  predictor.onDelta(makeDelta(0, "X"), 10, 5);
  assert.equal(overlayText(predictor, 0), "a");
  assert.equal(predictor.stats().mismatched, 0);
});

test("a prediction nobody echoes times out and disappears", () => {
  const predictor = ready({ rtt: 200 });
  predictor.onKey(key("a"), context());
  assert.equal(predictor.active(), true);
  assert.equal(predictor.nextDeadline(), 400, "2x RTT after the keystroke");

  assert.equal(predictor.tick(399), false, "still inside the window");
  assert.equal(predictor.active(), true);
  assert.equal(predictor.tick(401), true);
  assert.equal(predictor.active(), false);
  assert.equal(predictor.stats().timedOut, 1);
});

test("Echo === false suppresses prediction entirely", () => {
  const predictor = ready();
  const ctx = context({ frame: makeFrame({ echo: false }) });
  assert.equal(predictor.gateReason(ctx, 0), "echo-off");
  assert.equal(predictor.onKey(key("s"), ctx), "skipped");
  assert.equal(predictor.active(), false);
  assert.equal(overlayText(predictor, 0), "");
});

test("Echo absent suppresses prediction (fail closed against the pending server contract)", () => {
  const predictor = ready();
  const ctx = context({ frame: makeFrame({ echo: undefined }) });
  assert.equal(predictor.gateReason(ctx, 0), "echo-off");
  assert.equal(predictor.onKey(key("s"), ctx), "skipped");
  assert.equal(predictor.active(), false);
});

test("a password prompt turning echo off mid-epoch drops what is already predicted", () => {
  const predictor = ready();
  predictor.onKey(key("a"), context());
  assert.equal(predictor.active(), true);
  predictor.onDelta(makeDelta(0, "", { echo: false }), 11, 5);
  assert.equal(predictor.active(), false, "no glyph survives echo going off");
});

test("the alt screen suppresses prediction", () => {
  const predictor = ready();
  const ctx = context({ frame: makeFrame({ alt: true }) });
  assert.equal(predictor.gateReason(ctx, 0), "alt-screen");
  assert.equal(predictor.onKey(key("j"), ctx), "skipped");
  assert.equal(predictor.active(), false);
});

test("entering the alt screen clears an overlay in flight", () => {
  const predictor = ready();
  predictor.onKey(key("a"), context());
  predictor.onDelta(makeDelta(0, "", { alt: true }), 11, 5);
  assert.equal(predictor.active(), false);
});

test("not holding the control lease suppresses prediction", () => {
  const predictor = ready();
  const ctx = context({ hasControl: false });
  assert.equal(predictor.gateReason(ctx, 0), "no-lease");
  assert.equal(predictor.onKey(key("s"), ctx), "skipped");
  assert.equal(predictor.active(), false);
});

test("a fast link suppresses prediction", () => {
  const predictor = createPredictor({ clock: () => 0 });
  assert.equal(predictor.gateReason(context(), 0), "fast-link", "no RTT measured yet");
  predictor.noteRTT(1);
  assert.equal(predictor.gateReason(context(), 0), "fast-link", "1ms is below the floor");
  assert.equal(predictor.onKey(key("s"), context()), "skipped");
  assert.equal(predictor.active(), false);
  predictor.noteRTT(400);
  assert.equal(predictor.gateReason(context(), 0), null);
});

test("a resize invalidates the whole overlay", () => {
  const predictor = ready();
  predictor.onKey(key("a"), context());
  predictor.onKey(key("b"), context({ seq: 11 }));
  assert.equal(predictor.size(), 2);
  assert.equal(predictor.onResize(5), true);
  assert.equal(predictor.active(), false);
  assert.equal(predictor.cursorPosition(), null);
});

test("a resized or full delta invalidates the overlay", () => {
  const predictor = ready();
  predictor.onKey(key("a"), context());
  predictor.onDelta(makeDelta(0, "a", { cols: 40 }), 11, 5);
  assert.equal(predictor.active(), false, "different geometry means different coordinates");

  const other = ready();
  other.onKey(key("a"), context());
  other.onDelta(makeDelta(0, "a", { full: true }), 11, 5);
  assert.equal(other.active(), false);
});

test("a non-predictable key flushes the overlay", () => {
  const predictor = ready();
  predictor.onKey(key("l"), context());
  predictor.onKey(key("s"), context({ seq: 11 }));
  assert.equal(predictor.size(), 2);
  assert.equal(predictor.onKey(key("Enter"), context({ seq: 12 })), "flushed");
  assert.equal(predictor.active(), false);
  assert.equal(predictor.cursorPosition(), null);
  assert.equal(predictor.stats().mismatched, 0, "a flush is not a mispredict");
});

test("arrow keys and control characters flush rather than predict", () => {
  for (const pressed of [key("ArrowLeft"), key("c", { ctrlKey: true }), key("Tab"), key("Escape")]) {
    const predictor = ready();
    predictor.onKey(key("a"), context());
    assert.equal(predictor.onKey(pressed, context({ seq: 11 })), "flushed");
    assert.equal(predictor.active(), false);
  }
});

test("a paste flushes the overlay and is never predicted", () => {
  const predictor = ready();
  predictor.onKey(key("a"), context());
  assert.equal(predictor.onUnpredictableInput(5), true);
  assert.equal(predictor.active(), false);
});

test("Backspace erases predictions we made ourselves", () => {
  const predictor = ready();
  predictor.onKey(key("l"), context());
  predictor.onKey(key("s"), context({ seq: 11 }));
  assert.equal(predictor.onKey(key("Backspace"), context({ seq: 12 })), "predicted");
  assert.equal(overlayText(predictor, 0), "l_", "the erased cell is predicted blank");
  assert.deepEqual(predictor.cursorPosition(), { x: 1, y: 0 });

  // The server may echo the character before it echoes the erase; that is a
  // legitimate intermediate state, not a mispredict.
  predictor.onDelta(makeDelta(0, "ls"), 13, 20);
  assert.equal(predictor.stats().mismatched, 0);
  predictor.onDelta(makeDelta(0, "l"), 14, 25);
  assert.equal(predictor.active(), false);
});

test("Backspace past the anchor flushes instead of rubbing out the prompt", () => {
  const predictor = ready();
  predictor.onKey(key("l"), context());
  assert.equal(predictor.onKey(key("Backspace"), context({ seq: 11 })), "predicted");
  assert.equal(predictor.onKey(key("Backspace"), context({ seq: 12 })), "flushed");
  assert.equal(predictor.active(), false);
});

test("Backspace with nothing predicted is never speculated on", () => {
  const predictor = ready();
  const frame = makeFrame({ cursor: { X: 5, Y: 0 } });
  assert.equal(predictor.onKey(key("Backspace"), context({ frame })), "skipped");
  assert.equal(predictor.active(), false);
});

test("typing mid-line is refused because the shell would insert and shift", () => {
  const lines = Array.from({ length: ROWS }, () => blankRow());
  lines[0][6] = cell("t");
  lines[0][7] = cell("l");
  const frame = makeFrame({ cursor: { X: 4, Y: 0 }, lines });
  const predictor = ready();
  assert.equal(predictor.onKey(key("x"), context({ frame })), "skipped");
  assert.equal(predictor.active(), false);
});

test("typing at the end of existing text is predicted", () => {
  const lines = Array.from({ length: ROWS }, () => blankRow());
  [..."$ "].forEach((character, index) => { lines[0][index] = cell(character); });
  const frame = makeFrame({ cursor: { X: 2, Y: 0 }, lines });
  const predictor = ready();
  assert.equal(predictor.onKey(key("x"), context({ frame })), "predicted");
  assert.equal(predictor.cellAt(2, 0).text, "x");
});

test("the last column is refused so wrap semantics are never guessed", () => {
  const frame = makeFrame({ cursor: { X: COLS - 1, Y: 0 } });
  const predictor = ready();
  assert.equal(predictor.onKey(key("x"), context({ frame })), "skipped");
  assert.equal(predictor.active(), false);
});

test("a poor confirmation rate disables prediction for a cooldown", () => {
  const predictor = ready({ rtt: 200, minSamplesForRate: 3, historySize: 8, confirmationFloor: 0.75, cooldownMs: 5000 });
  let clock = 0;
  for (let round = 0; round < 3; round++) {
    predictor.onKey(key("a"), context({ seq: 10 + round * 2, now: clock }));
    predictor.onDelta(makeDelta(0, "X"), 11 + round * 2, clock + 10);
    clock += 100;
  }
  assert.equal(predictor.stats().cooldowns, 1);
  assert.equal(predictor.gateReason(context(), clock), "cooldown");
  assert.equal(predictor.onKey(key("a"), context({ now: clock })), "skipped");
  assert.equal(predictor.gateReason(context(), clock + 6000), null, "and it tries again afterwards");
});

test("a healthy confirmation rate never trips the cooldown", () => {
  const predictor = ready({ minSamplesForRate: 3 });
  let clock = 0;
  let text = "";
  let seq = 10;
  for (const character of "hello") {
    // The frame carries what the server has echoed so far, and its cursor sits
    // at the end of it — the ordinary shell-prompt case.
    const lines = Array.from({ length: ROWS }, () => blankRow());
    [...text].forEach((echoed, index) => { lines[0][index] = cell(echoed); });
    const frame = makeFrame({ cursor: { X: text.length, Y: 0 }, lines });
    predictor.onKey(key(character), context({ frame, seq, now: clock }));
    text += character;
    seq += 1;
    predictor.onDelta(makeDelta(0, text), seq, clock + 5);
    clock += 50;
  }
  assert.equal(predictor.stats().cooldowns, 0);
  assert.equal(predictor.stats().confirmed, 5);
  assert.equal(predictor.active(), false);
});

test("history movement invalidates the overlay because rows shift underneath it", () => {
  const predictor = ready();
  predictor.onKey(key("a"), context());
  predictor.onDelta(makeDelta(0, "a", { extra: { HistoryAppend: [blankRow()] } }), 11, 5);
  assert.equal(predictor.active(), false);
});

test("dirty rows are reported once and then cleared", () => {
  const predictor = ready();
  predictor.onKey(key("a"), context());
  assert.deepEqual(predictor.takeDirtyRows(), [0]);
  assert.deepEqual(predictor.takeDirtyRows(), []);
  predictor.onDelta(makeDelta(0, "a"), 11, 5);
  assert.deepEqual(predictor.takeDirtyRows(), [0]);
});

test("strict mode marks predictions for underlining", () => {
  const predictor = ready();
  predictor.onKey(key("a"), context());
  assert.equal(predictor.cellAt(0, 0).underline, false);
  predictor.setStrict(true);
  assert.equal(predictor.cellAt(0, 0).underline, true);
  assert.deepEqual(predictor.takeDirtyRows(), [0], "the row repaints when the style changes");
});

test("disabling the predictor clears and stops it", () => {
  const predictor = ready();
  predictor.onKey(key("a"), context());
  predictor.setEnabled(false);
  assert.equal(predictor.active(), false);
  assert.equal(predictor.gateReason(context(), 0), "disabled");
  assert.equal(predictor.onKey(key("b"), context({ seq: 11 })), "skipped");
});

test("RTT is measured from input to the next server message", () => {
  const predictor = createPredictor({ clock: () => 0 });
  assert.equal(predictor.rtt(), null);
  predictor.onSend(0);
  predictor.onDelta(makeDelta(0, ""), 11, 80);
  assert.equal(predictor.rtt(), 80, "first sample seeds the estimate");
  predictor.onSend(100);
  predictor.onDelta(makeDelta(0, ""), 12, 140);
  assert.ok(predictor.rtt() < 80 && predictor.rtt() > 40, "later samples are smoothed");
});

test("a snapshot resets every scrap of speculative state", () => {
  const predictor = ready();
  predictor.onKey(key("a"), context());
  assert.equal(predictor.onSnapshot(5), true);
  assert.equal(predictor.active(), false);
  assert.equal(predictor.cursorPosition(), null);
});
