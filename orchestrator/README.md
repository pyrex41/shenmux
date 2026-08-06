# shenmux orchestrator (local demo)

A standard-library-only Go module that runs the shenmux Kubernetes
agent-orchestration design ([`docs/K8S-ORCHESTRATION.md`](../docs/K8S-ORCHESTRATION.md))
as **local OS processes** — a working, laptop-scale proof of the loop that the
Kubernetes operator would run at fleet scale.

A **worker** is a `shenmux run` PTY session wrapping a harness process, fronted
by a `secret-proxy` as its sole egress, with its filesystem state captured as
content-addressed overlay checkpoints. The `muxwork` CLI is the local operator
that spawns, suspends, resumes, and forks those workers.

> Module path: `github.com/pyrex41/shenmux/orchestrator`. No third-party
> dependencies — Go standard library only, so `go.mod`/`go.sum` stay static.
> The frozen inter-component interface is [`CONTRACTS.md`](CONTRACTS.md).

## Quick start

```sh
cd orchestrator
./demo/demo.sh          # builds everything into bin/, then proves the loop
```

See [`demo/README.md`](demo/README.md) for exactly what each section asserts
and how it maps back to the Kubernetes design. The demo is idempotent, needs no
root, and uses loopback only.

## Components

Every binary builds into `orchestrator/bin/` via `go build -o bin/ ./cmd/...`.

| Component | cmd | Role in the demo |
| --------- | --- | ---------------- |
| **secret-proxy** | `cmd/secret-proxy` | MITM egress proxy that swaps a placeholder for the real secret, only in auth headers and only for allow-listed hosts (`CONTRACTS.md` §1). |
| **snapshot** | `cmd/snapshot` | Content-addressed overlay checkpoints with autopoiesis-compatible manifests: `scan`/`checkpoint`/`restore`/`fork`/`diff` (§2). |
| **muxwork** | `cmd/muxwork` | The orchestration CLI *and* the local operator — manages worker processes instead of pods: `spawn`/`list`/`status`/`suspend`/`resume`/`fork`/`logs`/`watch`/`gc` (§3). |
| **mock-agent** | `cmd/mock-agent` | A scripted, env-driven harness that proves the whole loop without a real LLM key (§4). |
| **mock-upstream** | `cmd/mock-upstream` | A tiny echo server standing in for the provider API; reports the auth header it received so the swap is visible (§4). |

### mock-agent (this package: `mockagent`)

A stand-in for a real coding-agent CLI (codex/pi/opencode) running inside the
worker's PTY. Configured entirely from the environment:

| Env | Meaning | Default |
| --- | ------- | ------- |
| `ANTHROPIC_API_KEY` | the **placeholder** the agent holds | `sk-placeholder-anthropic` |
| `HTTPS_PROXY` / `HTTP_PROXY` | the secret-proxy to route through | direct |
| `WORKSPACE` | harness working dir (`notes.md`, `progress.txt`) | `.` |
| `MOCK_TARGET` | URL to GET each step | `http://127.0.0.1:9` |
| `MOCK_STEPS` | iterations | `3` |
| `WORKER` | worker identity in banners/notes | hostname |
| `SSL_CERT_FILE` / `NODE_EXTRA_CA_CERTS` | CA to trust for the MITM proxy | system roots |

Each step it prints a banner, GETs `MOCK_TARGET` with
`Authorization: Bearer $ANTHROPIC_API_KEY` through the proxy, and prints:

```
=== mock-agent step 1/3 worker=base target=http://127.0.0.1:8091/ ===
agent-holds: sk-placeholder-anthropic
upstream-received: sk-REAL-anthropic-value
```

`agent-holds` is the placeholder the agent sent; `upstream-received` is what the
provider actually saw — the visible proof that the proxy swapped the value the
agent never knew. It then appends to `WORKSPACE/notes.md` and rewrites
`WORKSPACE/progress.txt` so forked workspaces diverge, sleeps ~300ms, and after
`MOCK_STEPS` iterations prints `DONE <worker>` and exits 0. Unreachable targets
are reported and the loop continues.

Note: the harness routes loopback targets through the configured proxy on
purpose. `http.ProxyFromEnvironment` silently *excludes* loopback, which would
bypass the proxy in an all-localhost demo, so the client's `Proxy` is set to the
configured URL for every request.

## How this maps to the Kubernetes design

`worker = shenmux + secret-proxy + harness` · `muxwork = the operator` ·
`snapshot = the content-addressed checkpoint DAG` · `events.ndjson /
muxwork watch = the CR watch stream`.

**Real** here: the shenmux PTY, MITM credential interception, content-addressed
checkpoint/fork, and the event stream. **Simulated**: pods → processes, OpenBao
→ inline `value`, and Karpenter/Crossplane are not exercised. The full table is
in [`demo/README.md`](demo/README.md).

## Layout

```
orchestrator/
├── CONTRACTS.md            frozen inter-component interface
├── cmd/{secret-proxy,snapshot,muxwork,mock-agent,mock-upstream}/
├── secretproxy/  snapshot/  muxwork/  operator/  mockagent/   packages
├── demo/demo.sh            the runnable demonstration
└── bin/                    build output (git-ignored)
```
