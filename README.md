# shenmux

shenmux is an experimental terminal multiplexer for one or more named PTY
sessions. A session runs on the machine that starts it. Local clients connect
over owner-only ZeroMQ IPC; an optional development controller can relay the
same session protocol over WebSockets.

The local runtime is the usable core today. The controller, agent, remote
workspace, blind-stream primitives, and Tailscale path are implemented and
tested as development building blocks, but the repository does **not** yet ship
a production-ready hosted service or self-hosting package.

## What can I run?

| Goal | Commands | Status |
| --- | --- | --- |
| Local shell in the browser | `shenmux run` and `shenmux web` | Working local development path |
| Local shell from a native client | `shenmux run` and `muxctl` | Working local development path |
| Relay a running session through a controller | `shenmux controller`, `shenmux login`, and `shenmux agent` | Working development path; trusted browser only |
| Public or multi-user service | — | Requires deployment and security work listed below |

The controller and agent do not create or preserve PTYs. Start `shenmux run`
on the agent host first; the agent only bridges controller streams to that
session's local IPC endpoints.

## Build

The supported runtime platforms are Linux and macOS with Go 1.23+, a C
toolchain, and libzmq 4.x. The Nix development shell supplies these
dependencies.

```sh
nix develop
make build
```

This creates `bin/shenmux`, `bin/muxd`, `bin/muxctl`, and
`bin/shenmux-web`. `CGO_ENABLED=0` builds are useful as a portability check,
but PTY and ZeroMQ operations deliberately return unsupported errors in those
builds. Windows PTY support is not implemented.

## Quick start: local

Start a named session:

```sh
./bin/shenmux run --session work
```

In another terminal, start the local browser gateway:

```sh
./bin/shenmux web --session work
```

Open <http://127.0.0.1:8787>. The gateway listens on loopback by default and
has no authentication. Keep it local. To use the terminal client instead:

```sh
./bin/muxctl -session work
```

`shenmux run` starts a login shell and, by default, restarts it after exit.
Pass a command after `--` for a one-shot session, or use `--keepalive=false`.
The older `muxd`, `muxctl`, and `shenmux-web` commands remain available;
notably, legacy `muxd` defaults to `-keepalive=false`.

## Quick start: development controller

This example is for a private development network. It enables the deliberately
insecure browser subject shortcut and uses plain HTTP.

On the controller host:

```sh
./bin/shenmux controller \
  --listen 127.0.0.1:8788 \
  --origin http://127.0.0.1:8788 \
  --dev-browser-subject local-test
```

The controller prints a single-use enrollment code to stderr. It expires in
ten minutes. On the machine that owns the PTY, start the session and enroll the
agent:

```sh
./bin/shenmux run --session work

./bin/shenmux login \
  --controller http://127.0.0.1:8788 \
  --code CODE_FROM_CONTROLLER

./bin/shenmux agent \
  --controller http://127.0.0.1:8788 \
  --transport relay \
  --session work
```

Open
<http://127.0.0.1:8788/workspace?subject=local-test>. For different hosts,
replace loopback with a private reachable address and keep `--origin` equal to
the URL used by the agent. `shenmux login` permits plain HTTP only for
localhost or an IP address; named controller hosts must use HTTPS.

The remote workspace currently uses trusted relay mode, basic keyboard input,
and a simple screen renderer. It does not implement the richer local browser
client's full input and resize behavior. See [the browser guide](docs/WEB.md).

## Trust modes

- `trusted` is the default and the only mode used by the bundled controller
  workspace. TLS can protect the network hop, but the controller can read
  terminal output and input.
- `blind` is an implemented protocol/library path. It performs an
  authenticated X25519/Ed25519 handshake and encrypts inner session frames so
  the relay sees routing metadata and ciphertext. No bundled user-facing
  browser or native command initiates that handshake yet.

`shenmux login --trust-mode blind ...` persists a minimum mode on the agent.
With the bundled trusted workspace this intentionally makes attachment fail;
use it only while developing a compatible blind client. It is not a switch
that upgrades the bundled browser.

See [the trust model](docs/TRUST-MODEL.md) for the exact guarantees and gaps.

## Transport choices

The `agent` supports:

- `--transport relay`: connect to the configured controller's `/ws` endpoint;
- `--transport auto`: try a configured direct endpoint first, then the
  controller URL; without a direct endpoint it is relay-only;
- `--transport tailscale`: require the configured direct endpoint.

A direct endpoint is another reachable controller `/ws` listener, not a
browser-to-agent connection. Supply one explicitly:

```sh
./bin/shenmux agent \
  --controller https://controller.example \
  --transport auto \
  --direct tailscale://100.64.0.2:8788/ws \
  --session work
```

Or use `--tailscale-peer NAME`; shenmux calls the local `tailscale` CLI to find
and ping the peer. `tailscale://` and `wireguard://` are normalized to plain
WebSocket because the private network supplies hop encryption. `auto` falls
back after connection failure; it is not live stream migration.

## State and restarts

By default, user configuration is stored in
`$XDG_CONFIG_HOME/shenmux/config.json` (or `~/.config/shenmux/config.json`),
and agent/runtime metadata is stored in `$XDG_STATE_HOME/shenmux` (or
`~/.local/state/shenmux`). `SHENMUX_CONFIG_FILE` and `SHENMUX_STATE_DIR`
override them.

```sh
./bin/shenmux status
./bin/shenmux status --json
```

The state file contains device credentials, reconnect metadata, and a list of
sessions observed by `shenmux run`. That list is informational; it is not a
process supervisor or durable terminal journal. Stopping `shenmux run` or
rebooting its host ends the PTY.

The development controller stores `enrollment.json` and `policy.json` beneath
its `--state-dir`. Codes, device records, grants, capabilities, leases, and
audit metadata use those files, while live agent presence and streams remain
in memory. Backing up this directory does not back up terminal state.

## Production status

Do not expose the built-in controller or local browser gateway directly to the
Internet. A real deployment still needs, at minimum:

- TLS termination and strict WebSocket origin policy;
- real browser authentication (the current fallback trusts an
  `X-Shenmux-Subject` header) and an administrator-facing grant/revocation
  workflow;
- rate limits, request/body limits at the edge, credential rotation, and
  recovery procedures;
- a reviewed durable/concurrent data store, migrations, backups, monitoring,
  and an availability design;
- production images/packages and corrected service/Kubernetes wiring;
- a shipped blind-capable client if relay confidentiality is required.

The checked-in deployment files are design examples, not a supported release:
the root Dockerfile and `fly.toml` still package the legacy `muxd` TCP shape,
and the controller/sidecar manifests assume an image and production integration
that this repository does not currently build. Read
[deployment status and requirements](docs/DEPLOYMENT.md) before using them.

## How the implementation fits together

- `shenmux run` owns the PTY, terminal model, bounded in-memory checkpoint and
  delta tail, and local ZeroMQ ROUTER/XPUB sockets.
- The Shen reducer authorizes session transitions and returns effects; Go
  performs PTY, terminal, socket, persistence, and network effects.
- Local clients attach with a checkpoint and then consume ordered deltas. A
  sequence gap triggers resynchronization.
- `shenmux agent` maintains an authenticated outbound WebSocket and maps remote
  streams to local sessions.
- `shenmux controller` enrolls devices, authenticates agent connections,
  issues browser capabilities, tracks control leases, and forwards frames.

Details are in [architecture](docs/ARCHITECTURE.md),
[protocol](docs/PROTOCOL.md), [browser clients](docs/WEB.md), and the
[implementation plan](docs/V1-V2-PLAN.md).

## Test

```sh
nix develop --command make check
nix develop --command make race
```

`make check` rebuilds the web bundle, checks generated Shen guards, runs the
Shen verification gate, Go tests and vet, and both Cgo and no-Cgo builds.
`make race` runs the Go race detector. Focused targets are `make test-relay`
and `make test-deploy`; the latter is currently a compile check, not an
end-to-end deployment test.

The optional `libghostty-vt` adapter requires compatible headers and a library
and is selected with `-tags libghostty`. The default build uses the basic Go
terminal implementation.
