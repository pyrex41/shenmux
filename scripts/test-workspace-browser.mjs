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
const navigationURL = new URL(pageURL);
navigationURL.searchParams.set("smoke", Date.now().toString());
await command("Page.navigate", { url: navigationURL.href });

const evaluate = async (expression) => {
  const result = await command("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
  if (result.result?.exceptionDetails) throw new Error(result.result.exceptionDetails.text || "browser evaluation failed");
  return result.result?.result?.value;
};
const submit = (text, expected) => `(async()=>{const input=document.querySelector("#command-input"); input.value=${JSON.stringify(text)}; input.form.requestSubmit(); const deadline=Date.now()+5000; while(Date.now()<deadline){const output=document.querySelector("#command-output").innerText; if(output.includes(${JSON.stringify(expected)})) return output; await new Promise(r=>setTimeout(r,50));} return document.querySelector("#command-output").innerText})()`;

let ready = false;
const readyDeadline = Date.now() + 5000;
while (!ready && Date.now() < readyDeadline) {
  ready = await evaluate(`location.href === ${JSON.stringify(navigationURL.href)} && document.querySelector("#command-input")?.disabled === false`);
  if (!ready) await new Promise((resolve) => setTimeout(resolve, 50));
}
assert.equal(ready, true, "workspace command input did not become ready");
const commandRowAligned = await evaluate(`(() => {
  const input = document.querySelector("#command-input").getBoundingClientRect();
  const hint = document.querySelector(".command-hint").getBoundingClientRect();
  return Math.abs((input.top + input.height / 2) - (hint.top + hint.height / 2)) < 1;
})()`);
assert.equal(commandRowAligned, true, "Tab completion hint is not aligned with the command input");
const completionsInline = await evaluate(`(async () => {
  const input = document.querySelector("#command-input");
  input.value = "cat ";
  input.dispatchEvent(new Event("input", { bubbles: true }));
  input.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab", bubbles: true, cancelable: true }));
  const deadline = Date.now() + 1000;
  while (!document.querySelector("#command-form").classList.contains("has-completions") && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  const entry = document.querySelector("#command-form").getBoundingClientRect();
  const candidates = document.querySelector("#command-completions");
  const bounds = candidates.getBoundingClientRect();
  const inputBounds = input.getBoundingClientRect();
  const result = candidates.textContent.includes("README.md")
    && Math.abs((entry.top + entry.height / 2) - (bounds.top + bounds.height / 2)) < 1
    && bounds.left >= inputBounds.right
    && bounds.left - inputBounds.right <= 12
    && !document.querySelector("#command-output").innerText.includes("notes/  README.md");
  input.value = "";
  input.dispatchEvent(new Event("input", { bubbles: true }));
  return result;
})()`);
assert.equal(completionsInline, true, "Tab completion candidates are not shown in the command row");
const marker = `browser-${Date.now()}`;
const writeOutput = await evaluate(submit(`write notes/browser-test.txt ${marker}`, "saved notes/browser-test.txt"));
assert.match(writeOutput, new RegExp(`saved notes/browser-test\\.txt`));
const readOutput = await evaluate(submit("cat notes/browser-test.txt", marker));
assert.match(readOutput, new RegExp(marker));
await command("Page.reload");
await new Promise((resolve) => setTimeout(resolve, 1000));
const persisted = await evaluate(submit("cat notes/browser-test.txt", marker));
assert.match(persisted, new RegExp(marker));
socket.close();
console.log("workspace browser smoke test passed");
