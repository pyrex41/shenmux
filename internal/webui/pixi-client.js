import { Application, BitmapFont, BitmapText, Container, Graphics } from "pixi.js";
import Shen from "shen-script";
import { instanceEndpoint, pageIdentity, refusalVerdict, socketURL } from "./gateway.mjs";
import { fitGrid, renderScale } from "./fit.mjs";

(() => {
  "use strict";

  const FONT = "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace";
  const FONT_NAMES = { regular: "ShenmuxMono", bold: "ShenmuxMonoBold", italic: "ShenmuxMonoItalic", boldItalic: "ShenmuxMonoBoldItalic" };
  const FONT_SIZE = 15;
  const LINE_HEIGHT = 19;
  // Long enough that a window drag or an orientation change settles before the
  // PTY is asked to resize, short enough that the grid follows the window.
  const FIT_DEBOUNCE_MS = 120;
  // Identity stamped into this page by the gateway that served it. A tab left
  // open from a previous `shenmux web` process carries the previous instance
  // id and is refused rather than silently taking over the new session.
  const { instance: PAGE_INSTANCE, token: PAGE_TOKEN } = pageIdentity(document);
  const wrap = document.querySelector("#screen-wrap");
  const host = document.querySelector("#pixi-host");
  // A canvas cannot summon a touch keyboard. This field can, and it is where a
  // phone composes a line before the terminal sees any of it.
  const compose = document.querySelector("#compose");
  const softKeys = document.querySelector("#soft-keys");
  const empty = document.querySelector("#empty");
  const status = document.querySelector("#status");
  const sessionLabel = document.querySelector("#session");
  const details = document.querySelector("#details");
  const acquireButton = document.querySelector("#acquire");
  const releaseButton = document.querySelector("#release");

  let app;
  let shen;
  let socket;
  let state;
  let clientID = "";
  let lastSeq = 0;
  let cellW = 9;
  let lineH = LINE_HEIGHT;
  let background;
  let textLayer;
  let cursorLayer;
  let rowViews = [];
  let dirtyRows = new Set();
  let commandQueue = Promise.resolve();
  let renderQueued = false;
  let scrollPixels = 0;
  let rendererWidth = 0;
  let rendererHeight = 0;
  // The box the grid has to live inside, in CSS pixels, and the factor the
  // canvas is presented at so that it does. Measured on resize rather than
  // per frame: reading layout during a render is what makes a paint expensive.
  let usableWidth = 0;
  let usableHeight = 0;
  let presentedScale = 0;
  let fitTimer;
  // The last grid this client asked the PTY for. Without it a resize is
  // re-sent on every tick until the round trip lands.
  let requestedGrid = "";
  // Reconnects always begin with a fresh checkpoint. Keep the previous view
  // visible while offline, but never apply deltas from the old stream to it.
  let reconnectTimer;
  let reconnectDelay = 250;
  // Set when the gateway will never accept this page again. Retrying then is
  // noise, so the page stops and says what to do instead.
  let blocked = false;

  const setStatus = (text, good = false) => {
    status.textContent = text;
    status.style.color = good ? "#65e6a7" : "#748294";
  };

  function blockPage(message) {
    blocked = true;
    if (reconnectTimer) {
      clearTimeout(reconnectTimer);
      reconnectTimer = undefined;
    }
    setStatus(message);
    empty.hidden = false;
    empty.textContent = message;
    if (socket) {
      socket.onclose = null;
      socket.onerror = null;
      try { socket.close(); } catch (_) { /* already closing */ }
      socket = undefined;
    }
    acquireButton.disabled = true;
    releaseButton.disabled = true;
  }

  // Asks the gateway who it is. A browser cannot see the HTTP status of a
  // failed websocket handshake, so the reason for a refusal is read back here.
  async function gatewayIdentity() {
    try {
      const response = await fetch(instanceEndpoint(PAGE_TOKEN), { cache: "no-store" });
      if (!response.ok) return null;
      return await response.json();
    } catch (_) {
      return null; // the gateway is down; that is a retry, not a refusal
    }
  }
  const color = (value, fallback) => value && value.Valid
    ? ((value.R << 16) | (value.G << 8) | value.B) : fallback;
  const send = (message) => {
    if (socket && socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify(message));
  };
  const refreshControlButtons = () => {
    const ownsControl = Boolean(state && clientID && state.control_owner === clientID);
    acquireButton.disabled = ownsControl;
    releaseButton.disabled = !ownsControl;
    acquireButton.textContent = ownsControl ? "control held" : "take control";
  };
  const blankRow = (cols) => Array.from({ length: cols }, () => ({ Text: "", Width: 1, Style: {} }));
  const blankFrame = (cols, rows) => ({
    Cols: cols, Rows: rows,
    Lines: Array.from({ length: rows }, () => blankRow(cols)),
    Cursor: { X: 0, Y: 0, Visible: true, Blink: true, Style: 0 },
    DefaultFG: {}, DefaultBG: {},
  });

  function applyDelta(delta) {
    if (!state || !delta) return;
    let frame = state.screen.Frame;
    const oldCursorY = frame.Cursor?.Y;
    if (delta.Full || frame.Cols !== delta.Cols || frame.Rows !== delta.Rows) {
      frame = blankFrame(delta.Cols, delta.Rows);
      state.screen.Frame = frame;
      markAllRows();
      // Whatever this client last asked for, the terminal has since been some
      // other shape — possibly at another client's request. Holding on to the
      // old request would make the next fit believe it had already been sent.
      requestedGrid = "";
    }
    for (const line of (delta.Lines || [])) {
      frame.Lines[line.Y] = line.Cells;
      dirtyRows.add(line.Y);
    }
    frame.Cursor = delta.Cursor;
    frame.AltScreen = delta.AltScreen;
    frame.Title = delta.Title;
    frame.WorkingDirectory = delta.WorkingDirectory;
    frame.DefaultFG = delta.DefaultFG;
    frame.DefaultBG = delta.DefaultBG;
    frame.Modes = delta.Modes || {};
    if (delta.HistoryReset) state.screen.History = [];
    if (delta.HistoryDrop) state.screen.History = state.screen.History.slice(delta.HistoryDrop);
    if (delta.HistoryAppend) state.screen.History.push(...delta.HistoryAppend);
    if (delta.HistoryReset || delta.HistoryDrop || delta.HistoryAppend?.length) markAllRows();
    if (oldCursorY !== undefined) dirtyRows.add(oldCursorY);
    dirtyRows.add(frame.Cursor.Y);
  }

  function setupGrid(cols, rows) {
    if (rowViews.length === rows && rowViews[0]?.cols === cols) return;
    textLayer.removeChildren().forEach((view) => view.destroy({ children: true }));
    rowViews = Array.from({ length: rows }, () => {
      const container = new Container();
      const background = new Graphics();
      const text = new Container();
      container.addChild(background, text);
      textLayer.addChild(container);
      return { cols, container, background, text, runs: [] };
    });
    markAllRows();
  }

  function markAllRows() {
    for (let y = 0; y < rowViews.length; y++) dirtyRows.add(y);
  }

  function fontFor(style) {
    if (style.Bold && style.Italic) return FONT_NAMES.boldItalic;
    if (style.Bold) return FONT_NAMES.bold;
    if (style.Italic) return FONT_NAMES.italic;
    return FONT_NAMES.regular;
  }

  function renderRow(view, row, defaultFG, defaultBG, y) {
    view.container.y = y;
    view.background.clear();
    let backgroundColor = null;
    let backgroundStart = 0;
    let backgroundWidth = 0;
    const flushBackground = () => {
      if (backgroundColor !== null && backgroundWidth > 0) {
        view.background.rect(backgroundStart * cellW, 0, backgroundWidth * cellW, lineH).fill(backgroundColor);
      }
      backgroundColor = null;
      backgroundWidth = 0;
    };
    const runs = [];
    let runText = "";
    let runStart = 0;
    let runX = 0;
    let runKey = "";
    let runFG = defaultFG;
    let runStyle = {};
    const flushRun = () => {
      if (!runText) return;
      runs.push({ text: runText, x: runX, fg: runFG, style: runStyle });
      runText = "";
    };
    for (let x = 0; x < view.cols; x++) {
      const cell = row?.[x] || { Text: "", Width: 1, Style: {} };
      const style = cell.Style || {};
      let fg = color(style.FG, defaultFG);
      let bg = color(style.BG, defaultBG);
      if (style.Inverse) [fg, bg] = [bg, fg];
      const width = cell.Width === 2 ? 2 : 1;
      const visible = !style.Invisible;
      const bgKey = visible && bg !== defaultBG ? bg : null;
      if (bgKey !== backgroundColor) {
        flushBackground();
        backgroundColor = bgKey;
        backgroundStart = x;
      }
      if (bgKey !== null) backgroundWidth += width;
      const key = `${fontFor(style)}:${fg}:${visible ? 1 : 0}:${style.Faint ? 1 : 0}`;
      if (key !== runKey) {
        flushRun();
        runKey = key;
        runStart = x;
        runX = x * cellW;
        runFG = fg;
        runStyle = style;
      }
      runText += visible && cell.Width !== 0 ? (cell.Text || " ") : " ";
    }
    flushBackground();
    flushRun();

    for (let i = 0; i < runs.length; i++) {
      const spec = runs[i];
      let text = view.runs[i];
      if (!text) {
        text = new BitmapText({ text: " ", style: { fontFamily: fontFor(spec.style), fontSize: FONT_SIZE } });
        view.text.addChild(text);
        view.runs[i] = text;
      }
      text.style.fontFamily = fontFor(spec.style);
      text.text = spec.text;
      text.tint = spec.fg;
      text.alpha = spec.style.Faint ? .65 : 1;
      text.visible = spec.style.Invisible !== true;
      text.position.set(spec.x, 1);
    }
    for (let i = runs.length; i < view.runs.length; i++) view.runs[i].visible = false;
  }

  // The content box of the terminal pane. clientWidth still counts the
  // padding, and counting columns against the padded width is what drew the
  // grid past the window edge; it also excludes any scrollbar, which is the
  // conservative direction to be wrong in.
  function measureUsable() {
    const style = getComputedStyle(wrap);
    const horizontal = parseFloat(style.paddingLeft) + parseFloat(style.paddingRight);
    const vertical = parseFloat(style.paddingTop) + parseFloat(style.paddingBottom);
    usableWidth = Math.max(0, wrap.clientWidth - horizontal);
    usableHeight = Math.max(0, wrap.clientHeight - vertical);
  }

  // The grid is rendered at its natural size and presented at whatever
  // fraction of it the window can hold. Scaling in CSS rather than in the
  // scene keeps the backing store at full device resolution, so a shrunk
  // terminal resamples a sharp image instead of drawing small glyphs.
  function presentGrid(cols, rows) {
    if (usableWidth <= 0 || usableHeight <= 0) measureUsable();
    const width = cols * cellW;
    const height = rows * lineH;
    const scale = renderScale({ width: usableWidth, height: usableHeight, cols, rows, cellWidth: cellW, lineHeight: lineH });
    if (width === rendererWidth && height === rendererHeight && scale === presentedScale) return;
    if (width !== rendererWidth || height !== rendererHeight) {
      rendererWidth = width;
      rendererHeight = height;
      app.renderer.resize(width, height);
    }
    presentedScale = scale;
    app.canvas.style.width = `${width * scale}px`;
    app.canvas.style.height = `${height * scale}px`;
    markAllRows(); // a resized renderer comes back cleared
  }

  function renderFrame() {
    if (blocked) return; // keep the refusal on screen instead of a dead frame
    const frame = state?.screen?.Frame;
    if (!frame?.Lines) { empty.hidden = false; return; }
    empty.hidden = true;
    setupGrid(frame.Cols, frame.Rows);
    const width = frame.Cols * cellW;
    const height = frame.Rows * lineH;
    presentGrid(frame.Cols, frame.Rows);
    if (!dirtyRows.size) return;
    const defaultBG = color(frame.DefaultBG, 0x080b10);
    const defaultFG = color(frame.DefaultFG, 0xd7e0ea);
    background.clear().rect(0, 0, width, height).fill(defaultBG);
    const history = state.screen.History || [];
    const totalRows = history.length + frame.Rows;
    const maxScroll = history.length * lineH;
    scrollPixels = Math.max(0, Math.min(scrollPixels, maxScroll));
    const start = Math.max(0, totalRows - frame.Rows - scrollPixels / lineH);
    const firstRow = Math.floor(start);
    const rowOffset = (firstRow - start) * lineH;
    for (let y = 0; y < frame.Rows; y++) {
      const absoluteRow = firstRow + y;
      const row = absoluteRow < history.length ? history[absoluteRow] : frame.Lines[absoluteRow - history.length];
      if (dirtyRows.has(y) || scrollPixels !== 0) renderRow(rowViews[y], row, defaultFG, defaultBG, y * lineH + rowOffset);
    }
    cursorLayer.clear();
    const cursor = frame.Cursor;
    if (scrollPixels === 0 && cursor?.Visible && cursor.X < frame.Cols && cursor.Y < frame.Rows) {
      const cursorColor = color(frame.DefaultFG, 0xd7e0ea);
      const x = cursor.X * cellW;
      const y = cursor.Y * lineH;
      if (cursor.Style === 1) cursorLayer.rect(x, y, 2, lineH).fill({ color: cursorColor, alpha: .72 });
      else if (cursor.Style === 2) cursorLayer.rect(x, y + lineH - 2, cellW, 2).fill({ color: cursorColor, alpha: .72 });
      else cursorLayer.rect(x, y, cellW, lineH).fill({ color: cursorColor, alpha: .72 });
    }
    details.textContent = `${frame.Cols}×${frame.Rows} · seq ${lastSeq}${scrollPixels ? ` · ↑ ${Math.ceil(scrollPixels / lineH)} rows` : ""}${frame.Title ? ` · ${frame.Title}` : ""}`;
    dirtyRows.clear();
    // The first snapshot can arrive before Pixi's ticker has produced its
    // first frame. Render synchronously so a newly attached browser never
    // shows an apparently empty terminal.
    app.render();
  }

  // Publications can arrive in bursts while a command is producing output.
  // Let the browser coalesce those mutations into the next paint instead of
  // rebuilding the Pixi scene once per WebSocket message.
  function scheduleRender() {
    if (renderQueued) return;
    renderQueued = true;
    requestAnimationFrame(() => {
      renderQueued = false;
      renderFrame();
    });
  }

  async function classifyCommand(kind) {
    if (!shen) return kind;
    try {
      const result = await shen.exec(`(mux-command "${kind}")`);
      return String(result || kind);
    } catch (_) {
      return kind;
    }
  }
  function sendCommand(kind, payload = {}) {
    // Input is latency-sensitive. It is already a typed command and does not
    // need policy evaluation; awaiting ShenScript here makes every keypress
    // pay an interpreter round trip and serializes fast typing.
    if (kind === "input") {
      send({ type: "input", ...payload });
      return;
    }
    commandQueue = commandQueue.then(async () => {
      const command = await classifyCommand(kind);
      send({ type: command, ...payload });
    });
  }

  function onMessage(message) {
    if (message.type === "snapshot") {
      clientID = message.client_id || "";
      state = { seq: message.current.seq, screen: message.current.screen, control_owner: message.current.control_owner || "" };
      lastSeq = message.current.seq || 0;
      sessionLabel.textContent = message.session || "session";
      setStatus(state.control_owner === clientID ? "connected · control" : "connected", true);
      refreshControlButtons();
      scheduleRender();
      applyFit();
      return;
    }
    if (message.type === "delta" || message.type === "control" || message.type === "exit") {
      if (message.seq !== lastSeq + 1) { send({ type: "resync" }); return; }
      if (message.type === "delta") applyDelta(message.delta);
      if (message.type === "control") state.control_owner = message.control_owner || "";
      if (message.type === "exit") setStatus(`exited (${message.exit_code})`);
      lastSeq = message.seq;
      refreshControlButtons();
      scheduleRender();
      // Taking the lease is the moment this client becomes allowed to shape
      // the PTY, and the window it is looking at may not be the shape the
      // previous holder left behind.
      if (message.type === "control") scheduleFit();
      return;
    }
    if (message.type === "command") {
      if (state && message.meta) state.control_owner = message.meta.control_owner || "";
      setStatus(state?.control_owner === clientID ? "connected · control" : "connected", true);
      refreshControlButtons();
      scheduleFit();
      return;
    }
    if (message.type === "error") setStatus(`error · ${message.error || "server error"}`);
  }

  function keyData(event) {
    if (event.ctrlKey && event.key.length === 1) return String.fromCharCode(event.key.toUpperCase().charCodeAt(0) & 31);
    if (event.key === "Enter") return "\r";
    if (event.key === "Backspace") return "\x7f";
    if (event.key === "Tab") return event.shiftKey ? "\x1b[Z" : "\t";
    if (event.key === "Escape") return "\x1b";
    const arrows = { ArrowUp: "\x1b[A", ArrowDown: "\x1b[B", ArrowRight: "\x1b[C", ArrowLeft: "\x1b[D", Home: "\x1b[H", End: "\x1b[F", Delete: "\x1b[3~", PageUp: "\x1b[5~", PageDown: "\x1b[6~" };
    return arrows[event.key] || (event.key.length === 1 && !event.metaKey ? event.key : "");
  }

  function terminalModes() {
    return state?.screen?.Frame?.Modes || {};
  }

  function mousePoint(event) {
    const frame = state?.screen?.Frame;
    const rect = app.canvas.getBoundingClientRect();
    // The canvas is presented at presentedScale, so a CSS pixel is worth that
    // much less of a cell. Reporting unscaled cells would put the click
    // somewhere to the right of where the user pressed.
    const scale = presentedScale || 1;
    return {
      x: Math.max(1, Math.min(frame.Cols, Math.floor((event.clientX - rect.left) / (cellW * scale)) + 1)),
      y: Math.max(1, Math.min(frame.Rows, Math.floor((event.clientY - rect.top) / (lineH * scale)) + 1)),
    };
  }

  function mouseModifiers(event) {
    return (event.shiftKey ? 4 : 0) | (event.altKey ? 8 : 0) | (event.ctrlKey ? 16 : 0);
  }

  function sendMouse(event, button, release = false, wheel = false) {
    const modes = terminalModes();
    if (!modes.Mouse) return false;
    const point = mousePoint(event);
    let code = mouseModifiers(event) + button;
    if (wheel) code = mouseModifiers(event) + (event.deltaY < 0 ? 64 : 65);
    if (modes.MouseSGR) {
      sendCommand("input", { data: `\x1b[<${code};${point.x};${point.y}${release ? "m" : "M"}` });
    } else {
      sendCommand("input", { data: `\x1b[M${String.fromCharCode(32 + code, 32 + point.x, 32 + point.y)}` });
    }
    return true;
  }

  const holdsControl = () => Boolean(state && clientID && state.control_owner === clientID);

  // Two things happen here, and only one of them is anybody else's business.
  // Every client re-presents its own canvas to fit its own window. Only the
  // client holding the input lease asks the PTY to become that shape: resize
  // carries the same permission as input (mux.accept-resize?), and an observer
  // that sent one would either be rejected or, worse, reshape the terminal
  // under the person actually working in it.
  function applyFit() {
    fitTimer = undefined;
    measureUsable();
    if (!state) return;
    const frame = state.screen.Frame;
    if (!holdsControl()) {
      // Not this client's terminal to shape. Forget any request made while it
      // was, so regaining the lease asks again rather than assuming.
      requestedGrid = "";
    } else {
      const { cols, rows } = fitGrid({ width: usableWidth, height: usableHeight, cellWidth: cellW, lineHeight: lineH });
      const grid = `${cols}x${rows}`;
      if (cols === frame.Cols && rows === frame.Rows) requestedGrid = "";
      else if (grid !== requestedGrid) {
        requestedGrid = grid;
        sendCommand("resize", { cols, rows });
      }
    }
    scheduleRender();
  }

  function scheduleFit() {
    if (fitTimer) clearTimeout(fitTimer);
    fitTimer = setTimeout(applyFit, FIT_DEBOUNCE_MS);
  }

  // A canvas takes focus without raising a touch keyboard, so where the
  // pointer is coarse the compose field takes it instead.
  const touchInput = () => Boolean(window.matchMedia?.("(pointer: coarse)").matches);
  function focusTerminal() {
    const target = compose.hidden ? app.canvas : softKeys;
    target.focus({ preventScroll: true });
  }

  // An on-screen keyboard shrinks the visual viewport without touching the
  // layout viewport, so a page sized in vh keeps its bottom half underneath
  // the keyboard. Publishing the visible height lets the grid live above it.
  // Pinch zoom moves the same numbers and means nothing of the sort, so it is
  // left to the layout viewport.
  function trackVisualViewport() {
    const viewport = window.visualViewport;
    if (!viewport) return;
    const zoomed = Math.abs(viewport.scale - 1) > 0.01;
    document.documentElement.style.setProperty("--app-height", zoomed ? "100dvh" : `${Math.round(viewport.height)}px`);
  }

  async function start() {
    app = new Application();
    // Rendering is explicitly coalesced through scheduleRender(). Disable
    // Pixi's continuous ticker so each publication produces at most one
    // render, rather than competing with the ticker's automatic render loop.
    await app.init({ background: "#080b10", antialias: false, preference: ["webgl", "canvas"], resolution: window.devicePixelRatio || 1, autoDensity: true, autoStart: false });
    app.canvas.tabIndex = 0;
    app.canvas.setAttribute("aria-label", "shenmux terminal");
    host.replaceChildren(app.canvas);
    background = new Graphics();
    textLayer = new Container();
    cursorLayer = new Graphics();
    app.stage.addChild(background, textLayer, cursorLayer);
    const chars = String.fromCharCode(...Array.from({ length: 95 }, (_, index) => index + 32)) + "❯λ…─│┌┐└┘●✦╭╮╰╯";
    for (const [name, weight, italic] of [[FONT_NAMES.regular, "400", false], [FONT_NAMES.bold, "700", false], [FONT_NAMES.italic, "400", true], [FONT_NAMES.boldItalic, "700", true]]) {
      BitmapFont.install({
        name,
        chars,
        style: { fontFamily: FONT, fontSize: FONT_SIZE, fontWeight: weight, fontStyle: italic ? "italic" : "normal", fill: "#ffffff" },
        dynamicFill: true,
        resolution: window.devicePixelRatio || 1,
        padding: 0,
        skipKerning: true,
        textureStyle: { scaleMode: "nearest" },
      });
    }
    const sample = new BitmapText({ text: "M", style: { fontFamily: FONT_NAMES.regular, fontSize: FONT_SIZE } });
    // Keep the terminal grid comfortably readable even when a platform's
    // font metrics report a narrow bitmap glyph advance.
    cellW = Math.max(9, Math.ceil(sample.width));
    sample.destroy();

    try {
      shen = await new Shen();
      await shen.exec(`
        (define (mux-command kind)
          (cond (= kind "acquire") "acquire"
                (= kind "release") "release"
                (= kind "resize") "resize"
                (= kind "input") "input"
                (= kind "resync") "resync"
                true kind))
      `);
    } catch (_) {
      shen = null;
    }

    const jumpToLive = () => { if (scrollPixels !== 0) { scrollPixels = 0; markAllRows(); scheduleRender(); } };
    const sendText = (data) => {
      jumpToLive();
      // Bracketed paste is what tells a program that several lines arrived as
      // one act rather than as a person pressing return between them.
      const multiline = data.includes("\n") && terminalModes().BracketedPaste;
      sendCommand("input", { data: multiline ? `\x1b[200~${data}\x1b[201~` : data });
    };
    app.canvas.addEventListener("keydown", (event) => {
      const data = keyData(event);
      if (!data) return;
      event.preventDefault();
      jumpToLive();
      sendCommand("input", { data });
    });
    app.canvas.addEventListener("paste", (event) => {
      event.preventDefault();
      sendText(event.clipboardData.getData("text/plain"));
    });

    // The compose field, and the reason it is a buffer rather than a wire.
    // Dictation does not type: it inserts a guess and then rewrites it as
    // later words arrive, so streaming what the field holds would spell each
    // revision out at the prompt — visible, ugly, and enough to set a coding
    // agent running on half a sentence. Nothing leaves here until the person
    // says it is finished.
    compose.hidden = !touchInput();
    // Dictated prose is longer than a shell line and worth seeing all of.
    // Growing the field also shrinks the terminal pane, which the fit picks up
    // and turns into fewer rows rather than a hidden prompt.
    const resizeCompose = () => {
      softKeys.style.height = "auto";
      softKeys.style.height = `${softKeys.scrollHeight}px`;
    };
    softKeys.addEventListener("input", resizeCompose);
    const submitCompose = () => {
      const text = softKeys.value;
      softKeys.value = "";
      resizeCompose();
      // An empty send is the return key, which is how a phone answers a
      // prompt that is waiting for one.
      sendText(`${text}\r`);
      softKeys.focus({ preventScroll: true });
    };
    // Keys that steer a program rather than write into a sentence. A physical
    // keyboard attached to a tablet still needs to interrupt, escape and walk
    // the shell's history while the compose field holds focus.
    const STEERING_KEYS = new Set(["Escape", "Tab", "ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight", "PageUp", "PageDown", "Home", "End"]);
    compose.addEventListener("submit", (event) => {
      event.preventDefault();
      submitCompose();
    });
    softKeys.addEventListener("keydown", (event) => {
      // Mid-composition keydowns describe the IME's own editing, not input.
      if (event.isComposing || event.keyCode === 229) return;
      if (event.key === "Enter" && !event.shiftKey) {
        event.preventDefault();
        submitCompose();
        return;
      }
      if (!event.ctrlKey && !event.altKey && !STEERING_KEYS.has(event.key)) return;
      const data = keyData(event);
      if (!data) return;
      event.preventDefault();
      jumpToLive();
      sendCommand("input", { data });
    });
    app.canvas.addEventListener("pointerdown", (event) => {
      if (!terminalModes().Mouse) return;
      event.preventDefault();
      app.canvas.setPointerCapture?.(event.pointerId);
      focusTerminal();
      sendMouse(event, event.button);
    });
    app.canvas.addEventListener("pointerup", (event) => {
      if (!terminalModes().Mouse) return;
      event.preventDefault();
      sendMouse(event, event.button, true);
      app.canvas.releasePointerCapture?.(event.pointerId);
    });
    app.canvas.addEventListener("pointermove", (event) => {
      const mode = terminalModes().Mouse;
      if (!mode || (mode !== 3 && event.buttons === 0)) return;
      event.preventDefault();
      sendMouse(event, event.buttons ? Math.max(0, Math.log2(event.buttons) | 0) : 3);
    });
    for (const target of [app.canvas, softKeys]) {
      target.addEventListener("focus", () => {
        if (terminalModes().FocusEvents) sendCommand("input", { data: "\x1b[I" });
      });
      target.addEventListener("blur", () => {
        if (terminalModes().FocusEvents) sendCommand("input", { data: "\x1b[O" });
      });
    }
    // Anywhere in the pane, not just on the glyphs: on a phone this tap is the
    // only way to raise the keyboard, and the grid rarely fills the pane.
    wrap.addEventListener("click", focusTerminal);
    acquireButton.addEventListener("click", () => sendCommand("acquire"));
    releaseButton.addEventListener("click", () => sendCommand("release"));
    refreshControlButtons();
    // Three sources, one debounced answer: the pane's own layout, the window
    // (orientation changes arrive here), and the visual viewport, which is the
    // only one that moves when a phone opens its keyboard.
    new ResizeObserver(scheduleFit).observe(wrap);
    window.addEventListener("resize", scheduleFit);
    window.addEventListener("orientationchange", scheduleFit);
    if (window.visualViewport) {
      window.visualViewport.addEventListener("resize", () => {
        trackVisualViewport();
        scheduleFit();
      });
    }
    trackVisualViewport();
    wrap.addEventListener("wheel", (event) => {
      if (sendMouse(event, 0, false, true)) {
        event.preventDefault();
        return;
      }
      if (state?.screen?.Frame?.AltScreen) return;
      const historyRows = state?.screen?.History?.length || 0;
      if (!historyRows) return;
      scrollPixels = Math.max(0, Math.min(historyRows * lineH, scrollPixels + event.deltaY));
      markAllRows();
      scheduleRender();
      event.preventDefault();
    }, { passive: false });
    const connect = () => {
      reconnectTimer = undefined;
      if (blocked) return;
      socket = new WebSocket(socketURL(location, PAGE_INSTANCE, PAGE_TOKEN));
      socket.onopen = () => {
        reconnectDelay = 250;
        setStatus(shen ? "connected · shen · pixi" : "connected · pixi", true);
        // Not on touch: focusing there raises the keyboard, and a page that
        // opens with half the terminal behind a keyboard nobody asked for is
        // worse than one waiting for a tap.
        if (!touchInput()) app.canvas.focus();
      };
      socket.onmessage = (event) => {
        try { onMessage(JSON.parse(event.data)); }
        catch (error) { setStatus(`render error · ${error.message}`); console.error(error); }
      };
      socket.onclose = async () => {
        if (blocked) return;
        setStatus("disconnected · checking gateway");
        const identity = await gatewayIdentity();
        if (blocked) return;
        const verdict = refusalVerdict(identity, PAGE_INSTANCE);
        if (verdict.terminal) {
          blockPage(verdict.message);
          return;
        }
        setStatus("disconnected · retrying");
        if (reconnectTimer) return;
        reconnectTimer = setTimeout(connect, reconnectDelay);
        reconnectDelay = Math.min(5000, reconnectDelay * 2);
      };
      socket.onerror = () => { if (!blocked) setStatus("connection error"); };
    };
    connect();
  }

  start().catch((error) => setStatus(`renderer error · ${error.message}`));
})();
