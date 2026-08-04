import assert from "node:assert/strict";
import { RemoteConflictError, RemoteObjectBackend, WorkspaceJournal, bytesToBase64, base64ToBytes } from "../internal/webui/workspace-runtime.js";

const calls = [];
const response = (body, { status = 200, headers = {} } = {}) => ({
  ok: status >= 200 && status < 300,
  status,
  statusText: "error",
  headers: new Headers(headers),
  async json() { return body; },
  async arrayBuffer() { return new TextEncoder().encode(body).buffer; },
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
assert.deepEqual(await journal.flush(remote), { pending: 0, conflict: false });
assert.deepEqual(flushed[0].data, [7]);
assert.equal(journal.records.length, 0);

await journal.append({ id: "offline-1", op: "write", path: "/offline", data: bytesToBase64(new Uint8Array([8])), etag: "" });
assert.deepEqual(await journal.flush({ async write() { throw new Error("network down"); } }), { pending: 1, conflict: false });
assert.equal(journal.records.length, 1);
console.log("workspace runtime smoke test passed");
