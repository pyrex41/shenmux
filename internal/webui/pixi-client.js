import { Application, BitmapFont, BitmapText, Container, Graphics } from "pixi.js";
import Shen from "shen-script";

(() => {
  "use strict";

  const FONT = "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace";
  const FONT_NAMES = { regular: "ShenmuxMono", bold: "ShenmuxMonoBold", italic: "ShenmuxMonoItalic", boldItalic: "ShenmuxMonoBoldItalic" };
  const FONT_SIZE = 15;
  const LINE_HEIGHT = 19;
  const MAX_COLS = 120;
  const MAX_ROWS = 48;
  const wrap = document.querySelector("#screen-wrap");
  const host = document.querySelector("#pixi-host");
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
  // Reconnects always begin with a fresh checkpoint. Keep the previous view
  // visible while offline, but never apply deltas from the old stream to it.
  let reconnectTimer;
  let reconnectDelay = 250;

  const setStatus = (text, good = false) => {
    status.textContent = text;
    status.style.color = good ? "#65e6a7" : "#748294";
  };
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

  function renderFrame() {
    const frame = state?.screen?.Frame;
    if (!frame?.Lines) { empty.hidden = false; return; }
    empty.hidden = true;
    setupGrid(frame.Cols, frame.Rows);
    if (!dirtyRows.size) return;
    const defaultBG = color(frame.DefaultBG, 0x080b10);
    const defaultFG = color(frame.DefaultFG, 0xd7e0ea);
    const width = frame.Cols * cellW;
    const height = frame.Rows * lineH;
    if (width !== rendererWidth || height !== rendererHeight) {
      rendererWidth = width;
      rendererHeight = height;
      app.renderer.resize(width, height);
      app.canvas.style.width = `${width}px`;
      app.canvas.style.height = `${height}px`;
    }
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
      resizeForViewport();
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
      return;
    }
    if (message.type === "command") {
      if (state && message.meta) state.control_owner = message.meta.control_owner || "";
      setStatus(state?.control_owner === clientID ? "connected · control" : "connected", true);
      refreshControlButtons();
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
    return {
      x: Math.max(1, Math.min(frame.Cols, Math.floor((event.clientX - rect.left) / cellW) + 1)),
      y: Math.max(1, Math.min(frame.Rows, Math.floor((event.clientY - rect.top) / lineH) + 1)),
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

  function resizeForViewport() {
    if (!state) return;
    const cols = Math.max(1, Math.min(MAX_COLS, Math.floor((wrap.clientWidth - 4) / cellW)));
    const rows = Math.max(1, Math.min(MAX_ROWS, Math.floor((wrap.clientHeight - 4) / lineH)));
    const frame = state.screen.Frame;
    if (cols !== frame.Cols || rows !== frame.Rows) sendCommand("resize", { cols, rows });
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
    app.canvas.addEventListener("keydown", (event) => {
      jumpToLive();
      const data = keyData(event);
      if (!data) return;
      event.preventDefault();
      sendCommand("input", { data });
    });
    app.canvas.addEventListener("paste", (event) => {
      event.preventDefault();
      jumpToLive();
      const data = event.clipboardData.getData("text/plain");
      const wrapped = terminalModes().BracketedPaste ? `\x1b[200~${data}\x1b[201~` : data;
      sendCommand("input", { data: wrapped });
    });
    app.canvas.addEventListener("pointerdown", (event) => {
      if (!terminalModes().Mouse) return;
      event.preventDefault();
      app.canvas.setPointerCapture?.(event.pointerId);
      app.canvas.focus();
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
    app.canvas.addEventListener("focus", () => {
      if (terminalModes().FocusEvents) sendCommand("input", { data: "\x1b[I" });
    });
    app.canvas.addEventListener("blur", () => {
      if (terminalModes().FocusEvents) sendCommand("input", { data: "\x1b[O" });
    });
    app.canvas.addEventListener("click", () => app.canvas.focus());
    acquireButton.addEventListener("click", () => sendCommand("acquire"));
    releaseButton.addEventListener("click", () => sendCommand("release"));
    refreshControlButtons();
    new ResizeObserver(() => setTimeout(resizeForViewport, 80)).observe(wrap);
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
    const scheme = location.protocol === "https:" ? "wss" : "ws";
    const connect = () => {
      reconnectTimer = undefined;
      socket = new WebSocket(`${scheme}://${location.host}/ws`);
      socket.onopen = () => {
        reconnectDelay = 250;
        setStatus(shen ? "connected · shen · pixi" : "connected · pixi", true);
        app.canvas.focus();
      };
      socket.onmessage = (event) => {
        try { onMessage(JSON.parse(event.data)); }
        catch (error) { setStatus(`render error · ${error.message}`); console.error(error); }
      };
      socket.onclose = () => {
        setStatus("disconnected · retrying");
        if (reconnectTimer) return;
        reconnectTimer = setTimeout(connect, reconnectDelay);
        reconnectDelay = Math.min(5000, reconnectDelay * 2);
      };
      socket.onerror = () => setStatus("connection error");
    };
    connect();
  }

  start().catch((error) => setStatus(`renderer error · ${error.message}`));
})();
