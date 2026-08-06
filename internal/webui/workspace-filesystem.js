// Synchronous WIT imports for the WASI workspace component. Browser storage is
// asynchronous, so workspace.js prepares an in-memory window before invoking
// the component and commits dirty pages afterward.
let host = null;

export function setWorkspaceFilesystem(next) { host = next; }

function unavailable() {
  throw new Error("workspace filesystem host is not configured");
}

export function listEntries(path) {
  if (!host) return unavailable();
  return host.listEntries(path).map((entry) => ({ ...entry, size: BigInt(entry.size || 0) }));
}

export function read(path, offset, length) {
  return host ? host.read(path, offset, length) : unavailable();
}

export function write(path, data, truncate) {
  if (!host) return unavailable();
  host.write(path, data, truncate);
}

export function mkdir(path) {
  if (!host) return unavailable();
  host.mkdir(path);
}
