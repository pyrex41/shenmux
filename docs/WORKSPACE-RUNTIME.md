# Browser workspace runtime (V3 experimental)

`/workspace` is a local-first filesystem client. Its command semantics run in
the Rust `wasm32-wasip2` component, while persistence and synchronization stay
in a host adapter. The component never sees OPFS, HTTP, S3, or bearer tokens.

## Runtime boundary

The WIT world imports a synchronous host filesystem:

```text
list-entries(path) -> entry[]
read(path, offset, length) -> bytes
write(path, bytes, truncate)
mkdir(path)
```

The browser prepares an in-memory window before invoking WASI, because OPFS and
HTTP APIs are asynchronous. The host commits dirty entries after the command.
JCO's generated imports are native JS arrays/bytes; `workspace-filesystem.js`
is the tiny adapter that connects those imports to the prepared cache.

## OPFS-first cache and remote sync

`workspace-runtime.js` provides:

- OPFS persistence for files and a journal, with a seeded README/notes demo;
- lazy remote directory manifests and HTTP `Range` reads for cold files;
- local writes first, followed by an append-only, idempotent write-back journal;
- `If-Match` ETags and explicit `conflict`/`offline` status instead of silent
  overwrite; and
- a storage-neutral `RemoteObjectBackend` that sends only a short-lived
  capability bearer to shenmux, never an S3 credential.

The optional Go handler is mounted at `/workspace-objects`. It authorizes every
operation (`list`, `read`, `write`, `mkdir`) against the opaque capability and
then calls a `workspace.Store`. `LocalStore` is safe for a single node; an S3,
Fly volume, or home-server implementation can satisfy the same interface.

Example wiring:

```go
store := workspace.NewLocalStore("/var/lib/shenmux/workspaces/alice")
server := webgateway.New(ctx, webgateway.Config{
    WorkspaceStore: store,
    WorkspaceAuthorize: func(cap, op, path string) bool {
        return cap == issuedCapability && strings.HasPrefix(path, "/notes")
    },
})
```

The browser can opt into it by setting `globalThis.__SHENMUX_WORKSPACE_REMOTE__`
to `{ baseURL: "/workspace-objects", capability: "..." }`. Omitting that
configuration keeps the entire path offline/local.

## Golem durability seam

`runtime/golem-workspace` is an opt-in Golem 1.5 Rust agent. Its durable state
is intentionally small: session identity, cwd, command watermark, and a
monotonic sync sequence. `request_sync` and `flush_sync` are the background-job
seam; Golem's durable replay (with a future custom snapshot policy) protects
the metadata while the object service transfers bytes. It is built separately with `make golem-workspace-build` and
does not alter the browser's fast path.

## Development

```sh
make workspace-test
go test ./internal/workspace
# with shenmux-web running and Chrome remote debugging on :9223:
node scripts/test-workspace-browser.mjs
```

The normal Go/web build still produces one `shenmux-web` binary with the
workspace assets embedded.
