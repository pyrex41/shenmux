# Local-First Replicated Runtime

## Status

Proposed product and architecture specification. This describes a new product
direction rather than a compatibility change to the current shenmux daemon and
browser prototype.

The working product name is **shenmux Local-First Sessions**. The name is
deliberately provisional; the important idea is the execution model.

## Executive summary

shenmux Local-First Sessions runs a durable session runtime on the node the user
is actively using. That node is the session leader and is optimized for
immediate interaction. The server and dormant clients are followers: they
receive an append-only event stream and periodic snapshots, persist what they
have received, and converge toward the leader's state.

The leader is the only node allowed to produce session events during an epoch.
This makes the normal path simple: there is one writer, one order, and no
consensus round-trip for every keystroke. The server still acts as the fencing
authority for leadership epochs and as the durable rendezvous point for
replication. It does not need to be on the input hot path.

The design has two intentionally separate notions of determinism:

1. The session protocol and state reducer are deterministic. Given the same
   initial snapshot and ordered events, every replica computes the same state.
2. The shell, PTY, filesystem, clock, network, and external processes are not
   assumed to be deterministic. Their authoritative outputs are captured as
   events by the leader and replayed by followers.

Golem is a runtime option for the durable session worker. Its durable execution,
oplog recovery, retry controls, and snapshotting are useful implementation
primitives, but the shenmux replication protocol remains explicit and
versioned. Golem's internal oplog is not the public wire format.

## Product definition

### User promise

Start or open a session on any capable device and get a local-feeling work
surface. The session continues across browser refreshes, laptop sleep, network
changes, and device handoff. A dormant browser or cloud worker can catch up
from the server without taking control. When the user intentionally moves
control, the new node becomes the leader and the old leader becomes a follower.

### Primary surfaces

- **Leader node:** a native CLI/desktop client or a local service that runs the
  session worker and owns the PTY. This is normally the user's current device.
- **Cloud follower:** a server-side durable worker that persists the event log,
  serves reconnects, and can become leader after an explicit or fenced
  promotion.
- **Browser follower:** a web client that renders snapshots and live events,
  provides observation, and can request control transfer. It does not become a
  leader unless a supported local runtime is available.
- **Native follower:** another CLI or desktop client that can observe, cache,
  or eventually receive leadership.

### Product boundary

This is not a distributed shell that independently executes the same command
on every replica. It is a replicated durable session runtime with one active
execution authority and many convergent observers.

## Goals

### G1. Local interaction latency

The leader must be able to accept input, update local UI state, and render
without waiting for a server acknowledgement. The target is one display frame
for local interaction under normal load (less than 16 ms p95 from input event to
scheduled render, excluding the process's own response time).

### G2. Server-side durability

The leader continuously streams events and snapshots to the server. The server
retains enough data to restore the latest committed state, allow a follower to
catch up, and promote a new leader after fencing the old one.

### G3. Deterministic protocol state

The reducer, sequence rules, control lease, permissions, and terminal-state
model must be deterministic and portable. State hashes allow replicas to detect
divergence instead of silently displaying different sessions.

### G4. Explicit leadership

At most one leader may produce accepted events for a session epoch. The normal
leader is the node currently used by the user. Leadership transfer is explicit,
observable, and fenced by a monotonically increasing epoch.

### G5. Offline tolerance

The leader may continue during a temporary server outage. It must spool events
locally, expose that the server is behind, and reconcile on reconnect. Offline
operation must not silently create two concurrent leaders.

### G6. Efficient catch-up

A follower can join using a compact snapshot plus a bounded suffix of events. It
must not require replaying an entire session from process start.

### G7. Portable execution

The same session component should be runnable in a local container, a cloud
container, or another supported Golem deployment with the same event semantics.

### G8. Safe handoff

The user can move control from the current leader to another authorized node
without losing accepted input or creating a split-brain writer.

## Non-goals

- Making arbitrary POSIX processes, filesystems, clocks, and networks globally
  deterministic.
- Running a full duplicate shell on every follower.
- Using a browser as a trusted execution environment by default.
- Sending every keypress through a server-side consensus or durable RPC before
  local rendering.
- Treating Golem's private implementation log as a stable client protocol.
- Automatically resolving concurrent edits to one PTY stream with CRDT
  semantics. A terminal session has one input writer per epoch.

## Consistency model

### Single-writer leader, eventual followers

The session uses a leader/follower model. During epoch `E`, exactly one
authenticated node has the write lease. The leader assigns local sequence
numbers and emits events in order. Followers apply events in order and may lag.

The server provides:

- leader lease issuance and renewal;
- epoch fencing;
- durable append and deduplication;
- snapshot storage;
- follower catch-up;
- promotion authorization.

The server does not have to synchronously acknowledge every leader event. The
server's role is coordination and durable replication, not the normal input
arbiter.

### Epochs and fencing

Every session has a monotonically increasing `epoch`. A leader lease is valid
only for one epoch. The server accepts an event only when its epoch and leader
identity match the current lease. An old leader may continue running locally
after losing connectivity, but its events are marked uncommitted and cannot be
merged into a newer epoch without an explicit recovery decision.

The server is therefore a coordination witness even when it is not on the hot
path. Automatic promotion requires contact with the witness. If the witness is
unavailable, the safe behavior is to remain in offline-leader mode and prevent
another node from claiming the same session.

### What “eventually consistent” means here

Followers may be behind in sequence, snapshots, presence, and displayed
output. They must not invent accepted events. A follower can display a stale
session, show a replication-lag indicator, and request a new snapshot. It may
not issue input unless it owns the current lease.

## Runtime model

The runtime is split into a deterministic component and an effect adapter.

```text
              deterministic session component
        ┌────────────────────────────────────────┐
        │ reducer + state + sequencing + hashes   │
        │ permissions + lease + snapshot metadata │
        └────────────────────────────────────────┘
                    │ declarative effects
                    ▼
        ┌────────────────────────────────────────┐
        │ leader-only effect adapter             │
        │ PTY, shell, filesystem, signals, I/O    │
        └────────────────────────────────────────┘
```

The component receives typed commands and external observations. It returns a
new state, an event, and declarative effects. The effect adapter performs the
impure operation and feeds the authoritative result back into the component.

### Leader execution

1. The leader receives user input or an external process observation.
2. The reducer validates the command and allocates the next leader sequence.
3. The effect adapter writes to the PTY or performs the requested operation.
4. PTY output, resize results, process exit, and other observations become
   authoritative events.
5. The leader commits the event to its local durable spool.
6. The local UI renders immediately from the committed or explicitly marked
   speculative state.
7. Replication sends events and snapshots to the server asynchronously.

For input, the local event is accepted before server replication. If the PTY
write itself fails, the leader emits a typed failure and the local UI rolls back
or resynchronizes.

### Follower execution

1. The follower obtains a snapshot at sequence `S`.
2. It applies events `S+1...N` in order.
3. It verifies each event's previous hash and resulting state hash.
4. It publishes the resulting state to its local renderer.
5. It acknowledges the highest contiguous sequence received.

A state-hash mismatch or sequence gap pauses normal application and requests a
new snapshot. The follower never guesses through a gap.

## Event and snapshot format

The public replication envelope is independent of Golem's internal format.

```json
{
  "version": 1,
  "session_id": "sess_01...",
  "epoch": 42,
  "leader_id": "device_abc",
  "leader_seq": 1842,
  "kind": "pty_output",
  "payload": "...",
  "previous_hash": "sha256:...",
  "state_hash": "sha256:...",
  "created_at": "2026-08-04T00:00:00Z"
}
```

The wire implementation may use CBOR or protobuf rather than JSON. The logical
fields are stable:

- `session_id`: immutable session identity;
- `epoch`: fencing epoch;
- `leader_id`: authenticated node that authored the event;
- `leader_seq`: strictly increasing sequence within the epoch;
- `kind`: versioned typed event kind;
- `payload`: bounded event data;
- `previous_hash`: hash-chain link;
- `state_hash`: reducer state after the event;
- `created_at`: diagnostic metadata, never used for state decisions.

### Event kinds

Initial kinds:

- `input`: bytes submitted to the PTY;
- `pty_output`: bytes read from the PTY;
- `resize`: terminal dimensions and result;
- `process_exit`: exit status and signal;
- `control`: attach, detach, focus, or mode transitions;
- `session_metadata`: name, labels, or configuration changes;
- `checkpoint`: snapshot reference and state hash;
- `error`: an authoritative rejected or failed operation.

The event log must preserve output bytes, not only inputs. Replaying inputs into
a real shell cannot guarantee the same output because the shell's environment
is external and time-dependent.

### Snapshots

A snapshot contains:

- protocol and schema versions;
- session ID and epoch;
- highest contiguous event sequence;
- reducer state;
- terminal model/state required by the client;
- PTY metadata and process status;
- event-log head hash;
- optional compressed scrollback/checkpoint data.

Snapshots are taken periodically and/or after a configured number of events.
The server retains at least the latest snapshot plus a suffix of events. A
leader retains a local snapshot and an append-only spool so it can recover while
the server is unavailable.

## Leadership lifecycle

### Session creation

1. An authorized client requests a new session.
2. The server creates a session record and epoch `1`.
3. The requesting device registers a public identity key and receives the
   leader lease.
4. The local runtime starts the Golem component and PTY adapter.
5. The runtime publishes an initial snapshot.

### Lease renewal

The leader renews its lease periodically. Renewal is a control-plane operation,
not an event for every keypress. A lease contains a TTL, epoch, leader ID, and
server-issued fencing token.

If renewal fails, the leader enters `degraded` mode:

- local interaction may continue;
- replication is spooled locally;
- a visible status indicator reports server lag;
- leadership transfer and remote control are disabled;
- the runtime cannot claim a new epoch without the server.

### Handoff

An explicit handoff is a short protocol:

1. Current leader flushes local events and publishes a final snapshot.
2. Current leader requests handoff to target device.
3. Server verifies authorization, fences the old lease, and increments epoch.
4. Target obtains the final snapshot and receives the new lease.
5. Old leader becomes a follower and rejects new input.

If the old leader cannot be contacted, the server may promote a follower only
after the old lease expires or is explicitly revoked. The new epoch prevents
late events from the old leader from being accepted.

### Crash recovery

On restart, a node loads its latest local snapshot and replays its local spool.
It then compares its head hash with the server. Events already accepted by the
server are deduplicated by `(session_id, epoch, leader_seq, previous_hash)`.
Unaccepted events are either appended if the epoch is still valid or placed in a
quarantine requiring user/server resolution if a newer epoch exists.

## Golem integration

### Role of Golem

Golem is a candidate implementation runtime for the deterministic component and
its durable state. Its durability controls, recovery/replay behavior, retry
policies, and custom snapshots map naturally to session lifecycle and
checkpointing. See the [Golem durability documentation](https://learn.golem.cloud/v1.5/develop/durability),
[snapshotting documentation](https://learn.golem.cloud/v1.5/develop/snapshotting),
and [reliability model](https://learn.golem.cloud/v1.5/concepts/reliability).

The Golem component should own:

- session reducer state;
- event sequence and hash metadata;
- lease/epoch state as supplied by the coordinator;
- snapshot serialization;
- replication cursors and acknowledgements;
- recovery and replay of deterministic state transitions.

The PTY and shell should be treated as an effect adapter unless experiments
prove that a fully controlled process sandbox can be packaged safely and
portably. Golem's atomic blocks are about replaying durable external effects;
they are not an in-memory STM system, and an external effect may be replayed
after a failure. See [atomic blocks and durability controls](https://learn.golem.cloud/v1.5/how-to-guides/ts/golem-atomic-block-ts).

### Local deployment

A native client may run the Golem runtime in a local container or bundled
service. This gives the active user a local leader and a uniform component
boundary. The local runtime must have:

- a local durable data directory;
- a PTY adapter with bounded output queues;
- a secure device identity;
- a replication connection to the server;
- an explicit offline/degraded status.

The browser cannot assume access to a local container. Browser-only clients are
followers unless a separate supported local runtime is installed.

### Cloud deployment

The same component can run as a cloud follower for persistence, catch-up, and
promotion. Cloud execution should not be required for normal keystroke
latency. It becomes leader only after a lease transition and a PTY/process
strategy is available in the cloud environment.

### Do not expose Golem internals as protocol

Golem's oplog and snapshots are implementation details. The shenmux event
schema must remain stable across local runtime versions, cloud versions, and
non-Golem clients. A future runtime could implement the same reducer using a
different WASM host without invalidating stored sessions.

## Client behavior

### Leader UI

The leader UI has three visual states for local events:

- `pending`: accepted by the local reducer but not yet durably replicated;
- `replicated`: accepted by the server;
- `rejected`: invalidated by the leader or a later authoritative state.

For ordinary shell echo, the UI should avoid duplicating text that will arrive
as PTY output. Safe local predictions are limited to cursor movement, input
composition, and explicitly modeled terminal behavior. TUI output remains
authoritative.

### Follower UI

Followers display:

- current replicated sequence;
- leader identity and epoch;
- replication lag;
- whether the view is a snapshot, live follower, or stale cache;
- a clear control-transfer action when authorized.

Followers may select, search, and inspect scrollback without becoming writers.

### Reconnect

Reconnect begins with the last contiguous sequence and hash known by the client.
The server returns either:

- the missing event suffix;
- a newer snapshot plus suffix;
- a conflict/fencing response requiring handoff or resync.

The client never silently clears local pending state. It either confirms,
reconciles, or marks it as unresolved.

## Replication service

The server needs a small replication service separate from the current browser
rendering gateway.

### Responsibilities

- authenticate devices and verify leader leases;
- append and deduplicate event batches;
- store snapshots and event suffixes;
- maintain per-follower cursors;
- broadcast live events to connected followers;
- issue and fence leadership epochs;
- expose catch-up and inspection APIs;
- report lag and divergence metrics.

### Batch protocol

The leader sends batches by size or short timer, whichever comes first. A batch
contains contiguous events from one epoch and includes the first/last sequence
and hash. The server replies with the highest accepted contiguous sequence.

The server may acknowledge a batch before all followers have applied it. A
leader's local status distinguishes `locally_durable`, `server_accepted`, and
`follower_applied`.

### Backpressure

Replication must not block the PTY writer. If the network is slow:

- the local spool grows within a configured bound;
- old output may be compacted only at snapshot boundaries;
- input and process execution remain local;
- the UI reports degraded replication;
- exceeding the bound pauses or terminates the session according to policy.

Lossy compaction is never allowed for accepted input or control events. PTY
output may be coalesced into larger records but not discarded when exact replay
is enabled.

## Security and trust

### Device identity

Every possible leader has a device keypair. Event batches are authenticated by
the leader key and bound to a server-issued lease. A browser session token is
not sufficient to become a leader.

### Local runtime trust

Running the leader locally means the local machine can observe session data and
may control the local PTY. A compromised device can produce valid-looking local
events until the server revokes its lease. The server remains responsible for
authorization, lease fencing, and audit records.

### Server trust

The server stores event logs and snapshots by default. End-to-end encryption of
terminal contents is a separate product decision because server-side replay,
search, and debugging require access to decoded state.

### Secrets and side effects

The local or cloud PTY adapter must never place credentials, environment
secrets, or arbitrary external effects in the deterministic reducer state.
Only typed results and bounded metadata should enter the event stream.

## Failure modes

| Failure | Expected behavior |
|---|---|
| Server temporarily unreachable | Leader continues locally, spools events, shows degraded replication. |
| Leader crashes | Server retains latest accepted snapshot/suffix; a promoted follower starts a new epoch. |
| Follower misses events | Follower requests suffix or snapshot; no gap-skipping. |
| Old leader reconnects after promotion | Its old-epoch events are rejected or quarantined. |
| Local spool is full | Apply configured fail-safe policy; never silently lose input events. |
| State hash mismatch | Pause follower, record divergence, request a trusted snapshot. |
| PTY process exits | Leader emits one authoritative `process_exit` event; followers converge. |
| Handoff interrupted | Old epoch remains fenced or valid according to the server's atomic handoff result; no ambiguous dual writer. |

## Observability

Every event and replication batch carries a trace ID in diagnostic metadata.
Metrics should include:

- input-to-local-render latency;
- PTY input-to-output latency;
- local spool depth and age;
- server replication lag in events and milliseconds;
- follower catch-up duration;
- snapshot size and replay duration;
- state-hash mismatch count;
- lease renewals, expirations, and handoffs;
- rejected old-epoch events;
- PTY/output queue depth.

The product must distinguish user-perceived latency from replication latency.
The former should remain low even when the latter temporarily increases.

## Testing strategy

### Deterministic reducer tests

- same snapshot plus event sequence produces identical state and hash;
- malformed, duplicated, reordered, and old-epoch events are rejected;
- every command has a stable acceptance/rejection trace;
- snapshot plus suffix equals full replay;
- handoff increments epoch and fences the old leader.

### Replication tests

- disconnect and reconnect at every batch boundary;
- duplicate batch delivery;
- delayed and reordered follower acknowledgements;
- server restart during append;
- leader crash before and after local spool commit;
- promotion while the old leader is partitioned;
- state-hash divergence and snapshot recovery.

### Effect-adapter tests

- PTY write failure;
- PTY output burst and backpressure;
- resize serialization with output;
- process exit races;
- bounded local spool and queue behavior.

### End-to-end matrix

Run the same scenario with:

- local Golem runtime and cloud Golem runtime;
- native leader and browser follower;
- server available, delayed, and unavailable;
- shell prompt, alternate-screen TUI, resize, paste, and mouse reporting;
- laptop sleep/wake and process restart.

## Delivery plan

### Phase 1 — protocol prototype

- Define the event envelope, epoch, hash, and snapshot formats.
- Implement a pure session reducer independent of Golem.
- Add a local append-only spool and replay tool.
- Add state-hash verification and a deterministic test fixture.

### Phase 2 — local leader and server follower

- Put the existing PTY runtime behind the effect-adapter boundary.
- Add asynchronous event batching to a server append endpoint.
- Implement reconnect, deduplication, and follower catch-up.
- Add replication status to the browser prototype.

### Phase 3 — leadership and handoff

- Add device identity, leases, epochs, and fencing.
- Implement explicit browser/native control transfer.
- Add cloud follower promotion and stale-leader quarantine.

### Phase 4 — Golem runtime experiment

- Implement the deterministic component as a Golem agent.
- Compare local-container and cloud-container behavior.
- Use custom snapshots and recovery tests.
- Keep the public shenmux event protocol unchanged.

### Phase 5 — product hardening

- Signed releases and device enrollment;
- encrypted replication transport;
- retention and compaction policies;
- multi-session inventory;
- replay/export tooling;
- operational dashboards and divergence alerts.

## Open decisions

1. Whether the first local runtime is a native process, Docker/OrbStack
   container, or a bundled WASM host.
2. Whether PTY output is retained indefinitely, compacted at snapshots, or
   encrypted and retained only for a configured period.
3. Whether the server can promote a cloud PTY automatically or only after a
   user-approved handoff.
4. Whether terminal state is represented by the current typed screen model, a
   Ghostty-compatible serializer, or a separate deterministic terminal VM.
5. Whether the server stores decoded event payloads or only encrypted blobs.
6. How much offline operation is allowed before input is paused.
7. Whether browser leadership is ever supported, or reserved for native nodes.

## Acceptance criteria for the first proof of concept

The architecture is validated when all of the following work:

1. A local leader starts a session and responds without server round-trips.
2. A server follower receives events and snapshots asynchronously.
3. A second client joins from a snapshot and catches up without PTY replay.
4. The server can disconnect while the leader continues and later reconcile.
5. A follower can be promoted with a new epoch after the leader is fenced.
6. Duplicate, delayed, and old-epoch events do not create divergent accepted
   state.
7. A deterministic reducer test produces the same state hash in local and cloud
   runtimes.
8. The user can see whether a state change is local-only, server-accepted, or
   fully caught up.
