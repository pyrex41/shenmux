# Demo component contracts

This is the frozen interface between the demo components so they can be built
in parallel. **All code is Go standard library only — no third-party
dependencies** (keeps `go.mod`/`go.sum` static and conflict-free). Every
binary builds into `orchestrator/bin/`. Do not change these signatures
without updating this file.

The demo runs the Kubernetes agent-orchestration architecture
(`docs/K8S-ORCHESTRATION.md`) as local OS processes instead of pods: a worker
is a `shenmux run` PTY session wrapping a harness process, fronted by a
secret-proxy as its sole egress, with state captured as content-addressed
overlay checkpoints.

## 1. `secret-proxy` (package ./secretproxy, cmd ./cmd/secret-proxy)

A MITM HTTPS proxy that swaps a placeholder token for a real secret only in
auth headers and only for allow-listed hosts. Adapted from sq-sandbox
`impl/go/proxy/proxy.go`, stdlib only.

```
secret-proxy --listen 127.0.0.1:PORT --secrets FILE --ca-dir DIR [--log FILE]
```
- `--secrets FILE`: JSON, schema below. `--ca-dir DIR`: holds `ca.crt`/`ca.key`;
  auto-generated (P-256 ECDSA CA) if absent. Prints the CA path to stdout so
  callers can trust it.
- Behavior: plain HTTP → rewrite auth headers, forward. HTTPS CONNECT to an
  allowed host → MITM with a per-host cert signed by the CA, rewrite auth
  headers. CONNECT to any other host → blind TCP tunnel (no inspection).
- Replacement scans ONLY these headers: Authorization, X-Api-Key, Api-Key,
  X-Auth-Token, X-Access-Token, Proxy-Authorization (and HTTP Basic within
  Authorization). Never the body. Replace only when the destination host
  matches that secret's allowed_hosts (suffix match: `.example.com` matches
  `api.example.com`; exact host also matches). On block, log and pass the
  placeholder through unchanged.

secrets.json:
```json
{ "secrets": {
    "ANTHROPIC_API_KEY": {
      "placeholder": "sk-placeholder-anthropic",
      "value": "REAL-VALUE",              // demo: inline. prod: omitted, use bao_ref
      "bao_ref": "secret/data/harness/anthropic#api_key",
      "allowed_hosts": ["api.anthropic.com", ".example.com"]
    } } }
```
For the demo `value` is inline; `bao_ref` is documented but unused in demo
mode. Log each action as `replaced <name> for <host>` / `blocked <name> host
<host> not allowed`, never the value.

## 2. `snapshot` (package ./snapshot, cmd ./cmd/snapshot)

Content-addressed overlay checkpoints, autopoiesis-compatible manifest.

```
snapshot scan       --dir DIR                                  # prints tree entries + tree-hash
snapshot checkpoint --workspace DIR --store STORE --manifest OUT [--parent ID]
snapshot restore    --manifest FILE --store STORE --into DIR
snapshot fork       --manifest FILE --store STORE --into DIR   # restore under new id, prints new manifest path
snapshot diff       --a MANIFEST --b MANIFEST                  # added/removed/modified
```
- Tree entry: `(:file "path" :hash SHA256 :mode MODE :size N :mtime T)` sorted
  by path. Tree hash = SHA-256 over concatenated canonical strings
  `F:path:hash:mode:size` (mtime excluded), matching autopoiesis
  `filesystem-tree.lisp`.
- Manifest = an s-expression written to disk, superset of autopoiesis
  `snapshot-to-sexpr`:
  `(snapshot :version 1 :id UUID :timestamp UNIX :parent ID|nil
    :agent-state nil :metadata (:tool "shenmux-orchestrator")
    :hash SHA256-of-agent-state :tree-root SHA256 :tree-entries (...)
    :upperdir-archive SHA256)`.
  The extra `:tree-root`/`:tree-entries`/`:upperdir-archive` are the superset
  fields (autopoiesis drops tree state on serialize).
- Store = content-addressed dir: blobs by sha256, plus the upperdir archive
  (tar.gz of the delta) by sha256. `checkpoint` captures the delta (whole
  workspace in demo, since we may not have a live overlay upper). `fork`
  restores into a fresh dir under a new UUID whose `:parent` is the source.
- Overlay: if `snapshot try-overlay` can `unshare -Urm` mount an overlayfs,
  use the upperdir as the delta; otherwise fall back to hashing the whole
  workspace tree. Demo must work either way.

## 3. `muxwork` (package ./muxwork + ./operator, cmd ./cmd/muxwork)

The orchestration CLI AND the local "operator" (demo mode manages worker OS
processes instead of pods). Single binary.

```
muxwork --state DIR spawn  --name NAME --harness HARNESS [--prompt P] [--from NAME|--checkpoint MANIFEST]
muxwork --state DIR list                       # table: name harness phase session pid
muxwork --state DIR status --name NAME         # JSON status
muxwork --state DIR suspend --name NAME        # checkpoint + stop processes, keep state
muxwork --state DIR resume  --name NAME        # restart from last checkpoint
muxwork --state DIR fork    --name NAME --from SRC [--prompt P]   # snapshot fork src state -> new worker
muxwork --state DIR logs    --name NAME        # tail the worker session log
muxwork --state DIR watch [--json]             # stream lifecycle events (resumable feed)
muxwork --state DIR gc                         # remove completed workers past ttl
```
- A worker's on-disk state under `STATE/workers/NAME/`: `workspace/` (the
  harness's working dir), `home/` (HOME for the harness), `secrets.json`,
  `proxy-ca/`, `manifests/` (checkpoint chain), `session.log`, `status.json`,
  `events.ndjson`.
- spawn launches, in order: (1) `secret-proxy` on a free loopback port with
  the worker's secrets.json + proxy-ca; (2) the worker process = `bin/shenmux
  run --session NAME --history-dir .../shenmux-history -- <harness-cmd>` with
  env `HTTPS_PROXY`, CA trust vars, and the placeholder key pointed at the
  proxy. If `bin/shenmux` is missing, fall back to running the harness under
  a bare PTY/`tmux new-session -d` so the demo still runs; record which path
  was used in status.
- `--harness mock` uses `bin/mock-agent`. Other harness names map to a
  command via a small built-in table (codex/pi/opencode → their CLI if on
  PATH, else mock with a warning).
- fork: call `snapshot fork` on SRC's latest manifest into the new worker's
  workspace, set `:parent`, then spawn. Must be O(delta) restore, not a live
  copy of a running worker (fork suspended workers in the demo).
- Events (`events.ndjson`, also emitted by `watch`): one JSON object per line
  `{"ts":UNIX,"worker":NAME,"event":"spawned|running|checkpointed|suspended|resumed|forked|completed|failed","detail":...}`.
  This is the demo's stand-in for the CR watch stream.

## 4. `mock-agent` (package ./mockagent, cmd ./cmd/mock-agent)

A scripted harness that proves the whole loop without a real LLM key.
Env-driven: `ANTHROPIC_API_KEY` (placeholder), `HTTPS_PROXY`, `WORKSPACE`,
`MOCK_TARGET` (URL to call, default the demo's mock upstream), `MOCK_STEPS`.
Behavior each run:
1. Print a banner to stdout (this becomes the shenmux PTY session content).
2. Read its `ANTHROPIC_API_KEY` and make an HTTPS GET to `MOCK_TARGET` with
   `Authorization: Bearer $ANTHROPIC_API_KEY` through `HTTPS_PROXY`; print
   what the upstream reports it received (proving the swap) vs. what the agent
   holds (the placeholder).
3. Append a line to `WORKSPACE/notes.md` and write/increment
   `WORKSPACE/progress.txt` (so checkpoints/forks visibly diverge).
4. Sleep briefly, loop `MOCK_STEPS` times, then exit 0.

Also provide `bin/mock-upstream` (cmd ./cmd/mock-upstream): a tiny HTTPS
server (self-signed, or plain HTTP for the allowed-host demo) that echoes the
`Authorization` header value it received as JSON `{"received_auth":"..."}` so
the demo can show the real key arriving at an allowed host and the placeholder
at a blocked one. It must NOT print the value to any shared log.

## Demo entrypoint

`orchestrator/demo/demo.sh` builds everything into `bin/`, then narrates:
spawn a base mock worker → show secret interception (allowed vs blocked host)
→ checkpoint → fork it 3 ways → run forks so their workspaces diverge →
`muxwork list` + `snapshot diff` showing the fork tree → attach output from
the shenmux session. Idempotent, no root, no network required beyond
localhost. Exit non-zero on any failed assertion.
