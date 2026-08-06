import assert from "node:assert/strict";
import { MAX_READ, RemoteConflictError, RemoteObjectBackend, WorkspaceJournal, bytesToBase64, base64ToBytes } from "../internal/webui/workspace-runtime.js";
import { CachedWorkspaceHost } from "../internal/webui/workspace.js";

const calls = [];
const response = (body, { status = 200, headers = {} } = {}) => ({
  ok: status >= 200 && status < 300,
  status,
  statusText: "error",
  headers: new Headers(headers),
  async json() { return body; },
  async arrayBuffer() {
    if (body instanceof Uint8Array) return body.buffer.slice(body.byteOffset, body.byteOffset + body.byteLength);
    return new TextEncoder().encode(body).buffer;
  },
  async text() { return typeof body === "string" ? body : JSON.stringify(body); },
});

const backend = new RemoteObjectBackend({
  baseURL: "https://workspace.test/api",
  capability: "capability-1",
  fetchImpl: async (url, init = {}) => {
    calls.push({ url, init });
    if (init.method === "PUT") return response("", { status: 204, headers: { ETag: '"new"' } });
    if (url.includes("/objects")) return response("llo", { headers: { ETag: '"etag"' } });
    return response({ entries: [{ name: "notes", kind: "directory", size: 0 }] });
  },
});

assert.deepEqual(await backend.list("/"), [{ name: "notes", kind: "directory", size: 0 }]);
assert.equal((await backend.read("/notes/a.txt", 2, 3)).etag, '"etag"');
assert.equal(await backend.write("/notes/a.txt", new Uint8Array([1]), { etag: '"old"', operationID: "op-1" }), '"new"');
await backend.mkdir("/notes", "mkdir-1");
assert.equal(calls[0].init.headers.Authorization, "Bearer capability-1");
assert.equal(calls[1].init.headers.Range, "bytes=2-4");
assert.equal(calls[2].init.headers["If-Match"], '"old"');
assert.equal(calls[2].init.headers["Idempotency-Key"], "op-1");

const remoteBytes = new Uint8Array(MAX_READ + 3).fill(7);
const pageCalls = [];
const pagedBackend = new RemoteObjectBackend({
  baseURL: "https://workspace.test",
  capability: "cap",
  fetchImpl: async (_url, init = {}) => {
    pageCalls.push(init.headers);
    if (init.headers["If-None-Match"] === '"cached"') {
      return response("", { status: 304, headers: { ETag: '"cached"' } });
    }
    const match = init.headers.Range.match(/^bytes=(\d+)-(\d+)$/);
    const start = Number(match[1]);
    const end = Math.min(Number(match[2]), remoteBytes.length - 1);
    return response(remoteBytes.slice(start, end + 1), {
      status: 206,
      headers: { ETag: '"paged"', "Content-Range": `bytes ${start}-${end}/${remoteBytes.length}` },
    });
  },
});
const completeRead = await pagedBackend.readAll("/large.bin");
assert.equal(completeRead.bytes.length, remoteBytes.length);
assert.equal(pageCalls.length, 2);
assert.equal((await pagedBackend.readAll("/large.bin", { etag: '"cached"' })).notModified, true);

const conflictBackend = new RemoteObjectBackend({
  baseURL: "https://workspace.test",
  capability: "cap",
  fetchImpl: async () => response("conflict", { status: 409 }),
});
await assert.rejects(() => conflictBackend.write("/a", new Uint8Array()), RemoteConflictError);

assert.deepEqual([...base64ToBytes(bytesToBase64(new Uint8Array([1, 2, 3])))], [1, 2, 3]);

const local = {
  value: "[]",
  async read() { return this.value; },
  async write(_path, value) { this.value = value; },
};
const journal = new WorkspaceJournal(local);
await journal.load();
await journal.append({ id: "op-1", op: "write", path: "/a", data: bytesToBase64(new Uint8Array([7])), etag: "" });
let flushed = [];
const remote = {
  async write(path, data, options) { flushed.push({ path, data: [...data], options }); },
};
assert.deepEqual(await journal.flush(remote), { pending: 0, conflict: false, etags: {} });
assert.deepEqual(flushed[0].data, [7]);
assert.equal(journal.records.length, 0);

await journal.append({ id: "offline-1", op: "write", path: "/offline", data: bytesToBase64(new Uint8Array([8])), etag: "" });
assert.deepEqual(await journal.flush({ async write() { throw new Error("network down"); } }), { pending: 1, conflict: false, etags: {} });
assert.equal(journal.records.length, 1);

journal.records = [];
await journal.save();
await journal.append({ id: "version-1", op: "write", path: "/versioned", data: bytesToBase64(new Uint8Array([1])), etag: '"v0"' });
await journal.append({ id: "version-2", op: "write", path: "/versioned", data: bytesToBase64(new Uint8Array([2])), etag: '"v0"' });
const versions = [];
const versionResult = await journal.flush({
  async write(_path, _data, options) {
    versions.push(options.etag);
    return versions.length === 1 ? '"v1"' : '"v2"';
  },
});
assert.deepEqual(versions, ['"v0"', '"v1"']);
assert.deepEqual(versionResult, { pending: 0, conflict: false, etags: { "/versioned": '"v2"' } });

journal.records = [];
await journal.save();
await journal.append({ id: "concurrent-1", op: "write", path: "/concurrent", data: bytesToBase64(new Uint8Array([1])), etag: '"c0"' });
let releaseFirst;
let markFirstStarted;
const firstStarted = new Promise((resolve) => { markFirstStarted = resolve; });
const firstBlocked = new Promise((resolve) => { releaseFirst = resolve; });
const concurrentVersions = [];
const concurrentFlush = journal.flush({
  async write(_path, _data, options) {
    concurrentVersions.push(options.etag);
    if (concurrentVersions.length === 1) {
      markFirstStarted();
      await firstBlocked;
    }
    return concurrentVersions.length === 1 ? '"c1"' : '"c2"';
  },
});
await firstStarted;
await journal.append({ id: "concurrent-2", op: "write", path: "/concurrent", data: bytesToBase64(new Uint8Array([2])), etag: '"c0"' });
releaseFirst();
assert.deepEqual(await concurrentFlush, { pending: 0, conflict: false, etags: { "/concurrent": '"c2"' } });
assert.deepEqual(concurrentVersions, ['"c0"', '"c1"']);

class MemoryWorkspace {
  constructor(files = {}) {
    this.files = new Map(Object.entries(files).map(([path, value]) => [path, typeof value === "string" ? new TextEncoder().encode(value) : value]));
    this.directories = new Set(["/"]);
  }
  async read(path) { return new TextDecoder().decode(await this.readBytes(path)); }
  async readBytes(path) {
    const value = this.files.get(path);
    if (!value) throw new Error(`missing ${path}`);
    return new Uint8Array(value);
  }
  async write(path, value) { await this.writeBytes(path, typeof value === "string" ? new TextEncoder().encode(value) : value); }
  async writeBytes(path, value) { this.files.set(path, new Uint8Array(value)); }
  async mkdir(path) { this.directories.add(path); }
  async list(path) {
    const prefix = path === "/" ? "/" : `${path}/`;
    return [...this.files.entries()]
      .filter(([name]) => name.startsWith(prefix) && !name.slice(prefix.length).includes("/"))
      .map(([name, value]) => ({ name: name.slice(prefix.length), kind: "file", size: value.length }));
  }
}

const cachedLocal = new MemoryWorkspace({
  "/README.md": "cached contents",
  "/.shenmux-etags.json": JSON.stringify({ "/README.md": '"cache-v0"' }),
});
let validatedETag = "";
const hostWrites = [];
const cachedRemote = {
  async list() { return [{ name: "README.md", kind: "file", size: 15 }]; },
  async readAll(_path, options) {
    validatedETag = options.etag;
    return { bytes: new Uint8Array(), etag: '"cache-v0"', notModified: true, complete: true };
  },
  async write(path, data, options) {
    hostWrites.push({ path, contents: new TextDecoder().decode(data), etag: options.etag });
    return '"cache-v1"';
  },
  async mkdir() {},
};
const cachedHost = new CachedWorkspaceHost(cachedLocal, cachedRemote);
await cachedHost.init();
assert.equal(new TextDecoder().decode(await cachedHost.loadFile("/README.md")), "cached contents");
assert.equal(validatedETag, '"cache-v0"');
cachedHost.write("/README.md", new TextEncoder().encode("updated contents"), true);
await cachedHost.commit();
await cachedHost.flush();
assert.deepEqual(hostWrites, [{ path: "/README.md", contents: "updated contents", etag: '"cache-v0"' }]);
assert.equal(cachedHost.etags.get("/README.md"), '"cache-v1"');
assert.match(await cachedLocal.read("/.shenmux-etags.json"), /cache-v1/);

const emptyRemoteHost = new CachedWorkspaceHost(new MemoryWorkspace(), {
  async list() { return []; },
});
await emptyRemoteHost.init();
assert.deepEqual(emptyRemoteHost.listEntries("/"), []);
console.log("workspace runtime smoke test passed");
