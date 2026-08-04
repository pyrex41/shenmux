# Golem workspace agent (experimental)

This is an opt-in Golem 1.5 Rust component. It is deliberately separate from
the browser WASI component and is not part of the default `make build` target.

`WorkspaceAgent` holds only durable coordination metadata: session identity,
cwd, command counters, and a journal/sync watermark. The browser remains the
fast local filesystem client, while `request_sync` and `flush_sync` are the
seam for a scheduled background object-sync worker. Golem's normal durable
agent replay protects this metadata (custom snapshotting can be added when the
watermark grows); object bytes are
still transferred through the capability-scoped `/workspace-objects` service.

With the Golem CLI installed:

```sh
cd runtime/golem-workspace
golem build --yes
golem server
```

The generated component is intentionally not invoked by the existing web
gateway yet. That keeps the experimental durability host from changing the
local/offline browser path while the object-sync protocol stabilizes.
