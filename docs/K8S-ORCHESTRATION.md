# Kubernetes agent orchestration design

## Status

This is a design document for a Kubernetes orchestration layer that runs
coding-agent workers observable and controllable through shenmux. Nothing in
this document is implemented unless it links to code in this repository. It
builds on the implemented controller/agent path in
[ARCHITECTURE.md](ARCHITECTURE.md), the durable-history direction in
[SESSION-HISTORY.md](SESSION-HISTORY.md), and the production gates in
[V1-V2-PLAN.md](V1-V2-PLAN.md).

## Goal

Run a fleet of coding-agent **workers** on Kubernetes, where each worker is:

- **durable** — its workspace and conversation state survive pod deletion;
- **resumable** — a suspended worker can be brought back and continue its
  conversation where it stopped;
- **forkable** — a worker can be cloned at a point in time into a new worker
  that diverges independently;
- **observable and steerable** — every worker's terminal is a shenmux
  session, attachable from the controller workspace, with control leases for
  human takeover.

Workers run one of several **harnesses**:

| Harness | Loop location | Pod process | Interactive |
| --- | --- | --- | --- |
| `claude-managed` | Anthropic's servers (managed agents) | Anthropic runner executing tool calls | No (headless runner) |
| `codex` | In pod | `codex` TUI or `codex exec` | Yes |
| `pi` | In pod | `pi` TUI or `pi -p` | Yes |
| `opencode` | In pod | `opencode` TUI or `opencode run` | Yes |

The split matters. `claude-managed` follows the Sprites-style work-queue
model: Anthropic hosts the model loop and enqueues sessions; our side claims
work items and provides an isolated execution environment. The other three are
CLI agents whose loop runs inside the pod, and whose TUI **is** the natural
interaction surface — which is exactly what shenmux relays.

shenmux already carries the vocabulary for this: the agent advertises
`--harness` ("codex, claude, pi, or custom"), `--workload`, `--pod`,
`--namespace`, and `--orchestrator` metadata
(`cmd/shenmux/main.go`), and the relay identity model includes
`Harness` and `Orchestrator` fields (`internal/relay/identity.go`).

## Worker pod shape

Every worker, regardless of harness, is the same three-part pod. This extends
the sidecar sketch in
[`deploy/kubernetes/agent-sidecar.yaml`](../deploy/kubernetes/agent-sidecar.yaml):

```text
per-worker Pod
├─ main container:
│    shenmux run --session $WORKER_NAME \
│                --history-dir /state/shenmux/history \
│                -- <harness command>
├─ sidecar container:
│    shenmux agent --controller https://mux.example.com \
│                  --transport relay \
│                  --harness <harness> --workload <worker> \
│                  --namespace $(POD_NAMESPACE) --pod $(POD_NAME) \
│                  --session $WORKER_NAME
├─ volume: state    (PVC, per worker — the durable identity)
└─ volume: shenmux-ipc (emptyDir shared by both containers)
```

Rules:

- The harness process runs **inside** the PTY owned by `shenmux run`. That is
  what makes the worker's terminal a durable, attachable session rather than
  logs. For `claude-managed` the runner is headless, so the session is an
  observation surface; for the CLI harnesses it is the full interaction
  surface — attach, take control, type into the agent, release control.
- One state volume per worker, mounted at `/state` (backed by a PVC or an
  overlayfs merged mount — see "State layers" below). Everything that defines
  the worker lives under it, so suspend/resume/fork are operations on exactly
  one tree:

```text
/state
├── workspace/        the working tree (repos, artifacts)
├── home/             harness home; state dirs live here via $HOME=/state/home
│   ├── .codex/       codex sessions (rollout files), auth
│   ├── .pi/          pi agent sessions
│   └── .local/share/opencode/   opencode sessions, auth
└── shenmux/
    └── history/      durable terminal archive (shenmux run --history-dir)
```

- The agent sidecar dials **out** to the shenmux controller. No inbound
  connections to worker pods, no `kubectl exec` as the access path.

## Harness abstraction

A harness is a small static profile the orchestrator consumes:

```yaml
harness:
  name: codex
  image: ghcr.io/pyrex41/shenmux-worker-codex:latest
  command: ["codex"]                      # interactive default
  runArgs: ["exec", "$(PROMPT)"]          # non-interactive task form
  resumeArgs: ["resume", "--last"]        # continue after suspend
  stateDirs: ["/state/home/.codex"]
  credentials:
    secretRef: harness-codex-credentials  # OPENAI_API_KEY or auth.json
```

Equivalent profiles:

- `pi`: `pi` / `pi -p "$(PROMPT)"` / `pi --continue`; state in
  `/state/home/.pi`.
- `opencode`: `opencode` / `opencode run "$(PROMPT)"` /
  `opencode --continue`; state in `/state/home/.local/share/opencode`.
  opencode's client/server mode (`opencode serve`) is a candidate for a
  richer non-TTY control path later, but the v1 contract is the same PTY
  wrapper as the others.
- `claude-managed`: image carries Anthropic's provider-agnostic runner and
  SDK baked in (replacing the Sprites upload + `pip install` step);
  credentials are the **scoped environment key only** — the org API key
  never enters the cluster. Resume/redelivery is handled by Anthropic's work
  queue; the pod name derives from the session ID so redelivery is
  idempotent (`AlreadyExists` plays the role of Sprites' HTTP 409).

Exact resume flags and state paths per harness are pinned down during
image-building (phase 0 below); the abstraction only requires that each
harness has *some* file-backed session state and *some* resume invocation.

## State layers, checkpoints, and forking

Two storage modes, selected per worker (`spec.workspace.mode`):

**`pvc` — simple mode.** One PVC per worker holds `/state`; suspend keeps
the PVC, fork clones it via CSI `VolumeSnapshot` + `dataSourceRef`. No extra
infrastructure, but forks are whole-volume operations, RWO volumes pin
resume to one zone, and storage cost scales with full copies.

**`overlay` — checkpoint mode, the intended default.** Layered state
following the snapshot model of
[autopoiesis](https://github.com/pyrex41/autopoiesis), which represents
agent state as a content-addressable snapshot DAG with O(1) forking and
diffable divergent timelines. Here the same model is applied to worker
*filesystem* state: a worker's `/state` is an overlayfs mount over a shared
immutable base.

```text
lowerdir   shared read-only base: repo clone, toolchain, harness install
upperdir   the worker's private delta, on node-local disk
merged     /state as the worker sees it
```

A **checkpoint** is the upperdir streamed as a content-addressed archive to
object storage plus a small manifest — parent checkpoint, git commit,
harness session ref — the same manifest shape already sketched in
[SESSION-HISTORY.md](SESSION-HISTORY.md). Because the base never changes, a
checkpoint captures only the delta: cheap to take, cheap to store,
deduplicated across the fleet.

Checkpoints form a **DAG**, not a per-worker chain: each manifest points at
its parent, forks create branches, and a worker is just a mutable pointer
to a branch head — the same relationship an autopoiesis context has to its
turn DAG. That framing buys three things:

- fork from *any* historical checkpoint, not only a worker's latest state
  (`workspace.from.checkpointRef`);
- branch-level identity: at the metadata layer a fork is O(pointer); the
  O(delta) cost is only paid when the new worker's node materializes the
  upperdir;
- **diffable timelines**: two divergent branches can be compared — the
  upperdirs give the workspace diff, the harness session files give the
  conversation diff, and the shenmux history archives give the terminal
  diff. An orchestrator that forked N approaches can answer "how did these
  runs actually differ?" from the DAG alone.

The lifecycle verbs become checkpoint operations:

- **suspend** = final checkpoint upload, then the pod *and its node-local
  state* are discarded entirely; the worker's durable identity is its
  checkpoint chain, not a volume.
- **resume** = on any node, in any zone: mount the base, lay the downloaded
  checkpoint archive down as upperdir, remount, run the harness resume
  invocation.
- **fork** = resume from another worker's checkpoint under a new worker
  identity. Forks are O(delta) rather than O(volume), and work cross-node,
  cross-zone, even cross-cluster, because state rehydrates from object
  storage instead of following a volume.

Checkpoint triggers mirror SESSION-HISTORY.md's lifecycle hooks: on
suspend, on fork request, on clean completion, periodically while running,
and in the pod's `preStop` hook — so losing a node costs at most the
since-last-checkpoint window.

Mount mechanics: kernel overlayfs needs `CAP_SYS_ADMIN`, which conflicts
with the restricted/gVisor posture the harness container should run under.
Keep the mount out of the worker pod's trust domain: either a per-node
**snapshot-agent DaemonSet** that prepares merged mounts and bind-mounts
them into worker pods (and flushes upperdirs on drain), or `fuse-overlayfs`
in an unprivileged sidecar. Worker containers only ever see the merged
`/state`.

In either mode, forking a live worker is crash-consistent; fork suspended
workers for clean lineage. Overlay checkpoints and the git-based workspace
checkpoints in SESSION-HISTORY.md compose rather than compete: the overlay
delta is the coarse restore/fork unit, and git worktree state is the
human-readable diff inside it.

### Checkpoint manifest: autopoiesis-compatible

Verified against the autopoiesis source (`packages/core/src/snapshot/`),
so worker checkpoints can be read by its tooling rather than merely
resembling it:

- Its on-disk snapshot is an s-expression
  `(snapshot :version 1 :id <uuid> :timestamp <unix-seconds>
  :parent <uuid|nil> :agent-state <sexpr> :metadata <plist>
  :hash <sha256-of-agent-state>)`, stored as
  `snapshots/<id[0:2]>/<id>.sexpr` beside an index file. Worker manifests
  adopt `:id` (random UUID as identity), `:parent`, `:metadata`, and the
  hash-as-dedup-fingerprint distinction verbatim.
- Its filesystem tree entries are plists
  `(:file "path" :hash <sha256-of-bytes> :mode <st_mode> :size N :mtime T)`
  sorted by path, with a canonical tree hash over `F:path:hash:mode:size`
  strings (mtime excluded). But `snapshot-to-sexpr` **drops**
  `:tree-root`/`:tree-entries` — filesystem state never round-trips to
  disk. Worker manifests are therefore a strict superset: the same entry
  format, plus persisted `:tree-root`, `:tree-entries`, and an
  `:upperdir-archive <sha256>` pointing at the delta archive in object
  storage. Persisting the entries makes autopoiesis's `tree-diff` (two
  sorted entry lists in, add/remove/modify records out) work on *stored*
  checkpoints — its own `manager-diff` can only compare live sandboxes by
  rescanning.
- The natural integration point is its **execution-backend protocol**
  (`backend-create/destroy/exec/snapshot/restore/fork`,
  `backend-supports-native-fork-p`) — almost exactly the operator's verb
  set. A `k8s-overlay-backend` implementing it, answering
  `supports-native-fork-p → T`, gives its `manager-fork` a genuinely
  O(delta) native path (today only its docker backend claims one, via
  `docker commit`), and the overlay upperdir realizes the incremental
  snapshot path its unused `changeset.lisp` was built for.
- Impedance mismatches to respect: three time bases coexist in autopoiesis
  (double-float unix seconds, universal-time integers) — worker manifests
  carry integer unix seconds and label them in `:metadata`; its branch
  objects are in-memory only and branch merge is unimplemented, so
  checkpoint-DAG branches are owned by the operator/CR layer, with
  autopoiesis branches treated as ephemeral views.

### Reuse strategy: autopoiesis as specification, not vendored code

Decision: autopoiesis code is not extracted or transliterated into the
Kubernetes side. The formats are the asset — the implementations are
prototype-grade (tree fields dropped on serialization, an unused
incremental-changeset module, in-memory-only branches, unvalidated
`:version`, non-crypto UUIDs, mixed time bases) and the snapshot code is
idiomatic SBCL that has no clean path into a Go control plane.

- **Kubernetes/shenmux side (Go):** a small `checkpoint` package
  reimplements exactly the format surface — manifest s-expression
  read/write, tree-entry canonical strings and tree hash, `sexpr-hash`'s
  tag-prefixed walk, and blob hashing. Compatibility is enforced, not
  assumed: golden fixtures are generated by autopoiesis's own functions
  (`snapshot-to-sexpr`, `tree-hash`, `sexpr-hash` on known inputs),
  committed, and cross-tested in CI. Known quirks are fixed forward in our
  layer (integer unix timestamps, persisted tree fields, crypto-random
  UUIDs) — safe because autopoiesis never validates `:version` and reads
  superset manifests.
- **autopoiesis side (Lisp, in its repo):** the `k8s-overlay-backend` is a
  thin (~100-line) implementation of the existing `execution-backend`
  generics that calls the operator's API. `manager-fork`, `manager-diff`,
  the MCP snapshot tools, and the capability layer are reused unchanged by
  *not moving them*.
- **The seam is the API, not a protocol adapter**: declarative verbs are
  `AgentWorker` CRs (reached via the `muxwork` CLI or any Kubernetes
  client); the imperative backend calls that don't map onto CRs —
  `backend-exec`, `backend-snapshot`, `backend-restore` — are a small
  synchronous HTTP/JSON facade on the operator, which suits the
  execution-backend generics' plain request/response shape and SBCL's
  mature HTTP clients. Events flow back as the CR watch stream
  (`muxwork watch --json`).

## The orchestrator layer

### AgentWorker CRD

The unit of orchestration is a namespaced `AgentWorker` custom resource
reconciled by an operator:

```yaml
apiVersion: shenmux.dev/v1alpha1
kind: AgentWorker
metadata:
  name: fix-flaky-tests
spec:
  harness: codex
  prompt: "Find and fix the flaky tests in ./services/api"
  interactive: false          # true = TUI idle, steer via shenmux
  suspend: false              # true = delete pod, keep PVC
  workspace:
    mode: overlay             # overlay (checkpointed) | pvc (simple)
    size: 20Gi
    from:                     # omit for a fresh worker
      workerRef: refactor-auth   # fork: clone this worker's state
  ttlSecondsAfterFinished: 86400
status:
  phase: Running              # Pending|Provisioning|Running|Suspended|Completed|Failed
  podName: worker-fix-flaky-tests-abc12
  pvcName: worker-fix-flaky-tests
  shenmuxSession: fix-flaky-tests
  workspaceURL: https://mux.example.com/workspace?session=fix-flaky-tests
  forkedFrom: refactor-auth@snap-2026-08-06T17-04
```

The operator reconciles each `AgentWorker` into: a PVC (fresh, or cloned for
forks), a minted shenmux enrollment credential, and the worker pod described
above. It garbage-collects pods on suspend/TTL and never deletes a PVC except
on `AgentWorker` deletion.

### Lifecycle verbs

- **spawn** — create an `AgentWorker`. Fresh PVC, new shenmux session.
- **suspend** — set `spec.suspend: true`. The operator deletes the pod; the
  PVC (workspace + harness session files + shenmux history) remains. This is
  the idle-cost answer: Kubernetes has no Sprites-style pause, so durability
  lives in the volume, not the pod.
- **resume** — clear `suspend`. The operator recreates the pod with the
  harness's `resumeArgs`, so the agent continues its recorded conversation
  against the same workspace. The shenmux session name is stable, so the
  controller workspace shows the same session again; `--history-dir` gives
  the terminal archive continuity across the pod boundary.
- **fork** — create an `AgentWorker` whose `workspace.from` names a source
  worker or a specific checkpoint. The operator materializes the source
  state into the new worker — a checkpoint restore in overlay mode, a CSI
  volume clone in pvc mode (see "State layers, checkpoints, and forking").
  Because harness session state is plain files under `/state`, cloning the
  state clones the conversation; the fork resumes it under a new worker
  identity and diverges. Neither mechanism needs harness cooperation.
  - `claude-managed` caveat: the conversation lives on Anthropic's side and
    has no fork API; forking such a worker clones the workspace only and
    starts a fresh managed session against it.
- **attach** — not an operator verb at all: `status.workspaceURL` deep-links
  into the shenmux controller workspace, where control leases already govern
  who may write.

### Who calls the verbs

Three producers create and drive `AgentWorker`s:

1. **Humans**, via `kubectl` or the shenmux workspace (fleet view grouped by
   namespace/workload/harness — the metadata the agent already advertises).
2. **The claude-managed bridge**: a small Deployment running Anthropic's
   environment work poller (or webhook receiver). Each claimed work item
   becomes an `AgentWorker` with `harness: claude-managed`; session
   completion marks the worker `Completed`. This is the direct analogue of
   the Sprites worker, with `spawn()` replaced by creating a CR.
3. **Orchestrator agents**: a worker whose sidecar is marked
   `--orchestrator`, given the **orchestration CLI** in its image (working
   name `muxwork`): `muxwork spawn|suspend|resume|fork|list|status|logs`,
   plus `muxwork watch` for the event stream. Every harness can play
   orchestrator because every harness can run a shell command — no
   protocol adapter per harness. This is how "an agent spins up
   durable/resumable/forkable sub-workers" is expressed: the orchestrator
   plans, forks a base worker per approach, and harvests results — while
   every sub-worker remains a first-class shenmux session a human can
   open and steer.

   autopoiesis is the natural orchestrator brain here, beyond being the
   source of the snapshot model, and a CLI is native to its idioms: its
   entire provider layer already drives claude/codex/opencode as
   pipe-based subprocesses, so `muxwork` is just another
   `run-provider-subprocess` target (and `muxwork watch --json` lines
   pump straight into its substrate as datoms for its reactive
   `defsystem` hooks). It routes `:pi` tasks despite shipping no pi
   provider, and it contains no PTY or multiplexer code at all — shenmux
   workers supply exactly the durable interactive terminal surface it
   lacks. The symmetry is the point — the orchestrator's *cognitive*
   state and each worker's *filesystem* state are both content-addressed
   forkable DAGs, so forking a plan branch can fork the workers it was
   driving, and diffing two plan branches can pull in the corresponding
   worker-timeline diffs.

### The verb surface: API first, CLI as the face

There is no separate orchestration service. The **Kubernetes API is the
API**: every verb is CRUD on `AgentWorker` CRs, and asynchronous events
are the CR **watch stream** — an ordered, resumable
(`resourceVersion`-cursored) message feed of status transitions
(spawned, running, checkpointed, suspended, completed, failed) that
already is a real message protocol, with delivery, resume, and authz
semantics we don't have to invent.

`muxwork` is a thin Go client over that API (shared client library with
the operator), not a second control plane. Each worker pod gets a
ServiceAccount whose RBAC defines what its agent may do — e.g. an
orchestrator may create and fork workers in its namespace, a leaf worker
may only read its own status. One authorization point — CR RBAC — covers
humans with kubectl, the bridge, orchestrator agents, and autopoiesis's
`k8s-overlay-backend` (which consumes a small HTTP facade on the operator
for the exec/snapshot/restore calls that don't map onto CRs; see the
reuse strategy).

If fan-out beyond the cluster ever matters (many external observers,
cross-cluster fleets), a broker like NATS can mirror the watch stream —
an addition behind the same event schema, not a replacement.

## Security posture

- **Isolation**: default pod isolation is weaker than the microVMs the
  Sprites model assumes. Worker pods should run under a gVisor/Kata
  `runtimeClassName`, the restricted Pod Security Standard, and a
  default-deny egress `NetworkPolicy` with per-harness allowlists (the
  provider API endpoints, plus whatever the task needs).
- **Permission bypass is the norm, not the exception**: harnesses run
  unattended inside workers with their own permission prompts disabled
  (autopoiesis's claude-code provider passes
  `--dangerously-skip-permissions`; codex runs `--full-auto`). The pod
  sandbox, egress policy, and shenmux control leases are therefore the
  *only* effective controls — design them as such.
- **Credentials**: `claude-managed` keeps the org key out of the cluster by
  design; the CLI harnesses cannot — they need live provider credentials in
  the pod. Mitigate by routing them through an LLM gateway (so pods hold
  only short-lived gateway credentials) or per-worker scoped keys, mounted
  as Secret files, never argv. Assume anything in `/state` is readable by
  the task the agent executes.
- **shenmux trust boundary**: the controller/agent path is currently a
  development path — trusted browser only, no production identity, and
  controller restarts drop live connections (agents reconnect). The fleet
  workspace must sit behind real authentication before this design leaves a
  trusted internal network; that work is already gated in
  [V1-V2-PLAN.md](V1-V2-PLAN.md).

## Elastic capacity (Karpenter)

Worker pods are the unit of scheduling, so node capacity should follow the
fleet rather than be pre-provisioned:

- A dedicated Karpenter `NodePool` (tainted `shenmux.dev/workers`) sized by
  whatever `AgentWorker`s currently demand, consolidating to zero when the
  fleet is suspended.
- Fork swarms are the burst case: an orchestrator agent forking N approaches
  creates N pods at once; Karpenter provisions just-in-time nodes and
  consolidates them away as workers complete or suspend.
- Overlay mode makes **Spot capacity safe** for workers: with periodic and
  `preStop` checkpoints, a Spot interruption is just an involuntary
  suspend — the operator marks the worker `Suspended` and resumes it on the
  next node from its last checkpoint. Keep pvc-mode workers on on-demand
  capacity (RWO volumes plus interruption is a worse story), and keep the
  shenmux controller and the operator off the Spot pool.
- The node termination grace window must cover a checkpoint upload; the
  snapshot agent prioritizes flushing upperdirs on node drain.

## Platform provisioning (Crossplane)

Crossplane earns a place in two roles — with a deliberate line drawn
around what it should *not* do:

1. **Environment stamping.** An `AgentEnvironment` XRD + Composition
   renders everything a fleet needs as one claim: the namespace and
   quotas, the checkpoint bucket and its IAM/IRSA binding for the
   snapshot agent, per-harness credential secrets, the Karpenter
   `NodePool`, default-deny NetworkPolicies, and the shenmux controller
   install. One claim = one ready fleet environment; per-team or
   per-cluster fleets become claim-per-team, and the cloud-side pieces
   (bucket, IAM) stay continuously reconciled by Crossplane's providers
   in the same GitOps flow as the workers. This is Crossplane's sweet
   spot and should be adopted as the way environments come to exist.
2. **Optionally, the worker's provisioning half.** `AgentWorker`'s
   render-a-set-of-resources portion — PVC, enrollment Secret, pod,
   NetworkPolicy — could be a Composition (with a composition function
   building the pod spec from the harness profile), shrinking the custom
   operator. What must stay custom either way is the **lifecycle
   brain**: the phase machine, suspend/resume sequencing, fork's
   snapshot-then-provision ordering, checkpoint GC, and status for the
   watch stream. Compositions reconcile toward a desired resource set;
   they are the wrong tool for ordered stateful workflows, so fork and
   checkpointing are never forced into one. Decide at Phase 1 whether
   the operator embeds provisioning or delegates it to a Composition.

Either way, Crossplane sits **behind** the CR API: `AgentWorker`,
`muxwork`, RBAC, and the watch stream are unchanged and agents never see
it.

## Net-new work this design requires

In shenmux:

1. **Enrollment minting API.** The controller issues a single-use enrollment
   code on stderr with a ten-minute expiry — right shape, wrong delivery for
   ephemeral pods. The operator needs an authenticated controller endpoint to
   mint one code per worker at provision time, injected as a Secret and
   consumed on first agent start.
2. **Worker images.** The root `Dockerfile` builds only the legacy `muxd`
   image. Needed: a base image with the full `shenmux` binary, plus one thin
   layer per harness (harness binary + runner, `HOME=/state/home`).
3. **History restore verification.** `shenmux run --history-dir` persists the
   authoritative archive; confirm (and wire, if missing) that a restarted
   `shenmux run` restores the persisted checkpoint so resumed workers keep
   their terminal scrollback, not just their conversation files.

New components (this repo or a sibling):

4. `AgentWorker` CRD + operator (spawn/suspend/resume/fork/GC).
5. Snapshot-agent DaemonSet (overlay mounts, upperdir flush on drain) and a
   content-addressed checkpoint store client for object storage.
6. Claude-managed bridge (work-queue poller → CRs).
7. The `muxwork` CLI and the operator's HTTP facade for
   execution-backend calls (both thin clients of the CR API and operator;
   no separate orchestration service).

Karpenter itself is off-the-shelf; the design only adds a `NodePool` and
interruption-aware suspend handling in the operator.

## Phasing

- **Phase 0 — prove the pod and the contract.** Build the worker images;
  run one hand-written pod per harness with the `shenmux run` wrapper and
  agent sidecar; attach from the workspace; pin down each harness's state
  dirs and resume flags. Generate the autopoiesis golden fixtures and land
  the Go `checkpoint` package that passes them — the format contract
  everything later builds against. No operator yet.
- **Phase 1 — durable/resumable.** `AgentWorker` CRD + operator with
  spawn/suspend/resume in pvc mode; enrollment-mint API in the controller;
  the `AgentEnvironment` Crossplane Composition for environment stamping,
  and the decision on whether worker provisioning stays in the operator or
  moves to a Composition.
- **Phase 2 — forkable + managed.** Overlay mode: snapshot-agent DaemonSet,
  checkpoint store, fork-from-checkpoint, Spot-friendly interruption
  handling on a Karpenter `NodePool`; CSI `VolumeSnapshot` clone remains
  the pvc-mode fallback. The claude-managed work-queue bridge with
  idempotent redelivery.
- **Phase 3 — orchestrator agents.** The `muxwork` CLI in worker images
  with per-worker ServiceAccount RBAC; the operator's HTTP facade for
  autopoiesis's `k8s-overlay-backend`; fleet grouping in the workspace
  using the advertised harness/workload/orchestrator metadata.

Sketch manifests for the CRD and example workers live in
[`deploy/kubernetes/orchestrator/`](../deploy/kubernetes/orchestrator/).
