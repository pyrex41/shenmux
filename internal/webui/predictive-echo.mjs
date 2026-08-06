// Predictive local echo — the shared state machine behind the browser
// clients' immediate typing feedback.
//
// The problem: a keystroke only reaches the screen after browser → websocket →
// gateway → ZMQ → daemon → PTY → shell echo → delta → back. Over the relay path
// that is one RTT per character. The fix is to paint the character locally,
// immediately, and reconcile it against the authoritative frame when it
// arrives.
//
// Two invariants make this safe rather than reckless:
//
//  1. Predictions are an *overlay*. This module never mutates the server frame;
//     it hands the renderer a sparse set of cells to composite on top. The
//     server's frame remains the single source of truth, so discarding the
//     overlay always restores exactly what the server said.
//
//  2. Predictions are conditional. In vim's normal mode `j` does not produce a
//     `j`, and at a password prompt nothing must appear at all. So we predict
//     only when the daemon has published `Modes.Echo === true` (it reads the
//     PTY's termios ECHO bit — a fact, not a heuristic), only outside the alt
//     screen, only while this client holds the control lease, and only when the
//     measured round trip is large enough for prediction to be worth any risk.
//
//     `Modes.Echo` is published by a parallel server-side work item. Until it
//     lands the field is absent, `!== true`, and this module is inert. That is
//     deliberate: guessing at echo state is exactly what would leak a password
//     character onto the screen.
//
// The module is a plain ES module with no browser globals at import time, so it
// is unit-testable headlessly: feed it keystrokes and synthetic deltas and
// assert the overlay.

export const DEFAULTS = {
  // Prediction is disabled below this smoothed RTT. Locally the round trip is
  // imperceptible, so prediction would buy nothing and only add mispredict
  // risk; this is what keeps the local demo path bit-for-bit unaffected.
  minRTTMs: 5,
  // A prediction unconfirmed after this multiple of the RTT is discarded. Belt
  // to the echo flag's braces: if the server never echoes, the glyph vanishes.
  timeoutFactor: 2,
  minTimeoutMs: 60,
  maxTimeoutMs: 2000,
  // Absurd samples (a shell that sat silent for a minute) must not inflate the
  // estimator into never timing anything out.
  maxRTTSampleMs: 2000,
  rttAlpha: 0.125,
  // Adaptation: a mispredicting terminal is worse than a slow one.
  historySize: 20,
  minSamplesForRate: 8,
  confirmationFloor: 0.75,
  cooldownMs: 5000,
  // mosh underlines predictions. We render them as ordinary text because
  // immediacy is the whole point; the underline stays available for debugging.
  strict: false,
  enabled: true,
};

const BLANK = "";

/** Blank cells arrive as "" or " " depending on how a row was produced. */
export function normalizeText(text) {
  if (text === undefined || text === null || text === " ") return BLANK;
  return text;
}

function frameCellText(frame, x, y) {
  return normalizeText(frame?.Lines?.[y]?.[x]?.Text);
}

/**
 * Decide whether a keydown is predictable at all.
 *
 * Printable characters and Backspace only. Never arrows, control characters or
 * escape sequences: their effect on the remote terminal is not knowable here.
 * Characters at or above U+0300 (combining marks, CJK, emoji) are refused too,
 * because their cell width and composition behaviour are not knowable either.
 */
export function classifyKey(event) {
  if (!event || typeof event.key !== "string") return { kind: "other" };
  if (event.ctrlKey || event.metaKey || event.altKey) return { kind: "other" };
  if (event.key === "Backspace") return { kind: "backspace" };
  const points = Array.from(event.key);
  if (points.length !== 1) return { kind: "other" };
  const point = event.key.codePointAt(0);
  if (point < 0x20 || point === 0x7f || point >= 0x300) return { kind: "other" };
  return { kind: "char", text: event.key };
}

/**
 * createPredictor returns the state machine. All time is supplied by the caller
 * (`ctx.now`) or by `options.clock`, so tests drive it without timers.
 *
 * Entry points:
 *   onKey(event, ctx)      a keydown that is about to be sent upstream
 *   onSend(now)            any other input sent (paste, mouse, focus)
 *   onDelta(delta, seq, now)  reconcile against the authoritative frame
 *   onResize(now)          any resize invalidates the whole overlay
 *   tick(now)              expire timed-out predictions
 *
 * Render surface:
 *   cellAt(x, y) / rowAt(y) / activeRows() / cursorPosition() / takeDirtyRows()
 *
 * ctx is `{ frame, seq, hasControl, now }`, where `frame` is the authoritative
 * screen frame, `seq` the last applied envelope seq, and `hasControl` whether
 * this client holds the control lease.
 */
export function createPredictor(options = {}) {
  const config = { ...DEFAULTS, ...options };
  const clock = config.clock
    || (() => (typeof performance !== "undefined" && performance.now ? performance.now() : Date.now()));

  /** y -> Map(x -> entry). entry = { x, y, text, seq, at, accepts:Set<string> } */
  const rows = new Map();
  const dirty = new Set();
  const outcomes = [];
  let epoch = 1;
  let cursor = null; // predicted cursor, {x, y}
  let anchorX = null; // column where this epoch began; backspace stops here
  let anchorY = null;
  let srtt = null;
  let sentAt = null; // outstanding input awaiting any server reply, for RTT
  let cooldownUntil = 0;
  let cols = null;
  let rowCount = null;
  let enabled = config.enabled !== false;
  let strict = config.strict === true;
  const counters = { predicted: 0, confirmed: 0, mismatched: 0, timedOut: 0, cooldowns: 0 };

  const now_ = (ctx) => (ctx && typeof ctx.now === "number" ? ctx.now : clock());

  function markRow(y) {
    dirty.add(y);
  }

  function size() {
    let total = 0;
    for (const row of rows.values()) total += row.size;
    return total;
  }

  function clearOverlay() {
    for (const y of rows.keys()) markRow(y);
    rows.clear();
    cursor = null;
    anchorX = null;
    anchorY = null;
    epoch += 1;
  }

  function recordOutcome(hit, now) {
    outcomes.push(hit === true);
    if (outcomes.length > config.historySize) outcomes.shift();
    if (outcomes.length < config.minSamplesForRate) return;
    const hits = outcomes.reduce((count, value) => count + (value ? 1 : 0), 0);
    if (hits / outcomes.length >= config.confirmationFloor) return;
    // Below the confirmation floor this terminal is not one we understand.
    // Stand down for a while, then try again.
    cooldownUntil = now + config.cooldownMs;
    counters.cooldowns += 1;
    outcomes.length = 0;
    clearOverlay();
  }

  function discardEpoch(now, reason) {
    if (!rows.size) return false;
    clearOverlay();
    if (reason === "mismatch") counters.mismatched += 1;
    if (reason === "timeout") counters.timedOut += 1;
    if (reason === "mismatch" || reason === "timeout") recordOutcome(false, now);
    return true;
  }

  function sampleRTT(value) {
    if (!Number.isFinite(value) || value < 0) return;
    const sample = Math.min(value, config.maxRTTSampleMs);
    srtt = srtt === null ? sample : (1 - config.rttAlpha) * srtt + config.rttAlpha * sample;
  }

  function timeoutMs() {
    const base = srtt === null ? config.minTimeoutMs : srtt * config.timeoutFactor;
    return Math.max(config.minTimeoutMs, Math.min(config.maxTimeoutMs, base));
  }

  function oldestAt() {
    let oldest = null;
    for (const row of rows.values()) {
      for (const entry of row.values()) {
        if (oldest === null || entry.at < oldest) oldest = entry.at;
      }
    }
    return oldest;
  }

  function tick(now = clock()) {
    const oldest = oldestAt();
    if (oldest === null) return false;
    if (now - oldest <= timeoutMs()) return false;
    return discardEpoch(now, "timeout");
  }

  /**
   * Every gate that must hold before a single glyph is painted speculatively.
   * Fails closed on anything unknown — including `Modes.Echo` being absent.
   */
  function gate(ctx, now) {
    if (!enabled) return "disabled";
    if (now < cooldownUntil) return "cooldown";
    if (!ctx || !ctx.hasControl) return "no-lease";
    const frame = ctx.frame;
    if (!frame || !Array.isArray(frame.Lines)) return "no-frame";
    if (frame.AltScreen) return "alt-screen";
    if (frame.Modes?.Echo !== true) return "echo-off";
    if (srtt === null || srtt < config.minRTTMs) return "fast-link";
    return null;
  }

  function noteDimensions(frame) {
    if (!frame) return false;
    const changed = (cols !== null && frame.Cols !== cols) || (rowCount !== null && frame.Rows !== rowCount);
    cols = frame.Cols;
    rowCount = frame.Rows;
    return changed;
  }

  function entryAt(x, y) {
    return rows.get(y)?.get(x);
  }

  function setEntry(entry) {
    let row = rows.get(entry.y);
    if (!row) {
      row = new Map();
      rows.set(entry.y, row);
    }
    row.set(entry.x, entry);
    markRow(entry.y);
  }

  function dropEntry(x, y) {
    const row = rows.get(y);
    if (!row) return;
    row.delete(x);
    if (!row.size) rows.delete(y);
    markRow(y);
  }

  /**
   * Predict only when typing at the end of a line: every cell from the cursor
   * to the right margin must be blank. Mid-line typing means the shell will
   * insert and shift, which the overlay cannot model, so we decline instead of
   * mispredicting.
   */
  function atLineEnd(frame, x, y) {
    for (let column = x; column < frame.Cols; column++) {
      if (frameCellText(frame, column, y) !== BLANK) return false;
      if (entryAt(column, y)) return false;
    }
    return true;
  }

  function position(ctx) {
    if (cursor) return cursor;
    const c = ctx.frame?.Cursor;
    if (!c) return null;
    return { x: c.X, y: c.Y };
  }

  /**
   * onKey is called for a keydown the client is about to send. It returns
   * "predicted", "flushed" (overlay dropped, nothing predicted) or "skipped".
   * The caller sends the keystroke either way — prediction never changes what
   * goes on the wire.
   */
  function onKey(event, ctx = {}) {
    const now = now_(ctx);
    tick(now);
    onSend(now);
    if (noteDimensions(ctx.frame)) clearOverlay();

    const classified = classifyKey(event);
    if (classified.kind === "other") {
      // Anything we cannot model — arrows, Enter, Tab, control chars — makes
      // every outstanding prediction unverifiable. Drop them and wait for truth.
      return discardEpoch(now, "flush") ? "flushed" : "skipped";
    }
    if (gate(ctx, now) !== null) {
      return discardEpoch(now, "flush") ? "flushed" : "skipped";
    }

    const frame = ctx.frame;
    const at = position(ctx);
    if (!at || at.y >= frame.Rows || at.x < 0) {
      return discardEpoch(now, "flush") ? "flushed" : "skipped";
    }
    const seq = Number(ctx.seq) || 0;

    if (classified.kind === "backspace") {
      // Backspace is predicted only back over characters we predicted
      // ourselves. Erasing beyond the anchor could rub out a prompt we do not
      // own, so it flushes instead.
      if (!rows.size || anchorX === null || at.y !== anchorY || at.x <= anchorX) {
        return discardEpoch(now, "flush") ? "flushed" : "skipped";
      }
      const x = at.x - 1;
      const existing = entryAt(x, at.y);
      if (!existing) return discardEpoch(now, "flush") ? "flushed" : "skipped";
      // The server may still echo the character we are erasing before it
      // echoes the erase itself, so that value stays acceptable-but-unconfirmed.
      const accepts = new Set(existing.accepts);
      accepts.add(existing.text);
      setEntry({ x, y: at.y, text: BLANK, seq, at: now, accepts, epoch });
      cursor = { x, y: at.y };
      counters.predicted += 1;
      return "predicted";
    }

    // Refuse the last column: what happens next is a wrap, and wrap semantics
    // (autowrap on/off, deferred wrap) are not knowable from the frame.
    if (at.x >= frame.Cols - 1) {
      return discardEpoch(now, "flush") ? "flushed" : "skipped";
    }
    if (!atLineEnd(frame, at.x, at.y)) {
      return discardEpoch(now, "flush") ? "flushed" : "skipped";
    }
    if (!rows.size) {
      anchorX = at.x;
      anchorY = at.y;
    }
    setEntry({
      x: at.x, y: at.y, text: classified.text, seq, at: now,
      accepts: new Set([frameCellText(frame, at.x, at.y)]), epoch,
    });
    cursor = { x: at.x + 1, y: at.y };
    counters.predicted += 1;
    return "predicted";
  }

  /**
   * onSend records that input went upstream. The gap to the next server message
   * is the round trip we gate on, which is how RTT is measured without adding a
   * protocol message.
   */
  function onSend(now = clock()) {
    if (sentAt === null) sentAt = now;
  }

  /** Any input we cannot model at all (a paste) invalidates the overlay. */
  function onUnpredictableInput(now = clock()) {
    onSend(now);
    return discardEpoch(now, "flush");
  }

  function onResize(now = clock()) {
    const had = rows.size > 0;
    clearOverlay();
    sentAt = null;
    return had;
  }

  /** A fresh snapshot (attach or resync) invalidates everything. */
  function onSnapshot(now = clock()) {
    const had = rows.size > 0;
    clearOverlay();
    outcomes.length = 0;
    sentAt = null;
    cols = null;
    rowCount = null;
    return had;
  }

  /**
   * onDelta reconciles the overlay against authoritative state.
   *
   * For every prediction the delta's seq outranks, on a row the delta touched:
   *   the predicted glyph      → confirm and drop it from the overlay
   *   a value we already knew  → the echo has not arrived yet; keep waiting
   *   anything else            → mispredict: discard the whole epoch and let
   *                              the server's frame stand
   */
  function onDelta(delta, seq, now = clock()) {
    if (!delta) return false;
    if (sentAt !== null) {
      sampleRTT(now - sentAt);
      sentAt = null;
    }
    let changed = false;
    if (delta.Full || (cols !== null && (delta.Cols !== cols || delta.Rows !== rowCount))) {
      changed = rows.size > 0;
      clearOverlay();
    }
    cols = delta.Cols;
    rowCount = delta.Rows;
    if (delta.AltScreen || delta.Modes?.Echo !== true) {
      // The terminal stopped being one we may predict for. Everything
      // outstanding is now unverifiable.
      if (rows.size) changed = true;
      clearOverlay();
    }
    if (delta.HistoryReset || delta.HistoryDrop || delta.HistoryAppend?.length) {
      // Rows moved under the overlay; its coordinates no longer mean anything.
      if (rows.size) changed = true;
      clearOverlay();
    }
    if (!rows.size) {
      changed = tick(now) || changed;
      return changed;
    }

    for (const line of (delta.Lines || [])) {
      const row = rows.get(line.Y);
      if (!row) continue;
      for (const entry of [...row.values()]) {
        if (seq !== undefined && seq !== null && Number(seq) <= entry.seq) continue;
        const actual = normalizeText(line.Cells?.[entry.x]?.Text);
        if (actual === entry.text) {
          sampleRTT(now - entry.at);
          dropEntry(entry.x, entry.y);
          counters.confirmed += 1;
          recordOutcome(true, now);
          changed = true;
          continue;
        }
        if (entry.accepts.has(actual)) continue;
        discardEpoch(now, "mismatch");
        return true;
      }
    }
    if (!rows.size) {
      cursor = null;
      anchorX = null;
      anchorY = null;
    }
    changed = tick(now) || changed;
    return changed;
  }

  return {
    onKey,
    onSend,
    onUnpredictableInput,
    onDelta,
    onResize,
    onSnapshot,
    tick,
    flush(now = clock()) {
      return discardEpoch(now, "flush");
    },
    /** Overlay lookup for the renderer. Null means "use the server cell". */
    cellAt(x, y) {
      const entry = entryAt(x, y);
      if (!entry) return null;
      return { text: entry.text, underline: strict };
    },
    rowAt(y) {
      return rows.get(y) || null;
    },
    activeRows() {
      return [...rows.keys()];
    },
    /** Rows whose overlay changed since the last call, for dirty-row tracking. */
    takeDirtyRows() {
      const list = [...dirty];
      dirty.clear();
      return list;
    },
    cursorPosition() {
      return cursor ? { x: cursor.x, y: cursor.y } : null;
    },
    active() {
      return rows.size > 0;
    },
    size,
    /** When the oldest outstanding prediction expires, or null. */
    nextDeadline() {
      const oldest = oldestAt();
      return oldest === null ? null : oldest + timeoutMs();
    },
    rtt() {
      return srtt;
    },
    /** Test and debug seam: force an RTT estimate without a live link. */
    noteRTT(sample) {
      sampleRTT(sample);
    },
    setEnabled(value) {
      enabled = value !== false;
      if (!enabled) clearOverlay();
    },
    setStrict(value) {
      strict = value === true;
      for (const y of rows.keys()) markRow(y);
    },
    strict() {
      return strict;
    },
    gateReason(ctx = {}, now = clock()) {
      return gate(ctx, now);
    },
    stats() {
      return { ...counters, srtt, epoch, size: size(), cooldownUntil, timeout: timeoutMs() };
    },
  };
}

export default createPredictor;
