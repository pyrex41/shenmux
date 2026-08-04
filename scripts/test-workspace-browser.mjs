#!/usr/bin/env node
import assert from "node:assert/strict";

const args = process.argv.slice(2);
const value = (name, fallback) => {
  const index = args.indexOf(name);
  return index === -1 ? fallback : args[index + 1];
};
const pageURL = value("--url", "http://127.0.0.1:8791/workspace");
const cdpURL = value("--cdp", "http://127.0.0.1:9223");
const pages = await (await fetch(`${cdpURL}/json/list`)).json();
const page = pages.find((candidate) => candidate.type === "page");
if (!page) throw new Error("no Chrome page available; start Chrome with --remote-debugging-port=9223");

const socket = new WebSocket(page.webSocketDebuggerUrl);
let nextID = 0;
const pending = new Map();
socket.onmessage = (event) => {
  const message = JSON.parse(event.data);
  if (message.id && pending.has(message.id)) {
    pending.get(message.id)(message);
    pending.delete(message.id);
  }
};
const command = (method, params = {}) => new Promise((resolve) => {
  const id = ++nextID;
  pending.set(id, resolve);
  socket.send(JSON.stringify({ id, method, params }));
});
await new Promise((resolve) => { socket.addEventListener("open", resolve, { once: true }); });
await command("Page.navigate", { url: pageURL });
await new Promise((resolve) => setTimeout(resolve, 1000));

const evaluate = async (expression) => {
  const result = await command("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
  if (result.result?.exceptionDetails) throw new Error(result.result.exceptionDetails.text || "browser evaluation failed");
  return result.result?.result?.value;
};
const submit = (text) => `(async()=>{const input=document.querySelector("#command-input"); input.value=${JSON.stringify(text)}; input.form.requestSubmit(); await new Promise(r=>setTimeout(r,250)); return document.querySelector("#command-output").innerText})()`;

const ready = await evaluate(`!document.querySelector("#command-input").disabled`);
assert.equal(ready, true, "workspace command input did not become ready");
const marker = `browser-${Date.now()}`;
const writeOutput = await evaluate(submit(`write notes/browser-test.txt ${marker}`));
assert.match(writeOutput, new RegExp(`saved notes/browser-test\\.txt`));
const readOutput = await evaluate(submit("cat notes/browser-test.txt"));
assert.match(readOutput, new RegExp(marker));
await command("Page.reload");
await new Promise((resolve) => setTimeout(resolve, 1000));
const persisted = await evaluate(submit("cat notes/browser-test.txt"));
assert.match(persisted, new RegExp(marker));
socket.close();
console.log("workspace browser smoke test passed");
