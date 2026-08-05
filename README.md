# shenmux

shenmux gives you one terminal session that can be used locally or reached
from a browser. The process that starts the shell keeps ownership of the PTY;
the controller never becomes the terminal server.

The project is written in Go, with Shen defining the pure session policy.
Local IPC uses ZeroMQ. Remote access uses an authenticated, outbound WebSocket
relay, so an agent host does not need an inbound public port.

## Choose a mode

| Use case | Start here | Network required on the agent host |
| --- | --- | --- |
| Local terminal and browser | `shenmux run` + `shenmux web` | None |
| Remote or self-hosted controller | `shenmux controller` + `shenmux agent` | Outbound HTTPS/DNS |
| Lower-latency private path | Add `--transport auto` | Tailscale/WireGuard plus relay fallback |

The trusted-server relay is the default. Blind relay mode is also available
for clients that negotiate it; it encrypts terminal and input payloads before
they cross the controller.

## Quick start: local

With Nix:

```sh
nix develop
make build
```

Start a session and its browser gateway in separate terminals:

```sh
./bin/shenmux run --session work
./bin/shenmux web --session work
```

Open <http://localhost:8787>. For a terminal-only client, use:

```sh
./bin/muxctl -session work
```

The older `muxd`, `muxctl`, and `shenmux-web` commands remain supported.

## Quick start: remote controller

Run the controller on a host with HTTPS/WSS in front of it. The built-in
listener is plain HTTP for local development; put a TLS reverse proxy in front
of any public deployment.

```sh
shenmux controller \
  --listen 0.0.0.0:8788 \
  --origin https://controller.example \
  --state-dir /var/lib/shenmux-controller
```

The controller prints a short-lived, single-use enrollment code. On the host
that owns the PTY:

```sh
shenmux login \
  --controller https://controller.example \
  --code CODE_FROM_CONTROLLER

shenmux agent \
  --controller https://controller.example \
  --transport relay \
  --state-dir /var/lib/shenmux
```

The agent makes outbound WSS connections. The controller stores enrollment,
policy, and session metadata; terminal contents remain on the agent in trusted
mode and are encrypted end to end in blind mode.

To persist blind mode as the minimum allowed trust level:

```sh
shenmux login \
  --controller https://controller.example \
  --code CODE_FROM_CONTROLLER \
  --trust-mode blind
```

For a local development workspace, start the controller with
`--dev-browser-subject local-test` and open
`/workspace?subject=local-test`. Do not enable that development subject on a
public controller.

## Optional Tailscale/WireGuard path

`auto` tries a configured private peer first and falls back to the authenticated
relay when the peer is unavailable:

```sh
shenmux agent \
  --controller https://controller.example \
  --transport auto \
  --direct tailscale://100.64.0.2:8788/ws
```

Use `--transport relay` to force relay-only operation or
`--transport tailscale` to require the private path. A peer can also be
discovered with `--tailscale-peer NAME`.

## Deployments

Deployment contracts, service-manager units, controller backups, and examples
for Fly, AWS, Hetzner, home servers, and Kubernetes are in
[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md). Example assets are in
[`deploy/`](deploy/).

The controller state directory contains owner-only enrollment/device
credentials and policy state. Put it on durable storage and back it up. The
agent state directory contains its device key and credential and must also
survive restarts.

## How it works

- The agent owns the PTY, session history, checkpoints, and reconnect loop.
- Shen validates session commands and returns state transitions plus effects.
- Go performs PTY, terminal, ZeroMQ, persistence, and network effects.
- The controller authenticates agents and browsers, issues short-lived attach
  capabilities, enforces policy, and routes frames.
- A reconnecting browser receives a fresh checkpoint followed by ordered
  deltas; an agent outage does not intentionally kill the local PTY.

More detail is available in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md),
[docs/PROTOCOL.md](docs/PROTOCOL.md),
[docs/TRUST-MODEL.md](docs/TRUST-MODEL.md), and the
[V1/V2 plan](docs/V1-V2-PLAN.md).

## Build and test

Requirements for a native build are Go 1.23+, libzmq 4.x, and a C toolchain.
The Nix shell supplies the reproducible toolchain and native dependencies:

```sh
nix develop
make check
make race
make build
```

`make check` runs Go tests, vet, builds, generated-code checks, and the Shen
verification gate. `make race` runs the Go race detector. A no-Cgo build is
also checked with:

```sh
CGO_ENABLED=0 go build ./...
```

The optional `libghostty-vt` adapter can be built with `-tags libghostty` when
its compatible headers and library are installed.

## Important limits

- The built-in controller listener is plain HTTP; public deployments need a
  TLS reverse proxy and a real browser identity provider.
- The default browser workspace uses the trusted relay mode. Blind mode is a
  protocol capability for clients that perform the blind handshake.
- Local ZeroMQ endpoints are owner-protected but are not a public network
  security boundary. Do not expose them directly to untrusted networks.
- Windows PTY support is not implemented.

## Project layout

The protocol and reducer source live in [`specs/`](specs/). The main runtime
packages are under [`internal/`](internal/), while compatibility commands are
under [`cmd/`](cmd/).
