# Scope

This repository is five days old. It has 60 commits. In that time it grew a
Kubernetes deployment story, a WASI filesystem runtime in the browser, a
process supervisor, a signed-release verifier, a durable checkpoint store for a
different product, an object-storage service, and a vision document promising a
hosted SaaS with OIDC and WireGuard.

None of that carries a keystroke to a shell.

This document says what shenmux is, what it refuses, how the refusal is
enforced, and what gets deleted. The default answer to "should this stay" is
no. The only question that keeps something is: **does the pipe work without
it?** If it does, it is not in the pipe, and it does not live here.

---

## 1. What shenmux is

shenmux owns a PTY and lets you reach it from somewhere else.

That is the whole product. Five things are required to do it, and they are the
only five things this repository is allowed to contain:

| # | Owns | Packages | Non-test LOC |
|---|---|---|---|
| 1 | The PTY and the authoritative screen | `internal/ptyx`, `internal/term`, `internal/screen` | 2,841 |
| 2 | Who may attach, control, type, resize | `specs/mux.shen`, `internal/shenmodel`, `internal/shenguard` | 1,589 + 563 Shen |
| 3 | The wire that carries it | `internal/protocol`, `internal/zmqx`, `internal/relay`, `internal/transport` | 3,887 |
| 4 | Proof of who is asking | `internal/relay` (enrollment, device keys), `internal/policy` | 670 + shared |
| 5 | Two clients that paint the screen | `client/`, `cmd/muxctl`, `internal/ttyx`, `internal/webui`, `internal/webgateway` | 1,449 |

Plus `internal/server` (2,454), which is the daemon that runs items 1–3
together, and `internal/agent` (493), which bridges a remote stream onto a
local session.

Note carefully: shenmux is **not** "just a connector." It creates the PTY and
runs the terminal emulator (`internal/term`, 1,963 lines). If you describe it
as a connector, someone will eventually propose moving terminal emulation out,
and they will be wrong. Own the PTY. Own the screen. Connect to it.

---

## 2. What shenmux refuses

Five refusals. Each names the thing that owns it instead. Each is checkable in
a pull request without knowing the history.

**R1. It does not create, schedule, or reconcile work.**
No supervisor. No job runner. No operator. No reconciliation loop. `shenmux
run` starts exactly one process — the shell you asked for — and that is the
only process this binary will ever spawn. Everything else belongs to whatever
control plane decided the work should exist. That control plane may use
shenmux to attach to what it started. It does not live inside shenmux.

**R2. It does not store or synchronize user content.**
Not files, not scrollback beyond the bounded in-memory tail, not repositories,
not objects. If content needs to persist, something else persists it. shenmux
persists exactly two things: user configuration and the agent's device
credential. Both are required to attach.

**R3. It has no vocabulary for where the process runs.**
No `Cluster`, no `Namespace`, no `Workload`, no `Pod`, no `Node`, no
`Harness`, no `Orchestrator`. One `map[string]string` of opaque labels, passed
through untouched, rendered by whoever asked. A typed field is a claim that
shenmux understands the concept. It does not, and the moment it appears to,
someone will branch on it.

**R4. It does not distribute itself.**
No update checker, no signed manifest verifier, no self-install. Shipping
binaries is a release pipeline's job.

**R5. It does not remember a session after the session dies.**
The PTY is gone; there is nothing to reattach to. Writing the last screen to
disk so a different product can render a picture of a corpse is that product's
feature, not this one's.

---

## 3. Enforcement

A boundary you cannot point at is not a boundary. This repo already has the
right idiom — `scripts/shenguard-audit.sh` is a `find` and two `grep`s that
print PASS or FAIL, and it keeps the TCB closed. Add `scripts/scope-audit.sh`
in the same shape and wire it into the gate list. (Note there are currently
two gate lists — `make check` and `sb.toml` — and nothing runs the second one.
Resolve that first; see §4.5.)

Five checks. Every one of them would have failed on something in this
document.

**C1 — Vocabulary.** Fail if any non-test `.go` file under `internal/`,
`client/`, or `cmd/` matches:

```
-iE 'kubernetes|k8s|namespace|cluster|workload|orchestrat|harness|reconcile|schedul'
```

with an explicit allowlist file. Exactly one legitimate hit exists today:
`internal/screen/types.go:41` ("grapheme cluster"). Everything else the grep
finds right now is on the cut list in §5. This check is dumb on purpose. You
cannot talk it out of a failure in a PR description.

**C2 — Binaries.** Fail if `ls cmd/` is not exactly the frozen set. A new
product usually arrives as a new binary.

**C3 — Verbs.** Fail if the subcommand switch in `cmd/shenmux/main.go`
(currently `run`, `login`, `status`, `web`, `agent`, `controller`, plus
`help`/`version`) does not match a frozen list. This is the highest-value
check in the file. Every single thing cut below would have arrived as a new
verb: `shenmux workspace`, `shenmux job`, `shenmux operator`, `shenmux
update`. Freezing the verb list makes adding one cost an argued commit.

**C4 — Dependencies.** Fail if the direct `require` block in `go.mod` is not
exactly:

```
github.com/creack/pty/v2
github.com/gorilla/websocket
github.com/tomi77/zmq4
golang.org/x/term
```

Four direct dependencies is the correct number for a terminal multiplexer.
`client-go`, an S3 SDK, or an OPA binding cannot arrive quietly.

**C5 — Reachability.** Run `golang.org/x/tools/cmd/deadcode` against
`./cmd/...` and fail on anything reported outside a small allowlist. This is
the strongest check here, because "it exists because it was possible" is
mechanically detectable: it is code nothing calls. It catches
`internal/supervisor`, `internal/update`, `internal/workspace/checkpoint.go`,
and thirteen uncalled exported methods on `policy.Store` — today, without
anyone having to notice them.

### What cannot be enforced, said plainly rather than faked

C1–C5 would **not** have caught the `/workspace` pane. It has neutral names,
real callers, passing tests, and no forbidden vocabulary. It is 2,800 lines of
a storage-and-sync product wearing a plausible package name.

There is no honest automated check for that. Do not invent one. A LOC-ratio
gate or a package-count budget is ceremony; it will be tuned until it passes,
and the tuning commit will have a good reason. The only thing that catches the
workspace pane is a human asking the question at the top of this file in
review, and being willing to hear yes. Write the question into the PR template
and leave it at that.

---

## 4. Decisions

### 4.1 The `/workspace` pane — cut entirely (option a)

**Decision: delete. All of it.** Not a thin read-only view.

The pane is not a file browser attached to a terminal. It is a **second shell**.
`internal/webui/workspace.js:441` dispatches `ls`, `cat`, `cd`, `mkdir`,
`touch` and `write` into a Rust WASI component
(`workspace_component.core.wasm`, 56,001 bytes, loaded via
`WebAssembly.compileStreaming`) — five commands reimplemented in Rust, running
in a browser tab, next to a real PTY that has had all five since 1979.

That is the purest possible instance of a feature that exists because it was
possible.

The owner's question — *"how does this work with cloning a repo server side?"*
— has an answer already on the screen: type `git clone`. The fact that the
question needed asking is the tell. But the strongest form of the argument
isn't "the control plane owns the files." It is that a second, worse shell is
not a feature at any size.

**Why not option (b), a thin read-only file view.** Because there is no stable
resting point there. A read-only listing beside a live shell is a strictly
worse `ls`, and it is not free: it costs an HTTP surface, a capability scope,
and a path-traversal defence (`internal/workspace/store.go:198` symlink
escape, `:243` traversal) that has to stay correct forever. You pay real
security-review cost to render information the terminal already renders. And
the first time someone asks "can I edit it," you are back to ETags, `If-Match`,
409 conflict handling, and a write-back journal — which is exactly the code
this cut deletes. Go to zero.

**What gets deleted:**

| Path | Lines |
|---|---|
| `internal/workspace/` (store.go 279, http.go 184, checkpoint.go 168, + 186 test) | 817 |
| `internal/webui/workspace.js` | 540 |
| `internal/webui/workspace-runtime.js` (OPFS cache, journal, ETag sync) | 220 |
| `internal/webui/workspace.html` / `.css` / `-filesystem.js` / `-component-entry.js` | 163 |
| `internal/webui/workspace-component.js` (jco bundle, minified) | 113,929 B |
| `internal/webui/workspace_component.core.wasm` | 56,001 B |
| `runtime/workspace-component/` (Rust + wit + Cargo.lock 447) | 632 |
| `runtime/golem-workspace/` (Rust + Cargo.lock 1,452) | 1,582 |
| `scripts/test-workspace-{browser,component,runtime}.mjs` | 360 |
| `web/build-workspace.mjs` | 24 |
| `docs/WORKSPACE-RUNTIME.md` | 77 |
| Makefile targets `workspace-component-build/test`, `workspace-runtime-test`, `workspace-test`, `golem-workspace-build` | 16 |

Roughly **4,400 lines plus 170 KB of committed binary artifacts**, against a
real browser terminal client (`pixi-client.js`) of 535 lines. The pane is
eight times the size of the thing it sits next to.

**Edits required (4 references, that is all):**
`internal/webgateway/server.go` lines 24, 66–67, 119, 133–137, 172–190;
`cmd/shenmux/main.go` lines 38, 486, 525–545.

**What breaks:** `GET /workspace` on the gateway 404s. `--workspace-dir`
disappears. `/workspace-objects` disappears. Three `internal/webgateway` tests
fail and are deleted (`TestHandlerMountsCapabilityWorkspace`,
`TestWorkspacePageCarriesItsMode`, and two assertions inside
`TestHandlerServesEmbeddedClient` at `server_test.go:22-23`).

**Who notices:** nobody. The remote path of this feature was unreachable code
until commit `835986c` on 2026-08-06 — its own commit message says so. And
`make workspace-component-build` is **red on this checkout right now**:
`Makefile:22` invokes `web/node_modules/@bytecodealliance/jco`, which is not
installed, and `web/build-workspace.mjs:22` copies from
`runtime/workspace-component/generated/`, which does not exist. Worse,
`make check` never runs `workspace-test`, so nothing noticed. A feature whose
build is broken and whose gate does not run it is already deleted; this just
makes it official.

**Data:** OPFS state lives in the browser, per-origin, on one laptop. No
migration.

**Irreversible?** No. The Rust source and the `.wasm` are in git history.

### 4.2 Kubernetes material — delete all of it, per item

| Item | Lines | Decision |
|---|---|---|
| `deploy/kubernetes/local-trial.yaml` | 197 | **Delete.** Added 2026-08-06. |
| `deploy/kubernetes/worker-template.yaml` | 107 | **Delete.** Added 2026-08-06. |
| `deploy/kubernetes/agent-sidecar.yaml` | 81 | **Delete.** Keep the *idea* as ~10 lines of prose in `docs/DEPLOYMENT.md`. |
| `deploy/kubernetes/controller.yaml` | 69 | **Delete.** |
| `deploy/kubernetes/Dockerfile` | 30 | **Delete.** Root `Dockerfile` now builds `shenmux`. |
| `deploy/kubernetes/README.md` | 37 | **Delete.** It is 16 lines of "before adapting these, fix all of this." |
| `scripts/k8s-worker.sh` | 105 | **Delete.** Added 2026-08-06. It is an orchestrator written in bash. |

626 lines of YAML that `deploy/kubernetes/README.md:3` itself calls "fragments
for development and design review, not a deployable release." Untested YAML is
not documentation. It is a promise, and this repo does not keep it.

`agent-sidecar.yaml` is the only one carrying a real architectural claim — an
agent sharing an owner-only IPC volume with a separately-running PTY process.
That claim is worth keeping as prose in `docs/DEPLOYMENT.md`. The YAML is not.

**The flags.** `cmd/shenmux/main.go:618–625` defines seven, on the `agent`
subcommand only:

```go
--cluster --namespace --workload --pod --node --harness --orchestrator
```

Five of them also read downward-API environment variables (`POD_NAMESPACE`,
`POD_NAME`, `NODE_NAME`) — shenmux reaching into a Kubernetes pod's env to
learn where it is.

**Delete all seven.** Replace with one repeatable `--label key=value`.
`AgentMetadata.Labels map[string]string` already exists at
`internal/relay/identity.go:109` and is *declared but never populated by any
code path* — so this is a flag parser and one assignment, not zero code, but
close.

**`AgentMetadata`** (`internal/relay/identity.go:102–110`). Delete `Cluster`,
`Namespace`, `Workload`, `Pod`, `Node`, `Harness`, `Orchestrator`. Keep
`Labels` and `Sessions`.

For the record, since the owner's evidence said "opaque metadata labels passed
through for display": `Cluster` and `Node` are **never read anywhere** —
write-only passthrough. `Pod` is display-only. `Namespace` and `Workload` are
**filter keys in controller logic** (below). So two of the five are
architecture, not labels.

**`SessionDescriptor`** (`identity.go:113–120`). Delete `Kind` (whose comment
reads `// terminal, harness, orchestrator`) and `Harness`. Delete `Interactive`
too — every PTY is interactive. Keep `ID` and `Name`. Every session shenmux
knows about is a terminal. If it is not a terminal, shenmux should not be
carrying it.

**The filter.** `internal/relay/tunnel.go:929–933` branches on
`agent.metadata.Namespace` and `.Workload` to filter `GET /sessions`. The
owner's evidence said these fields are opaque display labels; they are not —
this is real logic keyed on a Kubernetes concept, and it is the exact thing
R3 exists to prevent. **Delete the filter.** A controller in this design has
tens of agents. Let the client filter, on labels it does not have to
understand.

Two consumers must go in the same commit: `internal/relay/workspace.js:36`
renders `metadata.pod`, `metadata.harness` and `metadata.namespace` into the
session list and sends `?namespace=` from its own query string (that file is
being deleted anyway — see §4.5), and `internal/relay/relay_test.go:266,280`
exercises `GET /sessions?namespace=agents` and asserts
`Metadata.Pod == "codex-0"`.

*Unrelated bug, noted because it is in the line being deleted:*
`tunnel.go:933` reads `ns != "" && A || wl != "" && B` with no parentheses. Go
binds it as `(ns != "" && A) || (wl != "" && B)`, which is what was intended,
but nobody should have to work that out. It goes away with the filter.

**Wire compatibility:** this changes the handshake JSON. Every field is
`omitempty`, and after the filter is gone nothing branches on any of them, so
an old agent against a new controller just loses its display labels. There is
no deployment. The cost is zero today and non-zero next month. Do it now.

**What is *not* cut:** nothing about Kubernetes is architecturally present in
this repo, and after this change nothing about it is lexically present either.
That is the point. The control plane runs on Kubernetes; it uses shenmux;
shenmux does not know.

### 4.3 The unmerged branches

Six of the thirteen branches are already fully contained in `main` — zero
commits ahead, zero unmerged patches:

`agent/local-first-runtime-spec`, `agent/reliability-docs-and-transport`,
`agent/vision-document`, `agent/wasm-workspace`,
`origin/agent/shen-control-plane`, plus the matching remotes.

**Delete them.** Six branches whose names imply pending work and which contain
none. That is six lies about intent removed for free.

**`origin/claude/kubernetes-agents-shenmux-1wpiwn`** (16 commits, 7,446
insertions) is **strictly contained** in `fix/snapshot-darwin-portability`,
which is the same 16 commits plus one Darwin portability fix. **Delete the
remote branch**; it is a duplicate.

**`fix/snapshot-darwin-portability`** (17 commits, 7,458 insertions,
`orchestrator/{operator,snapshot,secretproxy,muxwork,mockagent}`, 3,782
non-test Go LOC). **Never merge here. Extract to its own repository.**

Two things about this branch matter more than the decision:

1. It has **its own `go.mod`** — module
   `github.com/pyrex41/shenmux/orchestrator`, `go 1.26`, and **no `require`
   block at all**. Stdlib only.
2. It **never imports the root `internal/` packages.** Not once, in 3,782
   lines.

Someone built an entire CRD-driven agent orchestrator with overlayfs
checkpoints and a credential-intercepting secret proxy, and never needed the
pipe. The boundary this document proposes is not aspirational — it already
holds, physically, in that code. You do not have to argue for it. You have to
run `git subtree split -P orchestrator` and push it somewhere else.

Also: rename the branch first. `fix/snapshot-darwin-portability` is a
7,458-line feature branch wearing a bugfix name. That is how 7,000 lines get
merged without a review.

**`agent/golem-job-runtime`** (1 commit, 2,492 insertions;
`internal/jobs` 363, `internal/relay/jobs.go` 203, `golem/` Rust component,
plus edits to `internal/relay/tunnel.go` and `cmd/shenmux/main.go`).
**Delete. Do not merge, do not extract.**

It is the mirror image of the orchestrator branch. It cannot be lifted out,
because it reaches *into* `internal/relay` and `cmd/shenmux`. That is not a
compliment to its design; it is the reason to refuse it. Durable background
job coordination is the control plane's definition, not a feature of a
terminal. If the idea is wanted, it gets rewritten on the far side of the wire
against the public protocol, where it belongs.

Record the hash here so the archaeology is honest, then delete the branch:
`847b9cf Add durable Golem job coordination`. Git keeps the objects until gc;
if you want them after that, `git tag archive/golem-job-runtime 847b9cf`
before deleting.

**A branch nobody will merge and nobody will delete is a lie about intent.**
After this, the branch list is `main`, and one extraction in flight.

### 4.4 `docs/VISION.md` — delete it

Not rewrite. Delete.

The load-bearing sentence is line 12:

> shenmux is a durable, identity-aware work surface for terminals, coding
> agents, background jobs, and production sessions.

That names four product categories and refuses none of them. Every single
thing cut in this document was, at the moment it was written, consistent with
that sentence. The workspace pane is a "work surface." The Golem job runtime
is "background jobs." The Kubernetes manifests are "production sessions." The
`--harness codex|claude|pi` flag is "coding agents."

A document that makes every addition defensible is not a vision. It is an
alibi. This one licensed the drift, and saying so plainly is part of the job.

It is also false in the same repository as a document that contradicts it.
`README.md:5` says "It is a development tool. Nothing here is deployed to the
public Internet." `VISION.md:32` says `curl -fsSL https://get.shenmux.dev | sh`
and goes on to describe OIDC/SAML, a regional relay service, ephemeral browser
WireGuard nodes, and Phase 3 live collaboration — for a repository that is five
days old and whose own README says the controller can read your terminal.

**Replacement:** this file, plus `docs/V1-V2-PLAN.md`, which is honest, tracks
gaps explicitly, and already does the roadmap job. The only part of VISION.md
worth saving is its `## Non-goals` section, folded into §2 above and
strengthened — "shenmux is not *initially* a general-purpose VPN" becomes "is
not."

**Also delete `docs/ROADMAP.md`** (24 lines). Its content is "the maintained
status is in V1-V2-PLAN.md." A document whose content is a redirect is a
redirect. Delete it and link V1-V2-PLAN.md directly.

### 4.5 What else goes — things not on the owner's list

**`internal/supervisor` (131 lines) — delete.** Zero importers. Absent from
every binary's dependency closure. Its own doc comment: *"a controller/agent
can use it to create and stop sessions."* That is R1, verbatim, sitting dead in
the tree waiting to be discovered by someone who needs it. It is not just
unused; it is the boundary violation in miniature.

**`internal/update` (62 lines) — delete.** Zero importers. Verifies Ed25519
signatures on release manifests. Kept from rotting only because `Makefile:48`
lists it in `test-relay`. R4.

**`internal/workspace/checkpoint.go` (168) + test (64) — delete.** Shells out
to `git rev-parse`/`diff --binary`/`ls-files -o` and tars the result. No
non-test caller anywhere. It dies with §4.1 regardless; named here so it does
not get rescued on the way out.

**`cmd/wsprobe` — `rmdir`.** Empty directory. No files, no git history, no
reference anywhere in the tree.

**`cmd/muxd` (237) and `cmd/shenmux-web` (48) — delete.** Both are strict
subsets of `shenmux run` and `shenmux web`. `cmd/shenmux/main.go:1` already
calls them "legacy."

This is the thesis's own problem. **shenmux is not one binary. It is four.**
And the one you need to attach from a terminal is not called `shenmux` — there
is no `shenmux attach` verb; `cmd/muxctl` (272 lines) is the only TTY client.
"One binary you drop in" has been true in the docs and false in `ls cmd/` since
the first commit. Fix that: fold `muxctl` in as `shenmux attach`, delete `muxd`
and `shenmux-web`. Result: one binary, six verbs, and `make install` stops
writing four files into `~/.local/bin`.

**`internal/history` (129) — delete. This one has data; see §6.**

Its own package comment: *"a crash-safe durable checkpoint that can be
inspected or served by a higher-level command center."* It is a feature for a
different repository, living in this one. R5.

It also does not do what it sounds like. It writes the last screen to disk.
Nothing can restore it into a live session — the PTY is gone. It is a
photograph of a corpse, shipped to another product. It already has an HTTP
route (`/api/history`), and the next thing it grows is a retention policy and
an index.

*What would have to be true to keep it:* the pipe would have to be able to
**restore** from it — reattach a client to a dead session's last state and
continue typing. It cannot. Until restore exists, this is a log with a
different product's name in its doc comment.

*Bug found on the way:* `README.md:156` says "Nothing survives the session...
Checkpoints and deltas are in memory." That has been false since commit
`ba301ff`. `shenmux run` defaults `HistoryDir` to `<state-dir>/history`
(`cmd/shenmux/main.go:171-176,191`) and `Runtime.persistHistory` writes on
every accepted event (`runtime.go:135,480,565,732,754`). Either the README or
the code is wrong. This cut makes the README right.

**`docs/LOCAL-FIRST-RUNTIME-SPEC.md` (629 lines) — delete.** Its own line 5:
*"Proposed product and architecture specification. This describes a **new
product direction**."* It designs leader/follower replication, epoch fencing,
offline editing, device handoff, and a Golem durable worker — for a product
with a provisional name ("shenmux Local-First Sessions").

Every one of those things is explicitly refused twice elsewhere in this repo:
`docs/ROADMAP.md:22-24` and `docs/V1-V2-PLAN.md:18-20` both say local-first
replication, offline editing, PTY handoff, Golem and collaborative conflict
resolution "are later research ideas... not current behavior and must not
weaken the single-PTY-writer contract."

This is VISION.md's twin at the design layer: 629 lines of specification for
work the project has declared out of scope, sitting in the same directory as
the declaration. Delete it. Nothing links to it from the README.

*Keep* `docs/PLAN-remaining-gaps.md` (403) and
`docs/PLAN-typing-and-robustness.md` (187). Both document closed work on the
pipe, and the reasoning in them — particularly the ZeroMQ ROUTER analysis — is
the most valuable prose in this repository. Reasoning about the pipe stays.
Specifications for products that were refused do not.

**Root archaeology — delete.** `WORKTREE-METADATA.txt` (103 lines),
`SOURCE-SHA256SUMS.txt` (73 lines), `BUILD-REPORT.md` (121 lines) are tracked
handoff artifacts from a pre-history worktree (`/mnt/data/shenmux-repo`, branch
`feat/authoritative-state`, base commit `b909ae10` — which is not in this
repository's history). `BUILD-REPORT.md` is dated 2026-08-01, two days before
the first commit here. `SOURCE-SHA256SUMS.txt` lists three files that no longer
exist. Nothing in any build, script, Makefile target, or doc links to any of
them. `shenmux-authoritative-state-source.zip` (132 KB) is *not* committed —
`.gitignore:8` covers it — but it is sitting in the working directory. Delete
it from disk.

**`sb.toml` vs the Makefile — pick one.** `sb.toml` declares five gates
(`shengen-drift`, `test`, `build`, `shen-check`, `tcb-audit`). Every one of
them shells out to a script that `make check` already runs directly. Nothing in
this repository invokes `sb`. Two gate definitions that must be kept in sync by
hand, one of which nothing runs, is a gate that will be wrong. Either make
`make check` call `sb gates`, or delete `sb.toml`. This matters for §3: adding
`scope-audit` to both is adding a divergence.

**Packaging — five stories, none tested, keep two.** Root `Dockerfile`, root
`fly.toml`, `deploy/fly.toml.example`, `deploy/systemd/`,
`deploy/kubernetes/`. `Makefile:50-51` admits `test-deploy` is a compile check.

- **Keep** the root `Dockerfile` — it now builds `shenmux` (as of `835986c`).
- **Keep** `deploy/systemd/` (42 lines). "Drop the binary on a server and it
  dials out" *is* the thesis. This is the one deployment artifact that states
  it.
- **Delete** `fly.toml` (its `[env]` still configures the legacy `muxd`
  ZeroMQ-over-TCP shape), `deploy/fly.toml.example`, `deploy/kubernetes/`.
- **Delete** `deploy/kubernetes/Dockerfile` (30) regardless of the rest — it is
  a second, divergent image definition that **no manifest references**. Every
  manifest uses either `ghcr.io/pyrex41/shenmux:latest` or `shenmux:local`, and
  `local-trial.yaml`'s own header says `docker build -t shenmux:local .` — the
  *root* Dockerfile. Orphaned since it was written.
- **Fix four stale claims** created by commit `c78164b` on 2026-08-06, which
  added the `shenmux` binary to the root Dockerfile and updated none of the
  documents that describe it: `README.md:148`, `deploy/README.md:14-15`,
  `deploy/kubernetes/README.md:4-5`, `docs/DEPLOYMENT.md:147`. Three of the four
  disappear with the deletions above; fix the README.

**`flake.nix` (39) — keep, current, one note.** It was updated with the pure-Go
ZeroMQ change and matches the build. It provides go, git, gnumake and nodejs —
and **no cargo**. That is why `make workspace-component-build` cannot work
inside `nix develop`, and part of why nobody noticed the workspace build was
red. It stops being a problem when §4.1 lands.

**`internal/policy` — keep, and I disagree with listing it as accreted.**

It is not accretion. It is the "it carries authentication" half of the thesis.
It gates for real: `internal/relay/tunnel.go` has roughly fifteen live
enforcement calls — `IsDeviceRevoked` (373, 797), `AuthenticateCapability`
(465, 673, 779, 802), `RedeemCapability` (489), `IssueCapability` (654),
`AcquireLease`/`RenewLease`/`ReleaseLease` (712/731/542/722), and permission
checks rejecting input/resize/control at 683/709/715/724.

But **delete its uncalled half.** These exported methods have no caller
outside `policy_test.go`: `NewInMemory`, `NewStore`, `GrantACL`, `Grant`,
`CanObserve`, `CanControl`, `ValidateCapability`, `RevocationVersion`,
`RevocationsSince`, `ApplyRevocations`, `ReapLeases`, `AuditEvents`. Three of
those are pure aliases of methods that *are* called (`NewMemory`, `New`,
`AddGrant`) — an alias with no caller is an API you designed twice and used
once. An exported method with no caller is a promise about a future you have
not designed. Check C5 catches all of them.

**Two browser clients — cut the crippled one.**

`README.md:57–68` spends twelve lines warning the user which of two pages
called `/workspace` they landed on, and telling them that if they landed on the
controller's, "you will conclude the terminal is broken. It isn't; you are in
the test harness." A test harness that ships to users is not a test harness.

**Delete** `internal/relay/workspace.js` (41), `workspace.html` (4),
`workspace_assets.go` (6) and the routes at `internal/relay/tunnel.go:304` and
`:316`. The controller serves no browser UI. Delete `README.md:57–68` and
`:103–104`.

The interim state — "the controller has no browser client" — is honest. "The
controller has a browser client that looks broken" is not. The follow-on, which
is *work and not a cut*, is teaching the one real client (`pixi-client.js`) to
attach through a controller. Sequence it after the cuts; do not let it block
them.

**`internal/zmqx` and `patches/` — keep.** ZeroMQ is the local IPC; it is the
pipe. `patches/zmq4-bound-router-sends-and-frame-allocation.patch` is a
prepared, deliberately unconsumed fix for a live hang documented in
`docs/PLAN-remaining-gaps.md:110-120`. Keep both.
*What would have to be true to cut the patch:* upstream lands the context fix
in `tomi77/zmq4`, or shenmux drops ZeroMQ for the local socket.

**`internal/naming` (50), `internal/appstate` (290), `internal/ttyx` (68),
`internal/webgateway` (505), `internal/webui` (9) — keep.** Endpoint
resolution, CLI config/enrollment state, raw-mode toggling, the browser
client's server, and one `//go:embed`. All in the pipe or one step from it.

### 4.6 Things I was asked to audit that do not exist

Checked with case-insensitive grep over the whole tree, excluding `.git` and
`node_modules`. Reported because a suspected cut that turns out to be nothing
is worth knowing, and because it narrows where the real mass actually is:

| Suspected | Found |
|---|---|
| `urdr` | **zero references** |
| `ratatoskr` | **zero references** |
| `shen-cedar`, `cedar` | **zero references** |
| `rego`, OPA, "open policy agent" | **zero references** |
| tree-shaking artifacts | **zero references** |
| Kubernetes API client (`client-go`, `apimachinery`) | **zero** — `go.sum` is 12 lines |

`internal/policy` is hand-written Go with a two-value string enum
(`observe`/`control`) and JSON-persisted grants. There is no external policy
engine in this repository, and `specs/` contains exactly one file —
`specs/mux.shen` (563 lines), which is Shen, not a policy language.

**`golem`** does exist, but only as `runtime/golem-workspace/` (dying with
§4.1), one Makefile target that is not in `make check`, and prose in
`docs/LOCAL-FIRST-RUNTIME-SPEC.md` and `docs/WORKSPACE-RUNTIME.md:56-62`. Zero
Go code touches it.

**`bifrost` — keep.** It is the Shen spec test harness, not an integration.
`scripts/shen-check.sh` is in `make check` via the `shen` target, prefers
`bifrost run tests/mux-spec.shen`, falls back to `SHEN_BIN`, and fails closed.
`bifrost.suite.json` is 18 lines declaring one case. This is the gate that
keeps `specs/mux.shen` honest. It stays.

---

## 5. The size of the problem, corrected

The owner's measurement was directionally right and numerically wrong in his
own favour, in a way that pointed at the wrong packages.

| Claimed | Actual |
|---|---|
| Accreted Go: 3,633 | **2,137** non-test (the 3,633 counts test files) |
| Core pipe: 9,759 | **9,759** — correct |
| K8s coupling: ~10 refs, "opaque, display only" | 30 refs, and one is a **live filter** on `Namespace`/`Workload` (`tunnel.go:929-933`) |
| Orchestrator branch: ~7,446 | 7,458 on the superseding branch — and **standalone, own `go.mod`, zero `internal/` imports** |

Counting Go LOC understates the problem and mislabels it. The real mass is not
Go:

- ~2,800 lines of browser/Rust/WASM workspace machinery
- 1,899 lines of `Cargo.lock` for two Rust crates nothing ships
- 2,504 lines of `package-lock.json` churn from one merge
- 626 lines of untested Kubernetes YAML
- 921 KB committed `app.bundle.js`, 114 KB committed jco bundle, 56 KB
  committed `.wasm`
- 920 lines of documentation for products this repository has formally refused
  (`VISION.md` 267 + `LOCAL-FIRST-RUNTIME-SPEC.md` 629 + `ROADMAP.md` 24),
  against 3,084 total doc lines — **30% of the documentation describes things
  that are not being built**

And the Go-LOC lens made two concrete errors: it flagged `internal/policy`
(670) as accretion when it is the authentication half of the thesis, and it
missed `internal/supervisor` and `internal/update`, which are dead and are
*exactly* the two refusals R1 and R4.

The single most instructive artifact is commit `9f17e7b`, from yesterday:
"merge agent/wasm-workspace: the V3 WASI workspace UI never landed —
**Restores work that was finished and then stranded on a branch**." 6,686
insertions merged because leaving them unmerged felt like waste. The very next
commit discovered the merged code was unreachable in every configuration.

That is what VISION.md bought. Sunk-cost merging, licensed by a sentence broad
enough to make it sound like progress.

---

## 6. Order, cost, and what is irreversible

Waves are ordered so that nothing later is blocked by anything earlier, and so
the cheap irreversible things happen before the expensive reversible ones.

### Wave 0 — free, no code changes

1. Delete the six already-merged branches and the duplicate k8s remote.
2. Delete `WORKTREE-METADATA.txt`, `SOURCE-SHA256SUMS.txt`, `BUILD-REPORT.md`;
   `rm` the zip from disk.
3. Delete `docs/VISION.md`, `docs/ROADMAP.md`, and
   `docs/LOCAL-FIRST-RUNTIME-SPEC.md`. Land this file. Point `README.md` at it.
4. Tag `archive/golem-job-runtime` at `847b9cf`, then delete the branch.
5. Rename `fix/snapshot-darwin-portability` to `orchestrator/extract-me`.

**Breaks:** nothing. **Irreversible:** no (tags and reflog).

### Wave 1 — dead code

6. `rm -r internal/supervisor internal/update cmd/wsprobe`. Remove
   `./internal/update` from `Makefile:48`.

**Breaks:** nothing. `go build ./...` and `go test ./...` stay green.
**Notices:** nobody. **Irreversible:** no.

### Wave 2 — the workspace cut (largest; ~4,400 lines)

7. Delete the files listed in §4.1.
8. Edit the four references in `internal/webgateway/server.go` and
   `cmd/shenmux/main.go`.
9. Delete the four Makefile workspace targets and `golem-workspace-build`, and
   their `.PHONY` entries.
10. Delete the three dead `webgateway` tests and the two assertions in
    `TestHandlerServesEmbeddedClient`.

**Breaks:** `/workspace` on the gateway; `--workspace-dir`.
**Notices:** nobody — the remote path was unreachable until yesterday and
`make workspace-component-build` is already red on this checkout.
**Data:** OPFS, browser-local, one laptop. No migration.
**Irreversible:** no.

### Wave 3 — the vocabulary cut (one commit; the pieces are coupled)

11. Delete `internal/relay/workspace.js` (41), `workspace.html` (4),
    `workspace_assets.go` (6) and the routes at `internal/relay/tunnel.go:304`
    and `:316`. This is the crippled second browser client, and it is also the
    only consumer of `metadata.pod`/`.harness`/`.namespace`, so it must go
    first or in the same commit as step 13.
12. `cmd/shenmux/main.go:618-625` — delete seven flags, add repeatable
    `--label k=v`. Delete the `Kind`/`Harness` construction at 757–773.
13. `internal/relay/identity.go:102-108,118-119` — delete seven
    `AgentMetadata` fields and three `SessionDescriptor` fields.
14. `internal/relay/tunnel.go:929-933` — delete the namespace/workload filter.
15. Fix `internal/relay/relay_test.go:261,266,280` and delete
    `TestControllerWorkspaceAssets` (`controller_policy_test.go:35`).
16. Delete `README.md:57-68` and `:103-104` — the twelve lines explaining which
    of two `/workspace` pages the user landed on, and the instruction to open
    the one that looks broken.

**Breaks:** the handshake JSON shape, and `GET /workspace` on the controller.
**Notices:** nobody — there is no deployment, every metadata field is
`omitempty`, and after step 14 nothing branches on any of them. The controller
having no browser UI is honest; having one that the README must apologise for
is not.
**Irreversible:** the wire format, in principle. In practice there is no fleet.
This is the item whose cost rises fastest with delay. Do it in the first week.

*Follow-on, which is work and not a cut, and must not block the above:* teach
the one real browser client (`internal/webui/pixi-client.js`) to attach through
a controller.

### Wave 4 — deployment

17. `rm -r deploy/kubernetes scripts/k8s-worker.sh fly.toml
    deploy/fly.toml.example`. Move the sidecar shape into ~10 lines of prose in
    `docs/DEPLOYMENT.md`. Fix the four stale Dockerfile claims listed in §4.5.

**Breaks:** the k3s local-trial demo built on 2026-08-06.
**Notices:** you. Nobody else. If you want the demo, it goes in the control
plane repo, where the cluster is.
**Irreversible:** no.

### Wave 5 — history (**the only item with user data**)

18. Delete `internal/history`, `Runtime.persistHistory`
    (`runtime.go:135,142-147,480,565,732,754`), the `/api/history` route
    (`webgateway/server.go:131,268,280`), and `--history-dir`.

**MIGRATION NOTE — read before deleting.** `shenmux run` has been writing to
`<state-dir>/history` by default since `ba301ff`. Any machine that has run
`shenmux run` has gzip'd protocol archives on disk, typically under
`~/.local/state/shenmux/history/`. Nothing can restore them into a live
session, but they are the user's files and they are not yours to remove.

Ship **one** release that:
- stops writing;
- on startup, if the directory exists and is non-empty, logs its path once with
  a line saying it is no longer written and can be deleted;
- says the same thing in the release note.

Do not delete user files on their behalf. Do not "migrate" them — there is
nothing to migrate them into.

Fix `README.md:156` in the same commit.

**Irreversible:** the feature, yes. The data, no — you are not touching it.

### Wave 6 — one binary (the only wave that adds code)

19. Add `shenmux attach`, folding in `cmd/muxctl`'s 272 lines.
20. Delete `cmd/muxd`, `cmd/shenmux-web`. Update `Makefile`
    (`build`/`install`/`test-deploy`), `Dockerfile`, `deploy/systemd/`,
    `README.md`, `scripts/demo.sh`.

**Breaks:** anything scripting `muxd`, `muxctl`, or `shenmux-web` — which is
`scripts/demo.sh` and you.
**Notices:** you.
**Irreversible:** no, but keep `muxctl` as an installed alias for one release
if `demo.sh` is not updated in the same commit.

This is the only wave that is net-new code. Everything before it is
subtraction.

### Wave 7 — enforcement

21. Resolve the `sb.toml`/Makefile gate duplication (§4.5) before adding
    anything to either. One gate list.
22. Write `scripts/scope-audit.sh` with C1–C5. Wire it into the surviving gate
    list.
23. Add the review question to the PR template: *does the pipe work without
    it?*

This goes last on purpose. Writing the gate before the cut means writing a gate
with an allowlist full of exceptions, and an allowlist full of exceptions is
not a gate.

---

## 7. What is kept, and what would have to be true to cut it later

| Kept | Cut it when |
|---|---|
| `internal/term` (1,963) | Never. Terminal truth is the product. |
| `internal/zmqx` (511) | The local socket stops being ZeroMQ. |
| `patches/` (1 patch) | `tomi77/zmq4` honours the send context upstream. |
| `internal/policy` (670, minus 12 uncalled methods) | Capability issuance moves entirely to the control plane and shenmux only *verifies*. That is a real future; it is not today. |
| `internal/transport` (272, Tailscale) | Direct paths stop being an optimization worth having. It shells out to one CLI and the tunnel falls back; the cost is bounded. |
| `internal/relay` blind-stream code (`blind.go` 734 + `blind_stream.go` 168) | **Watch this one.** It has no shipped client (`TRUST-MODEL.md:84`). 902 lines of cryptography that nothing initiates is the same shape as everything cut above. It survives this round because the threat it addresses — the controller reading your terminal — is real and named in `README.md:138`. If no client initiates it by the time V2 ships, cut it. |
| `internal/appstate` (290) | Enrollment state stops being local. It will not. |
| `deploy/systemd/` (42) | Never. This is the thesis, written down. |
| `internal/webgateway` + `internal/webui` | Never — the browser client needs a server. |

---

## 8. The rule, restated for the next pull request

Before adding anything to `internal/`, answer one question in the commit
message:

> **Does the pipe work without it?**

If the answer is yes, it does not go here. It goes in the control plane, or in
its own repository, or nowhere.

`scripts/scope-audit.sh` catches the cases where the answer is obvious.
Nothing catches the cases where it is not. That is what the question is for.
