import {
  MAX_READ,
  RemoteObjectBackend,
  WorkspaceJournal,
  base64ToBytes,
  bytesToBase64,
  normalizePath,
  parseCommand,
} from "/workspace-runtime.js";

(() => {
  "use strict";

  const COMMANDS = ["help", "ls", "pwd", "cd", "cat", "touch", "mkdir", "write", "clear", "sync"];

  class LocalOPFSWorkspace {
    constructor(root) { this.root = root; }
    normalize(path, base = "/") { return normalizePath(path, base); }
    parts(path) { return normalizePath(path).split("/").filter(Boolean); }

    async directory(path, create = false) {
      let dir = this.root;
      for (const part of this.parts(path)) dir = await dir.getDirectoryHandle(part, { create });
      return dir;
    }

    async parent(path, create = false) {
      const parts = this.parts(path);
      const name = parts.pop();
      let dir = this.root;
      for (const part of parts) dir = await dir.getDirectoryHandle(part, { create });
      return { dir, name };
    }

    async list(path = "/") {
      const dir = await this.directory(path);
      const entries = [];
      for await (const [name, handle] of dir.entries()) {
        if (name.startsWith(".shenmux-")) continue;
        let size = 0;
        if (handle.kind === "file") { try { size = (await handle.getFile()).size; } catch (_) {} }
        entries.push({ name, kind: handle.kind, size });
      }
      return entries.sort((a, b) => a.kind.localeCompare(b.kind) || a.name.localeCompare(b.name));
    }

    async readBytes(path) {
      const { dir, name } = await this.parent(path);
      return new Uint8Array(await (await dir.getFileHandle(name)).getFile().then((file) => file.arrayBuffer()));
    }

    async read(path) { return new TextDecoder().decode(await this.readBytes(path)); }

    async writeBytes(path, bytes) {
      const { dir, name } = await this.parent(path, true);
      const file = await dir.getFileHandle(name, { create: true });
      const writable = await file.createWritable();
      await writable.write(bytes);
      await writable.close();
    }

    async write(path, contents) {
      await this.writeBytes(path, typeof contents === "string" ? new TextEncoder().encode(contents) : contents);
    }

    async mkdir(path) { await this.directory(path, true); }
  }

  // The WIT host functions are synchronous. This cache prepares the needed
  // directory/file window asynchronously, then exposes only memory to WASI.
  class CachedWorkspaceHost {
    constructor(local, remote = null) {
      this.local = local;
      this.remote = remote;
      this.files = new Map();
      this.entries = new Map();
      this.loadedDirs = new Set();
      this.etags = new Map();
      this.dirty = new Map();
      this.journal = new WorkspaceJournal(local);
      this.syncState = "local";
    }

    normalize(path, base = "/") { return normalizePath(path, base); }

    async init() {
      await this.journal.load();
      if (!this.remote) {
        try { await this.local.readBytes("/README.md"); }
        catch (_) {
          await this.local.write("/README.md", "# shenmux workspace\n\nThis file lives in the browser's local workspace.\n");
          await this.local.mkdir("/notes");
          await this.local.write("/notes/hello.txt", "local-first, instant by default\n");
        }
      }
      await this.loadDir("/");
    }

    async loadDir(path) {
      path = this.normalize(path);
      if (this.loadedDirs.has(path)) return;
      const entries = this.remote ? await this.remote.list(path) : await this.local.list(path);
      this.entries.set(path, new Map(entries.filter((entry) => !entry.name.startsWith(".shenmux-"))
        .map((entry) => [entry.name, { ...entry, path: this.normalize(`${path}/${entry.name}`) }])));
      this.loadedDirs.add(path);
    }

    async loadFile(path) {
      path = this.normalize(path);
      if (this.files.has(path)) return this.files.get(path);
      let bytes;
      let etag = "";
      try {
        bytes = await this.local.readBytes(path);
      } catch (_) {
        if (!this.remote) throw new Error(`file not found: ${path}`);
        const result = await this.remote.read(path, 0, MAX_READ);
        bytes = result.bytes;
        etag = result.etag;
        await this.local.writeBytes(path, bytes);
      }
      this.files.set(path, bytes);
      this.etags.set(path, etag);
      return bytes;
    }

    async prepare(command, cwd) {
      const args = parseCommand(command);
      const name = args[0];
      if (["ls", "cd"].includes(name)) await this.loadDir(this.normalize(args[1] || "/", cwd));
      else if (name === "cat") await this.loadFile(this.normalize(args[1] || "", cwd));
      else if (["touch", "write", "mkdir"].includes(name)) {
        const path = this.normalize(args[1] || "", cwd);
        await this.loadDir(this.normalize(`${path}/..`));
        if (name === "touch" && this.entries.get(this.normalize(`${path}/..`))?.has(path.split("/").pop())) {
          await this.loadFile(path);
        }
      }
    }

    listEntries(path) {
      const entries = this.entries.get(this.normalize(path));
      return entries ? [...entries.values()].map(({ name, kind, size }) => ({ name, kind, size: size || 0 })) : [];
    }

    read(path, offset, length) {
      const bytes = this.files.get(this.normalize(path)) || new Uint8Array();
      return bytes.slice(Number(offset), Number(offset) + Number(length));
    }

    write(path, data, truncate) {
      path = this.normalize(path);
      if (!truncate && this.files.has(path)) return;
      const bytes = new Uint8Array(data);
      this.files.set(path, bytes);
      this.dirty.set(path, { kind: "write", bytes });
      const parent = this.normalize(`${path}/..`);
      const name = path.split("/").pop();
      if (!this.entries.has(parent)) this.entries.set(parent, new Map());
      this.entries.get(parent).set(name, { name, kind: "file", size: bytes.length, path });
    }

    mkdir(path) {
      path = this.normalize(path);
      const parts = path.split("/").filter(Boolean);
      let parent = "/";
      for (const name of parts) {
        const next = this.normalize(`${parent}/${name}`);
        if (!this.entries.has(parent)) this.entries.set(parent, new Map());
        this.entries.get(parent).set(name, { name, kind: "directory", size: 0, path: next });
        this.entries.set(next, this.entries.get(next) || new Map());
        this.loadedDirs.add(next);
        parent = next;
      }
      this.dirty.set(path, { kind: "mkdir" });
    }

    async commit() {
      for (const [path, change] of this.dirty) {
        if (change.kind === "mkdir") {
          await this.local.mkdir(path);
          if (this.remote) await this.journal.append({ id: crypto.randomUUID(), op: "mkdir", path });
        } else {
          await this.local.writeBytes(path, change.bytes);
          if (this.remote) await this.journal.append({
            id: crypto.randomUUID(), op: "write", path, data: bytesToBase64(change.bytes), etag: this.etags.get(path) || "",
          });
        }
      }
      this.dirty.clear();
      if (this.remote) {
        this.syncState = "syncing";
        void this.flush().catch(() => { this.syncState = "offline"; });
      }
    }

    async flush() {
      if (!this.remote) return { pending: 0, conflict: false };
      const result = await this.journal.flush(this.remote);
      this.syncState = result.conflict ? "conflict" : result.pending ? "offline" : "synced";
      return result;
    }
  }

  const fileList = document.querySelector("#file-list");
  const editor = document.querySelector("#editor");
  const fileName = document.querySelector("#file-name");
  const fileMeta = document.querySelector("#file-meta");
  const commandInput = document.querySelector("#command-input");
  const commandOutput = document.querySelector("#command-output");
  const commandForm = document.querySelector("#command-form");
  const commandPrompt = document.querySelector("#command-prompt");
  const storageStatus = document.querySelector("#storage-status");
  const saveStatus = document.querySelector("#save-status");
  let workspace;
  let cwd = "/";
  let openPath = "/README.md";
  let history = [];
  let historyIndex = 0;
  let running = false;
  let cancelGeneration = 0;
  let completion = null;
  let runtimeLabel = "OPFS";

  const displayPath = (path) => path === "/" ? "/" : path.slice(1);
  const promptText = () => `λ ${cwd}`;
  const setPrompt = () => { commandPrompt.textContent = promptText(); };
  const print = (text, kind = "") => {
    const line = document.createElement("div");
    line.className = `command-line ${kind}`;
    line.textContent = text;
    commandOutput.append(line);
    commandOutput.scrollTop = commandOutput.scrollHeight;
  };
  const printCommand = (text) => {
    const line = document.createElement("div");
    line.className = "command-line entered";
    line.textContent = `${promptText()} ${text}`;
    commandOutput.append(line);
  };

  async function refreshFiles() {
    const entries = await workspace.list(cwd);
    fileList.replaceChildren();
    for (const entry of entries) {
      const button = document.createElement("button");
      const path = workspace.normalize(`${cwd}/${entry.name}`);
      button.className = `file-entry ${entry.kind}${path === openPath ? " active" : ""}`;
      button.innerHTML = `<span class="kind">${entry.kind === "directory" ? "▸" : "·"}</span><span></span>`;
      button.lastElementChild.textContent = entry.name;
      button.addEventListener("click", async () => {
        if (entry.kind === "directory") { cwd = path; setPrompt(); await refreshFiles(); return; }
        await openFile(path);
      });
      fileList.append(button);
    }
    if (cwd !== "/") {
      const up = document.createElement("button");
      up.className = "file-entry dir";
      up.innerHTML = "<span class=\"kind\">↩</span><span>..</span>";
      up.addEventListener("click", async () => { cwd = workspace.normalize(`${cwd}/..`); setPrompt(); await refreshFiles(); });
      fileList.prepend(up);
    }
  }

  async function openFile(path) {
    const started = performance.now();
    openPath = path;
    editor.value = await workspace.read(path);
    fileName.textContent = displayPath(path);
    fileMeta.textContent = `${runtimeLabel} · ${(performance.now() - started).toFixed(1)} ms`;
    await refreshFiles();
    editor.focus();
  }

  function recordHistory(text) {
    if (history[history.length - 1] !== text) history.push(text);
    if (history.length > 100) history = history.slice(-100);
    historyIndex = history.length;
  }
  function historyMove(direction) {
    if (!history.length) return;
    historyIndex = Math.max(0, Math.min(history.length, historyIndex + direction));
    commandInput.value = historyIndex === history.length ? "" : history[historyIndex];
    requestAnimationFrame(() => commandInput.setSelectionRange(commandInput.value.length, commandInput.value.length));
  }

  async function completeInput() {
    const value = commandInput.value;
    const cursor = commandInput.selectionStart ?? value.length;
    if (cursor !== value.length) return;
    const match = value.slice(0, cursor).match(/(?:^|\s)([^\s]*)$/);
    if (!match) return;
    const token = match[1];
    const tokenStart = cursor - token.length;
    const args = parseCommand(value.slice(0, tokenStart));
    let candidates = [];
    if (!args.length) candidates = COMMANDS.filter((name) => name.startsWith(token));
    else {
      const command = args[0];
      const base = token.includes("/") ? token.slice(0, token.lastIndexOf("/") + 1) : "";
      const leaf = token.slice(token.lastIndexOf("/") + 1);
      try {
        const entries = await workspace.list(workspace.normalize(base || ".", cwd));
        candidates = entries.filter((entry) => entry.name.startsWith(leaf))
          .filter((entry) => command === "cd" ? entry.kind === "directory" : true)
          .map((entry) => `${base}${entry.name}${entry.kind === "directory" ? "/" : ""}`);
      } catch (_) { return; }
    }
    if (!candidates.length) return;
    const common = candidates.reduce((prefix, candidate) => {
      let i = 0;
      while (i < prefix.length && i < candidate.length && prefix[i] === candidate[i]) i++;
      return prefix.slice(0, i);
    });
    const replacement = candidates.length === 1 ? candidates[0] : common;
    commandInput.value = `${value.slice(0, tokenStart)}${replacement}${value.slice(cursor)}`;
    commandInput.setSelectionRange(tokenStart + replacement.length, tokenStart + replacement.length);
    if (candidates.length > 1 && replacement === token) print(candidates.join("  "), "hint");
    completion = { token, candidates };
  }

  async function command(text) {
    const started = performance.now();
    const args = parseCommand(text);
    const name = args.shift();
    if (!name) return;
    const generation = cancelGeneration;
    const checkCancelled = () => { if (generation !== cancelGeneration) throw new Error("cancelled"); };
    if (name === "clear") commandOutput.replaceChildren();
    else if (["help", "pwd", "ls", "cd", "cat", "touch", "mkdir", "write", "sync"].includes(name)) {
      const result = await workspace.invoke(text);
      checkCancelled();
      if (name === "ls") print(result.entries.map((entry) => `${entry.kind === "directory" ? "▸" : " "} ${entry.name}`).join("\n") || "(empty)");
      else if (name === "sync") print(`${result.output} · ${workspace.syncState}`);
      else if (result.output) print(result.output);
      cwd = result.cwd;
      setPrompt();
      await refreshFiles();
      if (["write", "touch"].includes(name)) {
        const path = workspace.normalize(args[0] || "", cwd);
        if (path === openPath) await openFile(path);
      }
    } else print(`command not found: ${name}`, "error");
    fileMeta.textContent = `${runtimeLabel} · ${(performance.now() - started).toFixed(1)} ms`;
  }

  async function start() {
    if (!navigator.storage?.getDirectory) throw new Error("OPFS is unavailable; use a secure browser context");
    commandInput.disabled = true;
    commandInput.placeholder = "starting local runtime…";
    const root = await navigator.storage.getDirectory();
    try {
      const componentModule = await import("/workspace-component.js");
      const local = new LocalOPFSWorkspace(root);
      const remoteConfig = globalThis.__SHENMUX_WORKSPACE_REMOTE__ || null;
      let remote = remoteConfig ? new RemoteObjectBackend(remoteConfig) : null;
      let host = new CachedWorkspaceHost(local, remote);
      try {
        await host.init();
      } catch (error) {
        if (!remote) throw error;
        print(`remote workspace unavailable · using local OPFS · ${error.message}`, "hint");
        remote = null;
        host = new CachedWorkspaceHost(local, null);
        await host.init();
      }
      workspace = {
        host,
        syncState: host.syncState,
        normalize: (path, base = cwd) => normalizePath(path, base),
        async invoke(commandText) {
          await host.prepare(commandText, cwd);
          componentModule.setWorkspaceFilesystem(host);
          let result;
          try { result = await componentModule.runtime.run(cwd, commandText); }
          finally { componentModule.setWorkspaceFilesystem(null); }
          if (result.error) throw new Error(result.error);
          await host.commit();
          workspace.syncState = host.syncState;
          return result;
        },
        async list(path) { return (await this.invoke(`ls ${normalizePath(path, cwd)}`)).entries; },
        async read(path) { return (await this.invoke(`cat ${normalizePath(path, cwd)}`)).output; },
        async write(path, contents) { await this.invoke(`write ${normalizePath(path, cwd)} ${contents}`); },
        async mkdir(path) { await this.invoke(`mkdir ${normalizePath(path, cwd)}`); },
        async touch(path) { await this.invoke(`touch ${normalizePath(path, cwd)}`); },
        async chdir(path) { return this.invoke(`cd ${normalizePath(path, cwd)}`); },
        async sync() { const result = await host.flush(); this.syncState = host.syncState; return result; },
      };
      runtimeLabel = remote ? "WASI · remote cache" : "WASI · OPFS cache";
    } catch (error) {
      runtimeLabel = "OPFS fallback";
      print(`WASI component unavailable · ${error.message}`, "hint");
      throw error;
    }
    await openFile(openPath);
    print(`ready · ${runtimeLabel} · Tab completes · ↑↓ history · Ctrl-L clears`);
    const estimate = await navigator.storage.estimate();
    const used = estimate.usage ? `${(estimate.usage / 1024 / 1024).toFixed(1)} MB` : "local";
    storageStatus.textContent = `${runtimeLabel} · ${used}`;
    setPrompt();
    commandInput.disabled = false;
    commandInput.placeholder = "try: ls, cat README.md, or write notes/today.md hello";
    commandInput.focus();
  }

  document.querySelector("#save-file").addEventListener("click", async () => {
    const started = performance.now();
    await workspace.write(openPath, editor.value);
    saveStatus.textContent = `saved locally in ${(performance.now() - started).toFixed(1)} ms`;
    await refreshFiles();
    setTimeout(() => { saveStatus.textContent = ""; }, 1800);
  });
  document.querySelector("#new-file").addEventListener("click", async () => {
    const name = window.prompt("New file name", "notes/new.txt");
    if (!name) return;
    await workspace.touch(normalizePath(name, cwd));
    await openFile(normalizePath(name, cwd));
  });
  commandForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    const text = commandInput.value.trim();
    if (!text || running || !workspace) return;
    recordHistory(text);
    commandInput.value = "";
    completion = null;
    printCommand(text);
    running = true;
    try { await command(text); } catch (error) { if (error.message !== "cancelled") print(error.message, "error"); }
    finally { running = false; setPrompt(); commandInput.focus(); }
  });
  commandInput.addEventListener("keydown", (event) => {
    if (event.key === "Tab") { event.preventDefault(); completeInput(); return; }
    if (event.key === "ArrowUp" && !event.altKey && !event.ctrlKey && !event.metaKey) { event.preventDefault(); historyMove(-1); return; }
    if (event.key === "ArrowDown" && !event.altKey && !event.ctrlKey && !event.metaKey) { event.preventDefault(); historyMove(1); return; }
    if (event.ctrlKey && event.key.toLowerCase() === "l") { event.preventDefault(); commandOutput.replaceChildren(); return; }
    if (event.ctrlKey && event.key.toLowerCase() === "c") {
      event.preventDefault(); cancelGeneration++;
      if (commandInput.value || running) { printCommand(`${commandInput.value}^C`); commandInput.value = ""; }
      running = false;
    }
  });
  editor.addEventListener("keydown", (event) => {
    if ((event.metaKey || event.ctrlKey) && event.key === "s") { event.preventDefault(); document.querySelector("#save-file").click(); }
  });
  start().catch((error) => { commandInput.disabled = true; storageStatus.textContent = `workspace unavailable · ${error.message}`; print(error.message, "error"); });
})();
