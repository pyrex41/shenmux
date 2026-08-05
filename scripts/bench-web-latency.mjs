#!/usr/bin/env node

// Measures WebSocket-send -> echoed-delta latency. Run this against a muxd
// whose command is an echo-friendly PTY, for example:
//   muxd -session bench -- sh -c 'stty -icanon -echo; exec cat'
// and its shenmux-web gateway.

const args = process.argv.slice(2);
const value = (name, fallback) => {
  const index = args.indexOf(name);
  return index === -1 ? fallback : args[index + 1];
};
const url = value("--url", "ws://127.0.0.1:8789/ws");
const count = Number(value("--count", "40"));
const warmup = Number(value("--warmup", "5"));
const simulatedRttMs = Number(value("--simulated-rtt-ms", "0"));

if (!Number.isInteger(count) || count < 1 || !Number.isInteger(warmup) || warmup < 0 || !Number.isFinite(simulatedRttMs) || simulatedRttMs < 0) {
  throw new Error("--count and --warmup must be non-negative integers (count >= 1); --simulated-rtt-ms must be non-negative");
}
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

const percentile = (values, p) => {
  const sorted = [...values].sort((a, b) => a - b);
  const index = Math.min(sorted.length - 1, Math.max(0, Math.ceil((p / 100) * sorted.length) - 1));
  return sorted[index];
};
const waitFor = (socket, predicate) => new Promise((resolve, reject) => {
  let settled = false;
  const cleanup = () => {
    socket.removeEventListener("message", onMessage);
    socket.removeEventListener("error", onError);
    socket.removeEventListener("close", onClose);
  };
  const finish = (callback, value) => {
    if (settled) return;
    settled = true;
    cleanup();
    callback(value);
  };
  const onMessage = (event) => {
    let message;
    try { message = JSON.parse(event.data.toString()); } catch (_) { return; }
    if (!predicate(message)) return;
    finish(resolve, message);
  };
  const onError = (event) => finish(reject, event instanceof Error ? event : new Error("WebSocket error"));
  const onClose = () => finish(reject, new Error("WebSocket closed"));
  socket.addEventListener("message", onMessage);
  socket.addEventListener("error", onError, { once: true });
  socket.addEventListener("close", onClose, { once: true });
});

const socket = new WebSocket(url);
await new Promise((resolve, reject) => {
  socket.addEventListener("open", resolve, { once: true });
  socket.addEventListener("error", reject, { once: true });
});
await waitFor(socket, (message) => message.type === "snapshot");

const samples = [];
for (let i = 0; i < warmup + count; i++) {
  const data = String.fromCharCode(0x61 + (i % 26));
  const sent = performance.now();
  // This is an application-level controlled profile for hosts where tc/dnctl
  // is unavailable. It delays each half of the measured path equally; use a
  // real network emulator for wire-level measurements.
  if (simulatedRttMs > 0) await sleep(simulatedRttMs / 2);
  socket.send(JSON.stringify({ type: "input", data }));
  await waitFor(socket, (message) => message.type === "delta" || message.type === "error");
  if (simulatedRttMs > 0) await sleep(simulatedRttMs / 2);
  const elapsed = performance.now() - sent;
  if (i >= warmup) samples.push(elapsed);
}
socket.close();

const mean = samples.reduce((sum, value) => sum + value, 0) / samples.length;
console.log(JSON.stringify({
  url,
  simulated_rtt_ms: simulatedRttMs,
  samples: samples.length,
  min_ms: Math.min(...samples),
  p50_ms: percentile(samples, 50),
  p95_ms: percentile(samples, 95),
  p99_ms: percentile(samples, 99),
  max_ms: Math.max(...samples),
  mean_ms: mean,
}, null, 2));
