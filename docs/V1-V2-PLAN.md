# Implementation status and V1/V2 plan

This is a forward-looking plan, not a release maturity statement. Items marked
implemented exist in the repository; they may still be development-grade.
Production gaps are tracked explicitly instead of being implied by version
labels.

## Stable product boundary

The architecture keeps PTY ownership on one host:

- `shenmux run` owns the PTY and terminal truth;
- local clients use owner-only IPC;
- an agent may bridge named local sessions over an outbound connection;
- a controller authenticates and routes remote streams but does not own or
  persist terminal state.

Local-first replication, offline editing, PTY handoff/migration, Golem, and
collaborative conflict resolution are later research ideas. They are not
current behavior and must not weaken the single-PTY-writer contract.

## Current implementation

| Area | Implemented now | Important limit |
| --- | --- | --- |
| Local session | PTY, Shen-authorized commands, canonical screen, bounded checkpoint/tail, ordered deltas, control ownership, resync | Memory-only; Linux/macOS pure-Go runtime |
| Local clients | `muxctl` and a PixiJS browser gateway | Gateway has no authentication |
| Command surface | `run`, `web`, `login`, `agent`, `controller`, `status`, `version`; legacy commands retained | No install/uninstall or admin commands |
| Agent identity | Single-use enrollment code, Ed25519 device key/proof, persisted credential | No automatic credential rotation/recovery |
| Relay | Authenticated outbound agent WebSocket, reconnect backoff, stream envelope/counters, multi-session bridge | Agent bridges separately running sessions; browser does not auto-reattach |
| Controller policy | Durable JSON grants, capabilities, control leases, revocations, audit metadata; short-lived single-use attach use | Command exposes no real user auth or admin workflow |
| Discovery | Live advertised session names and opaque labels at `/sessions` | Presence is memory-only and not reconciled with actual local processes |
| Trusted browser | Capability issuance, attach, input, checkpoint/delta rendering through `/browser` | Trusted-only; no bundled client attaches through a controller |
| Blind relay | Endpoint handshake/cipher, binding, replay/downgrade/rotation primitives and tests | No bundled initiating client |
| Direct transport | Explicit direct endpoint, Tailscale CLI discovery/ping, `auto` fallback | Still a controller endpoint; no live stream migration |
| Deployment | Example service, Fly, and Kubernetes files | No production image or validated end-to-end deployment |

## V1: coherent local release

V1 should make the local runtime an honest, installable release without
claiming a hosted service.

Remaining work:

1. Package the `shenmux`, `muxctl`, and browser assets for supported Linux and
   macOS targets, including Go 1.26 and Unix socket-path checks.
2. Add install/uninstall and service guidance that matches the actual state and
   IPC paths.
3. Define session lifecycle accurately: live process probing, stale metadata
   cleanup, and clear one-shot versus keepalive behavior.
4. Exercise local browser input, resize, mouse, paste, resync, and multiple
   control clients in end-to-end tests.
5. Decide whether durable checkpoint/journal recovery belongs in V1. Until it
   is implemented, document daemon/host restart as session loss.

V1 acceptance gate:

- a fresh supported host can install dependencies and binaries;
- local mode works without an account or network;
- two local clients cannot both control the PTY;
- refresh/resync does not require restarting the PTY;
- uninstall and failure behavior are documented and tested.

## V2: deployable controller and agent

The existing controller/agent code is a development foundation for V2. It
becomes deployable only after these gaps close.

### Browser identity and policy operations

1. Add a real OIDC/OAuth or pluggable session authenticator; remove direct
   trust in browser-supplied headers and development query subjects.
2. Enforce HTTP/WebSocket Origin policy and CSRF protections appropriate to
   the identity design.
3. Ship administrator flows for grants, device/subject/capability revocation,
   audit review, and credential rotation/recovery.
4. Add enrollment, capability, connection, and frame rate/size controls.

### Durable controller operations

5. Replace or formally scope the single-process JSON stores; add schemas,
   migrations, backups, restore tests, and multi-instance coordination if HA is
   claimed.
6. Define health/readiness semantics, structured logs/metrics, graceful
   upgrades, and rollback.
7. Build and publish an image that actually contains all advertised roles and
   correct the systemd, Fly, and Kubernetes examples against it.
8. Add end-to-end deployment tests. `make test-deploy` must become more than a
   compile check before it can be used as a release gate.

### Remote client quality

9. Teach the local browser client to attach through a controller, keeping the
   keyboard input, paste, resize, focus/mouse modes, reconnect, resync, and
   observe/control UX it already has locally.
10. Reconnect with a new capability and full archive after controller/agent
    outage; never queue arbitrary input while disconnected.
11. Decide how session processes are supervised on an agent host. Do not claim
    that a host reboot preserves an arbitrary PTY.

### Blind and direct paths

12. Ship a blind-capable client and a trustworthy way to obtain/pin the agent
    identity. Complete interoperability, downgrade, recovery, and rotation
    testing through the real browser/agent bridge.
13. Keep blind mode's claim scoped: routing, timing, size, identity-provider,
    browser-distribution, and endpoint compromise remain outside ciphertext
    confidentiality.
14. Test direct/Tailscale selection and relay fallback under failure. Direct
    transport remains an optimization and must use the same authenticated
    session semantics.

V2 acceptance gate:

- an agent behind NAT can be enrolled, revoked, monitored, upgraded, and
  recovered using documented operator flows;
- no development identity shortcut is reachable in production configuration;
- controller restart and transient network failure leave the separate PTY
  alive and clients can explicitly reattach;
- trusted-mode data exposure is visible to users, and blind mode is claimed
  only when a shipped compatible client is in use;
- backup/restore and the advertised deployment targets pass real integration
  tests.

## Validation already available

```sh
nix develop --command make check
nix develop --command make race
make test-relay
make test-deploy
```

`make check` is the main implementation gate. `make test-relay` runs focused
relay, policy, state, transport, update, and command tests. Despite its name,
`make test-deploy` currently performs CGO-disabled command builds only.

Future integration coverage should include enrollment expiry and reuse denial,
agent challenge failure, capability and control denial, revocation of live
streams, controller/agent restart, resync after sequence gaps, blind
ciphertext-only relay observation, Tailscale failure/fallback, browser refresh,
and a controlled 50–100 ms RTT profile.
