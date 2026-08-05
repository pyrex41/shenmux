# Deployment status and requirements

There are three distinct ways to run shenmux. Only the local development path
is packaged coherently in this repository today.

## 1. Local development

Run the PTY daemon and browser gateway as the same operating-system user:

```sh
./bin/shenmux run --session work
./bin/shenmux web --session work
```

No controller, account, or network access is required. Both processes use
owner-only local IPC. Keep the gateway on its default loopback listener; it has
no authentication.

This is the recommended way to evaluate the terminal runtime and browser UI.

## 2. Self-hosted development controller

The controller is a single plain-HTTP process suitable for an integration
test or a private development network. It stores enrollment and policy JSON in
one local directory.

```sh
./bin/shenmux controller \
  --listen 127.0.0.1:8788 \
  --origin http://127.0.0.1:8788 \
  --state-dir ./controller-state \
  --dev-browser-subject local-test
```

On the agent host, run a session separately, enroll, and advertise that
session:

```sh
./bin/shenmux run --session work

./bin/shenmux login \
  --controller http://127.0.0.1:8788 \
  --code CODE_FROM_CONTROLLER

./bin/shenmux agent \
  --controller http://127.0.0.1:8788 \
  --state-dir ./agent-state \
  --transport relay \
  --session work
```

The agent opens the connection outbound and does not need an inbound public
port. It still needs local access to the session's ZeroMQ sockets. If `run` and
`agent` use different users, containers, or filesystem namespaces, explicitly
share a private IPC directory and pass matching `--control` and `--data`
endpoints.

The browser then opens
`/workspace?subject=local-test`. The development subject grants wildcard
observe/control access and must never be enabled on a public listener.

### What survives restart

- The agent state directory contains its device key, credential, session
  metadata, and reconnect settings.
- The controller state directory contains device/enrollment records and policy
  records.
- An agent or controller reconnect does not deliberately stop a separately
  running local PTY.

The PTY, checkpoint/tail, connected-agent inventory, and live streams are not
persisted. Restarting `shenmux run` loses the terminal process. The bundled
browser workspace also requires a refresh/re-attach after disconnection.

## 3. Production controller

The current command is not a production controller. Putting a TLS proxy in
front of it solves only transport encryption. A deployable multi-user service
still needs all of the following integrations and operating work:

### Identity and authorization

- an actual browser identity provider and session validation;
- removal of client-supplied identity headers at the edge;
- a strict Origin allowlist for HTTP and WebSocket requests;
- administrator interfaces for grants, revocation, credential rotation, and
  audit review;
- CSRF and browser security policy appropriate to the chosen authentication
  design.

### Network and abuse controls

- TLS configuration and renewal for HTTPS/WSS;
- request, connection, frame, and enrollment rate limits;
- edge body/time limits, logging, monitoring, and alerting;
- a decision about trusted relay data exposure or a shipped blind-capable
  client.

### Data and availability

- a reviewed store for concurrent controller instances, schema migrations,
  backup/restore, and credential recovery;
- coordination for capabilities, single-use enforcement, leases, revocation,
  connected agents, and streams if more than one controller instance is used;
- upgrade, rollback, health, readiness, and disaster-recovery procedures.

The current `enrollment.json` and `policy.json` stores use owner-only atomic
file replacement and are useful for one development process. They are not a
SQLite/Postgres implementation and do not coordinate replicas.

### Agent operations

A production agent service would need to:

- run under the same user/security context as the session daemon, or share
  only the required private IPC directory;
- set `SHENMUX_STATE_DIR` or pass `--state-dir` to durable storage;
- enroll once without leaving the bootstrap code in a long-lived environment;
- preserve the device state across upgrades and monitor credential expiry;
- supervise `shenmux run` separately and define what should happen when its
  shell or host exits.

There is no PTY recovery across a daemon or host reboot. `--keepalive` restarts
a shell only while the daemon itself remains running.

## Transport deployment choices

`relay` connects the agent to the controller URL. `auto` can try a configured
direct controller address first and fall back to the controller URL.
`tailscale` requires that direct address or resolves one with the local
Tailscale CLI.

The direct address must expose the same controller `/ws` handler. It is not an
agent listener and does not remove controller authentication or routing. For a
tailnet address, WireGuard supplies hop encryption; use `wss://` as well if
your deployment requires TLS at the application endpoint.

## Checked-in deployment assets

Treat `deploy/` as design material, not an installable distribution.

| Asset | Current reality |
| --- | --- |
| `Dockerfile` and root `fly.toml` | Legacy `muxd` image exposing ZeroMQ TCP internally; it does not contain the `shenmux` controller/agent binary |
| `deploy/fly.toml.example` | Describes an egress-only agent shape but assumes a published image containing `shenmux`; that image is not built here |
| `deploy/systemd/*.service` | Illustrative hardening baseline; environment/state/IPC ownership and enrollment must be completed for the target host |
| `deploy/kubernetes/*.yaml` | Architecture fragments; they assume a compatible image, TLS/identity integration, writable durable state, and a separately running session daemon |

In particular, the Kubernetes controller example sets a read-only root
filesystem without mounting the controller state directory, and the sidecar
example does not currently pass its mounted `/var/lib/shenmux` as
`--state-dir`. Apply neither unchanged.

Fly.io, EC2, Hetzner, a home server, and Kubernetes can all host the same
future controller/agent architecture, but this repository does not claim a
tested end-to-end deployment for any of them. `make test-deploy` currently
checks no-Cgo compilation only.

See [ARCHITECTURE.md](ARCHITECTURE.md) for process boundaries and
[TRUST-MODEL.md](TRUST-MODEL.md) before exposing any interface.
