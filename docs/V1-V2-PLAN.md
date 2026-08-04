# V1/V2 architecture and implementation plan

This is the delivery plan for the binary-first product. It keeps the server as
the owner of the PTY and durable session, and leaves local-first leadership,
offline replication, Golem, and PTY handoff to V3 or later.

## Product boundary

**V1 (just works):** one `shenmux` binary can run locally, run as an agent on a
remote host, or run a self-hosted controller. The agent makes only outbound
HTTPS/WSS connections. A controller authenticates users, enrolls devices,
lists sessions, and relays frames. A browser can attach, disconnect, and
reconnect to the same session without restarting the PTY.

**V2 (secure and operable):** hosted and self-hosted controllers have durable
identity/policy, revocation, audit metadata, signed updates, service-manager
installation, multi-session inventory, direct/Tailscale transport when useful,
and a blind-relay encryption mode.

**V3 (future idea):** local-first ownership, replicated event logs, offline
editing, PTY migration/handoff, Golem, and collaborative conflict resolution.
V3 must not change the V1 server-owned PTY contract until migration semantics
and a leader-placement matrix are specified.

## Components and responsibilities

The **agent** owns PTYs, terminal interpretation, checkpoints, the existing
ordered checkpoint/delta protocol, named sessions, local admin IPC, and an
outbound reconnecting connection. It never requires an inbound listening port.

The **controller** owns user authentication (OIDC/OAuth in hosted mode),
device enrollment and revocation, durable device/capability/ACL records,
connection presence, rate/size limits, and multiplexing browser connections to
the correct agent. It caches inventory and presence but stores no checkpoints,
journals, or terminal payloads. It does not own PTYs or terminal truth. In V1
trusted mode it may inspect inner session frames; it must not mutate their
sequence or checkpoint semantics.

The **browser/native client** authenticates to the controller, requests a
short-lived single-use attach capability, and consumes the same `attached`
checkpoint followed by ordered `screen-delta`/`control-owner`/`resize`/`exit`
frames used by local clients. The capability is bound to the browser subject,
device, session, permission, and stream. On a gap it requests `resync`; on a
refresh or disconnect it mints a new capability and receives a fresh
checkpoint in V1. A suffix-resume optimization is explicitly V2 work.

## Transport and framing

Enrollment is an HTTPS bootstrap, not a WSS message: the user creates a
single-use code, and the agent submits that code plus its public key. Only
after the controller returns a device credential does the agent open WSS. The
authenticated connection starts with `hello`, capability/version negotiation,
challenge proof, and `ready`; reconnects send `resume` with the stored device
credential. Browser login and capability issuance use HTTPS; the session
stream uses WSS.

The relay envelope is one binary WebSocket message: a 4-byte big-endian
header length, a bounded header containing version, frame type, request ID,
device/session IDs, stream ID, and an outer transport counter, followed by the
payload. It
wraps the exact existing v2 multipart message (`kind`, JSON metadata, and
optional binary payload) byte-for-byte; it does not translate or invent a
second terminal protocol. Inner kinds are `attach`, `attached`, `input`,
`resize`, `detach`, `ping`, `pong`, `screen-delta`, `control-owner`,
`acquire-control`, `release-control`, `exit`, `error`, and `resync`. Existing
64 KiB metadata, 64 MiB payload, request-ID, checkpoint sequence, and gap rules
remain authoritative. The controller validates the outer counter, stream
binding, capabilities, and resource limits. In blind mode only the agent and
client validate inner sequence and checkpoint contents.

Input and control requests are idempotent by `(stream_id, request_id)`; the
agent acknowledges accepted/rejected requests and may safely replay an
unacknowledged request after reconnect. Data publication is ordered per session
and bounded; when backpressure exceeds the limit the controller closes that
stream and the client obtains a new checkpoint via `resync`.

There are three layers: TLS/WSS authenticates the hop; the relay envelope
binds a capability to a stream and carries an outer counter; the inner v2
message remains the session source of truth. The browser adapter maps the
inner `attached` archive and `screen-delta` messages to the existing UI model.
In V1 a reconnect creates a new stream and always receives a full `attached`
archive; the last sequence is not a promise of delta replay.

### Trust decision

The modes are explicit, negotiated per controller and visible in status:

* **V1 trusted-server mode:** TLS authenticates agent and browser to the
  controller; the controller can authorize and inspect session frames. This is
  the first vertical slice because it is testable with the current checkpoint
  codec and supports self-hosting without browser key-management work.
* **V2 blind-relay mode:** the agent and browser establish an ephemeral
  authenticated per-stream key using a reviewed Noise/HPKE-style transcript,
  with the agent Ed25519 key signing the handshake. HKDF derives keys and
  AEAD protects the entire inner kind/metadata/payload; counters provide nonce
  and replay protection. The controller routes opaque ciphertext and limited
  routing metadata only. Device identity and ACL checks remain controller-side,
  while terminal contents and input bytes are not visible to it.

The protocol carries a `trust_mode` and rejects downgrade unless the user
explicitly enables it. The agent persists a minimum allowed trust mode and
never silently falls back from blind to trusted. The V2 claim is scoped to an
honest identity provider, browser origin/client distribution, and endpoint
keys; a compromised controller that can replace browser code or mint identity
is outside this guarantee. Blind mode still leaks routing, timing, and size
metadata.

## Identity, enrollment, and authorization

Each agent creates an Ed25519 device key in the persistent state directory
(`$XDG_STATE_HOME/shenmux` or the platform equivalent), with owner-only file
permissions. The enrollment code is displayed once, expires (default ten
minutes), is bound to the requesting account, and cannot be reused or
replayed. The agent submits the code and public key over HTTPS; the controller
returns a device ID and device credential. The private key never leaves the
agent. The credential rotates before expiry and is stored owner-only; each WSS
connection proves possession of the Ed25519 key over a controller nonce.

Every browser attach receives a short-lived, session-scoped capability naming
the device, session, permissions (`observe`, `control`), expiry, and nonce.
Input, resize, and destructive session operations require `control`; observing
is the default. Revocation invalidates device credentials and outstanding
capabilities and is propagated to connected agents within a bounded interval.
The agent fails closed for remote streams after revocation or credential
expiry, while an already-running PTY continues locally. Local-only mode uses
the existing owner-only Unix IPC and has no account or controller.

The enrollment record is random, rate-limited, hashed at rest, and consumed
atomically. After enrollment, WSS establishment is `hello` → controller nonce
challenge → agent Ed25519 transcript signature (version, origin, nonces, and
device ID) → `ready`. A browser attach is OIDC/session-cookie authentication
→ HTTPS capability issuance → WSS `OPEN` carrying the capability (never in a
URL query) → agent verification → full `attached` archive. Revocation marks
the credential/capability revoked in a transaction, closes live streams and
tunnels, and blocks reauthentication; stateless tokens alone are insufficient.
The reducer's `control-owner` lease remains separate from the capability's
permission to request `acquire-control`.

## Configuration and deployment contracts

One command surface is targeted: `run` (PTY plus local IPC), `login --controller
URL` (enrollment), `agent --controller URL` (supervise/reconnect
an agent), `controller` (self-hosted controller), `status`, and `web` (local
gateway in local-only mode, controller browser endpoint otherwise). The same
binary is shipped for all roles; hosted controllers are operated by shenmux.
Precedence is CLI, environment, user config, then defaults.

| Mode | Process | Network | Durable state |
| --- | --- | --- | --- |
| Local-only | `shenmux run` | Unix IPC only | XDG state directory |
| Hosted | `shenmux agent --controller URL` | Agent egress TCP 443/DNS; browser WSS to controller | Agent state locally; hosted controller DB |
| Self-hosted | Agent plus `shenmux controller` | Controller HTTPS/WSS ingress; agent egress TCP 443/DNS | Agent state plus controller SQLite/Postgres |

Fly, AWS, Hetzner, and a home server all use the same agent contract: install
one binary, persist `/var/lib/shenmux` (or the documented XDG equivalent),
permit outbound TCP 443/DNS, and run it under a service manager. No inbound
port, public ZeroMQ socket, `fly proxy`, VPN, or port-forward is required.
Fly uses a Machine volume and egress-only networking; AWS uses EC2/EBS and a
security group with no inbound agent rule; Hetzner uses a persistent volume
and firewall egress; home servers use systemd or launchd behind NAT. A
self-hosted controller additionally needs public HTTPS/WSS ingress, TLS
renewal, durable SQLite/Postgres storage, backups, migrations, and health
checks. Existing `fly.toml`/Dockerfile TCP exposure is a migration task and
must be removed before the V1 deployment gate.

## Failure, reconnect, and offline behavior

The agent uses bounded exponential backoff with jitter, a connection deadline,
heartbeat/ping, and a persisted last-success timestamp. It buffers only
bounded control metadata; PTY output remains in the agent's bounded
replay/checkpoint store and is recovered through `attached`/`resync` after
reconnect. A controller outage
does not kill or pause the PTY. Existing browser streams show a reconnecting
state, then open a new stream and receive a fresh checkpoint. Browser input is
not queued or retried while disconnected; the latest resize may be coalesced
and resent after attach. If the agent is offline, local Unix
IPC continues to work; remote access is unavailable and no unbounded queue is
created. If the agent process or host restarts, V1 restores journal
and checkpoint metadata and applies the documented `-keepalive` policy
(restart a shell or mark the prior process exited); it does not claim that an
arbitrary OS PTY survives a host reboot. A controller or tunnel restart must
not interrupt the PTY.

## Milestones and acceptance gates

### V1.1 — consolidate and local-only compatibility

1. Introduce the `shenmux` command with `run`, `login`, `agent`, `controller`, `status`, and `web`
   aliases around the current daemon/gateway; preserve muxd/muxctl compatibility.
2. Add versioned persistent state, session inventory, bounded replay/checkpoint
   persistence, and a local admin API.
3. Freeze the relay envelope and controller/agent state machine, then add
   controller auth, single-use enrollment, device keys, capabilities, and
   revocation storage.
4. Implement trusted-mode WSS hello/challenge/heartbeat, attach,
   checkpoint/delta, resync, and browser reconnect.
5. Add controller outage/reconnect tests and a 50–100 ms RTT benchmark profile.

**Gate:** local-only mode works without an account; an enrolled agent behind
NAT is reachable from a browser; a browser refresh and controller restart do
not lose the PTY; no public ZeroMQ endpoint is needed.

### V1.2 — first deployable product

6. Ship a minimal hosted controller and a self-hosted controller package with
   OIDC pluggability (development login is local-only).
7. Add Fly/AWS/Hetzner/home service files, health/readiness endpoints, logs,
   and documented state-directory backup/restore.
8. Add release version reporting and signed update manifest verification
   (updates remain opt-in).

**Gate:** a fresh Linux host can be enrolled with one command/code; its agent
service, enrollment, and state directory survive reboot; and it is
observable/revocable from the browser. Live PTY continuity across host reboot
is not claimed until a separate worker-reconnect design exists.

### V2.1 — security and policy hardening

9. Implement blind-relay X25519 handshake, encrypted envelope payloads,
   capability-bound key confirmation, replay protection, and explicit mode
   negotiation/downgrade tests.
10. Add durable ACLs, control leases, audit metadata (never terminal contents
    by default), rate limiting, key rotation, and device/session management.

### V2.2 — transport and operations

11. Add optional direct/Tailscale transport behind the same `SessionTransport`
    interface, with encrypted relay fallback and health-based selection. The
    shipped agent accepts `--transport auto|relay|tailscale` and a
    `tailscale://`/`wireguard://` peer endpoint; `auto` prefers the tailnet
    path and retries through the authenticated relay when the peer is down.
12. Add multi-session UI/inventory, native-client API compatibility, signed
    releases with rollback, service-manager install/uninstall, and disaster
    recovery drills.

**V2 gate:** a compromised relay/storage path cannot reveal blind-mode terminal
contents or forge endpoint frames under the stated trust assumptions;
revocation is effective; direct transport is an optimization; relay fallback,
restart, and 100 ms RTT remain transparent to session continuity.

## Validation commands

Every milestone runs `nix develop --command make check` and
`nix develop --command make race`. Add `make test-relay` and
`make test-deploy` as implementation gates. The web latency harness is run
locally and with a controlled 50–100 ms RTT profile. Integration tests cover
local-only network isolation; enrollment expiry/single-use/replay denial;
capability denial and multi-client control; controller outage; tunnel and
agent restart; checkpoint/sequence resync; revocation propagation; and both
trust modes. The V2 blind-mode test must assert that a controller test double
sees only metadata/ciphertext, and covers downgrade, replay, rotation, and
key-confirmation failures.
