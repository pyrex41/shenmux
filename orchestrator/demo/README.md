# Orchestrator demo

`./demo/demo.sh` runs the Kubernetes agent-orchestration architecture from
[`docs/K8S-ORCHESTRATION.md`](../../docs/K8S-ORCHESTRATION.md) as local OS
processes instead of pods. It is self-contained: no root, no network beyond
loopback, idempotent, and it exits non-zero if any assertion fails.

## Run it

```sh
cd orchestrator
./demo/demo.sh
```

The script builds every component into `bin/`, then narrates six sections and
ends with a `DEMO OK` banner (or a non-zero exit and a `FAIL` line).

## What it proves

Each section makes a **substantive**, asserted claim — not a scripted echo.

| # | Section | Claim asserted |
| - | ------- | -------------- |
| 0 | Build | all cmds (`secret-proxy`, `mock-upstream`, `snapshot`, `muxwork`, `mock-agent`) build; `bin/shenmux` present or a fallback backend is used |
| 1 | Mock upstream | the "provider API" echo server is listening on loopback |
| 2 | **Secret interception** | for an *allow-listed* host the upstream receives the **real** secret; for a *blocked* host it receives only the **placeholder**; the agent only ever held the placeholder |
| 3 | **Base worker** | a real worker (shenmux PTY + secret-proxy + mock harness) performs the placeholder→real swap in flight — the agent holds the placeholder, the upstream reports a different, non-placeholder value |
| 4 | **Checkpoint + fork** | `base` is checkpointed on suspend, then forked three ways (`alpha`/`beta`/`gamma`) as O(delta) restores |
| 5 | **Divergence** | the three forks' `notes.md`/`progress.txt` are **not identical**; `snapshot diff` shows the checkpoint trees changed |
| 6 | Fleet view | `muxwork list` shows the fork tree and `muxwork watch --json` emits the lifecycle event stream |

### The interception check, precisely

The standalone interception test (section 2) is fully controlled by the demo,
so its assertions pin exact strings. It writes a `secrets.json` whose
`allowed_hosts` is `["127.0.0.1"]` and drives requests through the
`secret-proxy` with `mock-agent`:

- **allowed** — target `http://127.0.0.1:8091/`. `127.0.0.1` matches
  `allowed_hosts`, so the proxy swaps in the real value. Assert the upstream
  echoes `sk-REAL-anthropic-value`.
- **blocked** — target `http://127.0.0.2:8091/`. `127.0.0.2` is a loopback
  alias that is *not* allow-listed, so the placeholder passes through
  untouched. Assert the real value never appears; when the alias is reachable,
  assert the upstream echoed exactly the placeholder.

The upstream binds `0.0.0.0:8091` so both loopback aliases reach the same echo
server; if `127.0.0.2` is unreachable on a given host the blocked check falls
back to its weaker form (real value must be absent), and the demo still passes.

## Architecture → process mapping

| Kubernetes design | This demo |
| ----------------- | --------- |
| Worker **Pod** (main + sidecars) | a set of local processes under `state/workers/NAME/` |
| main container `shenmux run -- <harness>` | `bin/shenmux run … -- bin/mock-agent`, or a bare-PTY/tmux fallback |
| harness (codex/pi/opencode) | `bin/mock-agent` — a scripted, env-driven stand-in |
| secret-proxy **sidecar** (sole egress) | `bin/secret-proxy` on a loopback port per worker |
| OpenBao-sourced real secret | inline `value` in the worker's `secrets.json` |
| provider API (`api.anthropic.com`) | `bin/mock-upstream` echoing the auth header it saw |
| overlay checkpoint + object store | `bin/snapshot` content-addressed store + s-expr manifest |
| the **operator** reconciling `AgentWorker` CRs | `bin/muxwork` managing worker processes |
| CR **watch stream** | `muxwork watch --json` / each worker's `events.ndjson` |

`worker = shenmux + secret-proxy + harness`; `muxwork = operator`;
`snapshot = the checkpoint DAG`.

## Real vs simulated

**Real** (the load-bearing mechanisms):

- the shenmux PTY session wrapping the harness (when `bin/shenmux` is present);
- MITM credential interception — the actual placeholder→real swap, host-gated
  by `allowed_hosts`, in auth headers only;
- content-addressed checkpoint + O(delta) fork with autopoiesis-compatible
  manifests, and a real tree diff between checkpoints;
- the lifecycle event stream.

**Simulated** (deliberately out of scope for a laptop):

- pods → local processes (no kubelet, no NetworkPolicy L3/L4 enforcement — the
  proxy is sole egress here only by how the harness is configured, not by the
  kernel);
- OpenBao → the real value is inlined in `secrets.json` instead of fetched from
  a secret store with a ServiceAccount JWT;
- Karpenter (node autoscaling) and Crossplane (environment stamping) are **not
  exercised** at all;
- `claude-managed` harness (loop on Anthropic's side) is not part of the demo —
  only the CLI-harness shape is.
