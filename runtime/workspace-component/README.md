# shenmux workspace WASI component

This is a real WebAssembly Component Model guest. It exposes a small typed WIT
interface and imports a host virtual filesystem. The guest owns deterministic
command semantics; the host owns persistence, caching, capabilities, and sync.
That makes it usable in three places without changing command semantics:

- a browser host that caches files in OPFS and journals remote writes;
- a Tauri/native host that persists it on disk; and
- a Golem wrapper that keeps durable metadata and schedules sync separately.

Build it with:

```sh
rustup target add wasm32-wasip2
cargo build --release --target wasm32-wasip2
```

The component artifact is emitted under `target/wasm32-wasip2/release/`.
JCO transpiles it for the browser; see `make workspace-test`. Golem is kept as
an opt-in adapter under `runtime/golem-workspace`, independent of this guest's
filesystem imports and private oplog.
