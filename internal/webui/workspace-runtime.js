// Storage/runtime seams shared by the browser UI and Node tests. The WASI
// component itself remains storage-agnostic; this module owns remote protocol,
// cache journal, and path/encoding details.
export const MAX_READ = 16 * 1024 * 1024;

export function normalizePath(path, base = "/") {
  const raw = String(path || "");
  const joined = raw.startsWith("/") ? raw : `${base}/${raw}`;
  const parts = [];
  for (const part of joined.split("/")) {
    if (!part || part === ".") continue;
    if (part === "..") parts.pop(); else parts.push(part);
  }
  return `/${parts.join("/")}`;
}

export function parseCommand(text) {
  const args = [];
  let current = "";
  let quote = "";
  let escaped = false;
  for (const char of text) {
    if (escaped) { current += char; escaped = false; continue; }
    if (char === "\\" && quote !== "'") { escaped = true; continue; }
    if ((char === "'" || char === '"') && (!quote || quote === char)) { quote = quote ? "" : char; continue; }
    if (/\s/.test(char) && !quote) {
      if (current) { args.push(current); current = ""; }
    } else current += char;
  }
  if (escaped) current += "\\";
  if (current || quote) args.push(current);
  return args;
}

export class RemoteConflictError extends Error {
  constructor(message) { super(message); this.name = "RemoteConflictError"; }
}

function parseContentRange(value) {
  const match = String(value || "").match(/^bytes (\d+)-(\d+)\/(\d+)$/);
  if (!match) return null;
  const start = Number(match[1]);
  const end = Number(match[2]);
  const total = Number(match[3]);
  if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || !Number.isSafeInteger(total)
      || start < 0 || end < start || total <= end) return null;
  return { start, end, total };
}

export class RemoteObjectBackend {
  constructor({ baseURL, capability, fetchImpl = globalThis.fetch.bind(globalThis) }) {
    this.baseURL = String(baseURL || "").replace(/\/$/, "");
    this.capability = capability;
    this.fetch = fetchImpl;
    if (!this.baseURL || !this.capability) throw new Error("remote workspace requires a base URL and capability");
  }

  headers(extra = {}) { return { Authorization: `Bearer ${this.capability}`, ...extra }; }

  async request(path, init = {}) {
    const response = await this.fetch(`${this.baseURL}${path}`, { ...init, headers: this.headers(init.headers) });
    if (response.ok || response.status === 304) return response;
    if (response.status === 409) throw new RemoteConflictError("remote workspace version conflict");
    const body = await response.text().catch(() => "");
    throw new Error(`remote workspace ${response.status}: ${body || response.statusText}`);
  }

  async list(path = "/") {
    const response = await this.request(`/manifest?path=${encodeURIComponent(normalizePath(path))}`);
    const payload = await response.json();
    return Array.isArray(payload.entries) ? payload.entries : [];
  }

  async read(path, offset = 0, length = MAX_READ, { etag = "" } = {}) {
    const end = Math.max(offset, offset + Math.max(1, length) - 1);
    const headers = { Range: `bytes=${offset}-${end}` };
    if (etag) headers["If-None-Match"] = etag;
    const response = await this.request(`/objects?path=${encodeURIComponent(normalizePath(path))}`, {
      headers,
    });
    if (response.status === 304) {
      return { bytes: new Uint8Array(), etag: response.headers.get("ETag") || etag, notModified: true, complete: true };
    }
    const bytes = new Uint8Array(await response.arrayBuffer());
    const contentRange = parseContentRange(response.headers.get("Content-Range"));
    if (response.status === 206 && bytes.length && (!contentRange || contentRange.start !== offset)) {
      throw new Error("remote workspace returned an invalid content range");
    }
    return {
      bytes,
      etag: response.headers.get("ETag") || "",
      notModified: false,
      complete: response.status !== 206 || bytes.length === 0 || contentRange.end + 1 >= contentRange.total,
      total: contentRange?.total,
    };
  }

  async readAll(path, { etag = "" } = {}) {
    const chunks = [];
    let offset = 0;
    let version = "";
    let total;
    while (true) {
      const result = await this.read(path, offset, MAX_READ, { etag: offset === 0 ? etag : "" });
      if (result.notModified) return result;
      if (!version) version = result.etag;
      else if (version && result.etag && version !== result.etag) {
        throw new RemoteConflictError("remote workspace changed while it was being read");
      }
      chunks.push(result.bytes);
      offset += result.bytes.length;
      total = result.total ?? total;
      if (result.complete) break;
    }
    const bytes = new Uint8Array(total ?? offset);
    let cursor = 0;
    for (const chunk of chunks) {
      bytes.set(chunk, cursor);
      cursor += chunk.length;
    }
    return { bytes, etag: version, notModified: false, complete: true, total: total ?? offset };
  }

  async write(path, bytes, { etag = "", operationID = "" } = {}) {
    const headers = { "Content-Type": "application/octet-stream" };
    if (etag) headers["If-Match"] = etag;
    if (operationID) headers["Idempotency-Key"] = operationID;
    const response = await this.request(`/objects?path=${encodeURIComponent(normalizePath(path))}`, {
      method: "PUT", headers, body: bytes,
    });
    return response.headers.get("ETag") || "";
  }

  async mkdir(path, operationID = "") {
    const headers = { "Content-Type": "application/json" };
    if (operationID) headers["Idempotency-Key"] = operationID;
    await this.request(`/directories?path=${encodeURIComponent(normalizePath(path))}`, { method: "POST", headers });
  }
}

export const bytesToBase64 = (bytes) => {
  let binary = "";
  for (let i = 0; i < bytes.length; i += 0x8000) binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(binary);
};
export const base64ToBytes = (value) => Uint8Array.from(atob(value), (char) => char.charCodeAt(0));

export class WorkspaceJournal {
  constructor(local) { this.local = local; this.records = []; this.flushing = null; }

  async load() {
    try { this.records = JSON.parse(await this.local.read("/.shenmux-write-journal.json")); }
    catch (_) { this.records = []; }
    if (!Array.isArray(this.records)) this.records = [];
  }

  async save() { await this.local.write("/.shenmux-write-journal.json", JSON.stringify(this.records)); }

  async append(record) {
    this.records.push(record);
    await this.save();
  }

  async flush(remote) {
    if (!remote) return { pending: this.records.length, conflict: false, etags: {} };
    if (this.flushing) return this.flushing;
    if (!this.records.length) return { pending: 0, conflict: false, etags: {} };
    this.flushing = this.drain(remote).finally(() => { this.flushing = null; });
    return this.flushing;
  }

  async drain(remote) {
    const etags = {};
    let conflict = false;
    while (this.records.length) {
      const result = await this.flushRecords(remote);
      Object.assign(etags, result.etags);
      conflict ||= result.conflict;
      if (result.failures || !result.completed) break;
    }
    return { pending: this.records.length, conflict, etags };
  }

  async flushRecords(remote) {
    const records = [...this.records];
    const completed = new Set();
    const failedPaths = new Set();
    const etags = new Map();
    let conflict = false;
    let failures = 0;
    for (const record of records) {
      if (failedPaths.has(record.path)) {
        continue;
      }
      try {
        if (record.op === "mkdir") {
          await remote.mkdir(record.path, record.id);
        } else {
          const nextETag = await remote.write(record.path, base64ToBytes(record.data), {
            etag: etags.get(record.path) || record.etag,
            operationID: record.id,
          });
          if (nextETag) etags.set(record.path, nextETag);
        }
        completed.add(record.id);
      } catch (error) {
        failedPaths.add(record.path);
        failures++;
        conflict ||= error instanceof RemoteConflictError;
      }
    }
    this.records = this.records.filter((record) => !completed.has(record.id));
    for (const record of this.records) {
      const nextETag = etags.get(record.path);
      if (record.op === "write" && nextETag) record.etag = nextETag;
    }
    await this.save();
    return { conflict, etags: Object.fromEntries(etags), completed: completed.size, failures };
  }
}
