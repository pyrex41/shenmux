import assert from "node:assert/strict";
import { runtime, setWorkspaceFilesystem } from "../internal/webui/workspace-component.js";

const encoder = new TextEncoder();
const decoder = new TextDecoder();
const normalize = (path, base = "/") => {
  const source = path.startsWith("/") ? path : `${base}/${path}`;
  const parts = [];
  for (const part of source.split("/")) {
    if (!part || part === ".") continue;
    if (part === "..") parts.pop(); else parts.push(part);
  }
  return `/${parts.join("/")}`;
};

class MemoryHost {
  constructor() {
    this.files = new Map([
      ["/README.md", encoder.encode("# test workspace\n")],
      ["/notes/hello.txt", encoder.encode("hello\n")],
    ]);
    this.directories = new Set(["/", "/notes"]);
  }

  listEntries(path) {
    path = normalize(path);
    const prefix = path === "/" ? "/" : `${path}/`;
    const names = new Map();
    for (const directory of this.directories) {
      if (!directory.startsWith(prefix) || directory === path) continue;
      const name = directory.slice(prefix.length).split("/")[0];
      names.set(name, { name, kind: "directory", size: 0 });
    }
    for (const [file, bytes] of this.files) {
      if (!file.startsWith(prefix)) continue;
      const name = file.slice(prefix.length).split("/")[0];
      if (!name.includes("/")) names.set(name, { name, kind: "file", size: bytes.length });
    }
    return [...names.values()].sort((a, b) => a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
  }

  read(path, offset, length) {
    const bytes = this.files.get(normalize(path));
    if (!bytes) throw new Error(`missing ${path}`);
    return bytes.slice(Number(offset), Number(offset) + Number(length));
  }

  write(path, data, truncate) {
    path = normalize(path);
    if (!truncate && this.files.has(path)) return;
    this.files.set(path, new Uint8Array(data));
  }

  mkdir(path) {
    path = normalize(path);
    let current = "/";
    for (const part of path.split("/").filter(Boolean)) {
      current = normalize(`${current}/${part}`);
      this.directories.add(current);
    }
  }
}

const host = new MemoryHost();
let cwd = "/";

async function run(command) {
  setWorkspaceFilesystem(host);
  try {
    const response = await runtime.run(cwd, command);
    assert.equal(response.error, undefined, `${command}: ${response.error ?? "unknown error"}`);
    cwd = response.cwd;
    return response;
  } finally {
    setWorkspaceFilesystem(null);
  }
}

const root = await run("ls /");
assert.deepEqual(root.entries.map((entry) => entry.name), ["README.md", "notes"]);
await run("write notes/smoke.txt wasi component");
assert.equal(decoder.decode(host.files.get("/notes/smoke.txt")), "wasi component");
assert.equal((await run("cat notes/smoke.txt")).output, "wasi component");
assert.equal((await run("cd notes")).cwd, "/notes");
assert.equal((await run("pwd")).output, "/notes");
console.log("workspace component smoke test passed");
