(() => {
  "use strict";

  const canvas = document.querySelector("#screen");
  const wrap = document.querySelector("#screen-wrap");
  const empty = document.querySelector("#empty");
  const status = document.querySelector("#status");
  const sessionLabel = document.querySelector("#session");
  const details = document.querySelector("#details");
  const ctx = canvas.getContext("2d");
  const palette = { fg: "#d7e0ea", bg: "#080b10" };
  let socket;
  let reconnectTimer;
  let reconnectDelay = 250;
  let state;
  let clientID = "";
  let lastSeq = 0;
  let cellW = 9;
  let lineH = 17;

  const setStatus = (text, good = false) => {
    status.textContent = text;
    status.style.color = good ? "#65e6a7" : "#748294";
  };
  const send = (message) => {
    if (socket && socket.readyState === WebSocket.OPEN) {
      socket.send(JSON.stringify(message));
      return true;
    }
    return false;
  };
  const acquireButton = document.querySelector("#acquire");
  const releaseButton = document.querySelector("#release");
  function refreshControlButtons() {
    const ownsControl = Boolean(state && clientID && state.control_owner === clientID);
    acquireButton.disabled = ownsControl;
    releaseButton.disabled = !ownsControl;
    acquireButton.textContent = ownsControl ? "control held" : "take control";
  }
  const color = (value, fallback) => value && value.Valid
    ? `rgb(${value.R},${value.G},${value.B})` : fallback;

  function measure() {
    ctx.font = "14px ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace";
    cellW = Math.ceil(ctx.measureText("M").width);
    lineH = 18;
  }

  function blankRow(cols) {
    return Array.from({ length: cols }, () => ({ Text: "", Width: 1, Style: {} }));
  }
  function blankFrame(cols, rows) {
    return { Cols: cols, Rows: rows, Lines: Array.from({ length: rows }, () => blankRow(cols)),
      Cursor: { X: 0, Y: 0, Visible: true, Blink: true, Style: 0 },
      DefaultFG: {}, DefaultBG: {} };
  }
  function applyDelta(delta) {
    if (!state || !delta) return false;
    let frame = state.screen.Frame;
    if (delta.Full || frame.Cols !== delta.Cols || frame.Rows !== delta.Rows) {
      frame = blankFrame(delta.Cols, delta.Rows);
      state.screen.Frame = frame;
    }
    for (const line of (delta.Lines || [])) frame.Lines[line.Y] = line.Cells;
    frame.Cursor = delta.Cursor;
    frame.AltScreen = delta.AltScreen;
    frame.Title = delta.Title;
    frame.WorkingDirectory = delta.WorkingDirectory;
    frame.DefaultFG = delta.DefaultFG;
    frame.DefaultBG = delta.DefaultBG;
    if (delta.HistoryReset) state.screen.History = [];
    if (delta.HistoryDrop) state.screen.History = state.screen.History.slice(delta.HistoryDrop);
    if (delta.HistoryAppend) state.screen.History.push(...delta.HistoryAppend);
    return true;
  }

  function draw() {
    const frame = state && state.screen && state.screen.Frame;
    if (!frame || !frame.Lines) { empty.hidden = false; return; }
    empty.hidden = true;
    measure();
    const dpr = window.devicePixelRatio || 1;
    canvas.width = Math.max(1, Math.ceil(frame.Cols * cellW * dpr));
    canvas.height = Math.max(1, Math.ceil(frame.Rows * lineH * dpr));
    canvas.style.width = `${frame.Cols * cellW}px`;
    canvas.style.height = `${frame.Rows * lineH}px`;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.font = "14px ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace";
    ctx.textBaseline = "alphabetic";
    ctx.fillStyle = color(frame.DefaultBG, palette.bg);
    ctx.fillRect(0, 0, frame.Cols * cellW, frame.Rows * lineH);
    for (let y = 0; y < frame.Rows; y++) {
      const row = frame.Lines[y] || [];
      for (let x = 0; x < frame.Cols; x++) {
        const cell = row[x] || { Text: "", Width: 1, Style: {} };
        if (cell.Width === 0) continue;
        const style = cell.Style || {};
        let fg = color(style.FG, color(frame.DefaultFG, palette.fg));
        let bg = color(style.BG, color(frame.DefaultBG, palette.bg));
        if (style.Inverse) [fg, bg] = [bg, fg];
        if (bg !== palette.bg) { ctx.fillStyle = bg; ctx.fillRect(x * cellW, y * lineH, cellW * (cell.Width || 1), lineH); }
        if (style.Invisible) continue;
        ctx.fillStyle = fg;
        ctx.font = `${style.Bold ? "700" : "400"} 14px ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace`;
        ctx.fillText(cell.Text || " ", x * cellW, y * lineH + 14);
        if (style.Underline || style.DoubleUnderline) { ctx.fillRect(x * cellW, y * lineH + 16, cellW, style.DoubleUnderline ? 2 : 1); }
      }
    }
    const cursor = frame.Cursor;
    if (cursor && cursor.Visible && cursor.X < frame.Cols && cursor.Y < frame.Rows) {
      ctx.globalAlpha = .72;
      ctx.fillStyle = color(frame.DefaultFG, palette.fg);
      if (cursor.Style === 1) ctx.fillRect(cursor.X * cellW, cursor.Y * lineH, 2, lineH);
      else if (cursor.Style === 2) ctx.fillRect(cursor.X * cellW, (cursor.Y + 1) * lineH - 2, cellW, 2);
      else ctx.fillRect(cursor.X * cellW, cursor.Y * lineH, cellW, lineH);
      ctx.globalAlpha = 1;
    }
    details.textContent = `${frame.Cols}×${frame.Rows} · seq ${lastSeq}${frame.Title ? ` · ${frame.Title}` : ""}`;
  }

  function onMessage(message) {
    if (message.type === "snapshot") {
      clientID = message.client_id || "";
      state = { seq: message.current.seq, screen: message.current.screen,
        control_owner: message.current.control_owner || "" };
      lastSeq = message.current.seq || 0;
      sessionLabel.textContent = message.session || "session";
      setStatus(state.control_owner === message.client_id ? "connected · control" : "connected", true);
      refreshControlButtons();
      draw();
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
      draw();
      return;
    }
    if (message.type === "command") {
      if (state && message.meta) state.control_owner = message.meta.control_owner || "";
      const ownsControl = Boolean(state && state.control_owner === clientID);
      setStatus(ownsControl ? "connected · control" : "connected", true);
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
  canvas.addEventListener("keydown", (event) => {
    const data = keyData(event);
    if (!data) return;
    event.preventDefault(); send({ type: "input", data });
  });
  canvas.addEventListener("paste", (event) => {
    event.preventDefault(); send({ type: "input", data: event.clipboardData.getData("text/plain") });
  });
  canvas.addEventListener("click", () => canvas.focus());

  function resizeForViewport() {
    if (!state) return;
    const cols = Math.max(1, Math.floor((wrap.clientWidth - 4) / cellW));
    const rows = Math.max(1, Math.floor((wrap.clientHeight - 4) / lineH));
    const frame = state.screen.Frame;
    if (cols !== frame.Cols || rows !== frame.Rows) send({ type: "resize", cols, rows });
  }
  new ResizeObserver(() => setTimeout(resizeForViewport, 80)).observe(wrap);
  acquireButton.addEventListener("click", () => send({ type: "acquire" }));
  releaseButton.addEventListener("click", () => send({ type: "release" }));
  refreshControlButtons();

  const scheme = location.protocol === "https:" ? "wss" : "ws";
  function connect() {
    if (socket && socket.readyState === WebSocket.OPEN) return;
    socket = new WebSocket(`${scheme}://${location.host}/ws`);
    socket.onopen = () => {
      reconnectDelay = 250;
      setStatus("connected · restoring", true);
      canvas.focus();
    };
    socket.onmessage = (event) => { try { onMessage(JSON.parse(event.data)); } catch (_) { setStatus("invalid server message"); } };
    socket.onclose = () => {
      setStatus("disconnected · retrying");
      if (reconnectTimer) return;
      reconnectTimer = setTimeout(() => {
        reconnectTimer = undefined;
        connect();
      }, reconnectDelay);
      reconnectDelay = Math.min(5000, reconnectDelay * 2);
    };
    socket.onerror = () => setStatus("connection error");
  }
  connect();
})();
