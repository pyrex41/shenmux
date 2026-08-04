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
    if (response.ok) return response;
    if (response.status === 409) throw new RemoteConflictError("remote workspace version conflict");
    const body = await response.text().catch(() => "");
    throw new Error(`remote workspace ${response.status}: ${body || response.statusText}`);
  }

  async list(path = "/") {
    const response = await this.request(`/manifest?path=${encodeURIComponent(normalizePath(path))}`);
    const payload = await response.json();
    return Array.isArray(payload.entries) ? payload.entries : [];
  }

  async read(path, offset = 0, length = MAX_READ) {
    const end = Math.max(offset, offset + Math.max(1, length) - 1);
    const response = await this.request(`/objects?path=${encodeURIComponent(normalizePath(path))}`, {
      headers: { Range: `bytes=${offset}-${end}` },
    });
    return { bytes: new Uint8Array(await response.arrayBuffer()), etag: response.headers.get("ETag") || "" };
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
  constructor(local) { this.local = local; this.records = []; }

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
    if (!remote || !this.records.length) return { pending: this.records.length, conflict: false };
    const remaining = [];
    let conflict = false;
    for (const record of this.records) {
      try {
        if (record.op === "mkdir") await remote.mkdir(record.path, record.id);
        else await remote.write(record.path, base64ToBytes(record.data), { etag: record.etag, operationID: record.id });
      } catch (error) {
        remaining.push(record);
        conflict ||= error instanceof RemoteConflictError;
      }
    }
    this.records = remaining;
    await this.save();
    return { pending: remaining.length, conflict };
  }
}
