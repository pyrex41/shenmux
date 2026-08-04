# shenmux

A Shen-controlled terminal session multiplexer with a Go host runtime, a real Unix PTY, an authenticated outbound relay, and an optional `libghostty-vt` state engine. ZeroMQ remains the local IPC transport.

The design has one authoritative process and PTY writer, many attached readers, a targeted snapshot followed by an ordered broadcast stream, and a single sequence spanning PTY output, resizes, and process exit. Shen owns the pure control-plane reducer: it validates commands, evolves session state, returns rejection reasons, and describes effects. Go owns the impure edges (PTY, terminal, ZeroMQ, clocks, persistence, and effect execution).

> Status: V2 relay and transport foundations are implemented. The daemon/client, PTY, local IPC, authenticated enrollment, reconnecting agent tunnel, browser capability endpoint, blind relay framing, policy/revocation metadata, and direct Tailscale/WireGuard path selection are covered by tests. The optional `libghostty-vt` adapter is present but was not linked in this build environment.

## Architecture

```text
                                  control plane
 muxctl / GUI client  DEALER  ---------------------->  ROUTER
       |                                                |
       | SUB: session/<name>                            | serialized input/resize
       | SUB: ready/<client-id>                         v
       +-------------------- XPUB <--------------- Runtime mutex
                                  data plane            |
                                                        | one writer
                                                  PTY master
                                                        |
                                                   child shell

 Runtime ordering:
   command/event --> Shen reducer --> state + declarative effects
                                      |                 |
                                      |                 +--> Go PTY/ZeroMQ/terminal adapters
                                      +--> one monotonic sequence --> replay journal
                                      |
                                      +--> libghostty-vt (optional VT state)
```

Attachment uses an XPUB subscription barrier. A client subscribes to both the session topic and its unique `ready/<client-id>` topic before sending `attach`. The server does not return the targeted snapshot until XPUB has observed that subscription, avoiding the usual PUB/SUB slow-joiner race. Frames emitted while the snapshot archive is being compressed carry greater sequence numbers and are already queued at the subscriber.

See [Architecture](docs/ARCHITECTURE.md), [Protocol](docs/PROTOCOL.md), and [Trust model](docs/TRUST-MODEL.md).

## V1/V2 binary and relay foundations

The `shenmux` binary is the current binary-first entry point. `shenmux login
--controller URL --code CODE` enrolls an Ed25519 device, `shenmux agent`
maintains the authenticated outbound tunnel, and `shenmux controller` starts
the development/self-hosted relay endpoint. The relay preserves protocol v2
frames and reconnects with bounded backoff.

Agents can use a Tailscale/WireGuard peer when the hosts share a tailnet:

```sh
shenmux agent --controller https://relay.example --transport auto \
  --direct tailscale://100.64.0.2:8788/ws
```

`auto` probes the encrypted tailnet endpoint first and falls back to the
authenticated outbound relay after a bounded failure. Use `--transport relay`
to force relay-only operation, or `--transport tailscale` to require the peer
path. Instead of an explicit endpoint, pass `--tailscale-peer NAME` (and
optionally `--tailscale-port`) to discover an online peer via the local
`tailscale` CLI. The same values may be persisted as `transport`,
`direct_endpoint`, and `tailscale_peer` in
the XDG config file (or supplied with `SHENMUX_TRANSPORT` and
`SHENMUX_DIRECT_ENDPOINT`). No inbound public port is needed for relay mode;
tailnet mode requires the local Tailscale/WireGuard service and a peer address.

V2 relay primitives are implemented in `internal/relay`: blind per-stream
X25519/Ed25519 key confirmation, AES-GCM payload protection, replay and
downgrade checks, and authenticated key rotation. `internal/policy` provides
durable ACL/capability/control-lease/revocation/audit metadata, while
`internal/transport` selects a healthy direct path with relay fallback and
`internal/update` verifies signed update manifests. These are transport and
policy foundations; local-first leadership and PTY handoff remain future work.

### Remote/self-hosted setup

The controller command is a small development/self-hosted endpoint. It prints
a single-use enrollment code when it starts:

```sh
shenmux controller --listen 127.0.0.1:8788 --origin https://controller.example
```

For a remote deployment, place the controller behind a TLS reverse proxy and
publish only HTTPS/WSS. The built-in listener is plain HTTP for local
development; agents use the corresponding `https://` controller URL and
automatically upgrade the relay connection to WSS:

```sh
shenmux login --controller https://controller.example --code CODE_FROM_CONTROLLER
shenmux agent --controller https://controller.example --transport relay
```

The agent persists its device key, token, controller, and transport preference
in the XDG state/config directories. It needs only outbound HTTPS/DNS access
in relay mode. Browser clients authenticate to the controller, request a
short-lived capability from `/capabilities`, and attach over `/browser`; the
controller routes session frames to the enrolled agent. `shenmux web` remains
the local-only gateway for the existing PTY/ZeroMQ path.

### Kubernetes agents and orchestrators

Kubernetes workloads can run `shenmux agent` as a sidecar next to a Codex,
Claude Code, Pi, or orchestration process. The agent advertises cluster,
namespace, workload, pod, node, harness, and session metadata during the
authenticated handshake. The controller exposes metadata-only discovery at
`GET /sessions`, so a client can list available pods, orchestrators, and
harness sessions before requesting a capability and attaching to `/browser`.

The agent supports first-start enrollment with a short-lived
`SHENMUX_ENROLLMENT_CODE`, making it suitable for Jobs, Deployments, and
operator-created pods. Apply the example controller and sidecar assets in
[`deploy/kubernetes`](deploy/kubernetes/), replace the example hostname/image,
and put the controller behind an Ingress with TLS. Agents need only outbound
HTTPS; they do not need a Service or inbound port.

## Build

Requirements:

- Go 1.23 or newer.
- Cgo on Linux or macOS for the working PTY and ZeroMQ transport.
- A libzmq 4.x runtime. Linux links directly to the stable `libzmq.so.5` ABI and does not require ZeroMQ headers. On macOS, install `zeromq` so `-lzmq` is available.
- Node.js and `npm ci --prefix web` for rebuilding the embedded browser assets.
- Optional: Rust with the `wasm32-wasip2` target for regenerating the checked-in workspace component via `make workspace-test`.
- Optional: a current `libghostty-vt` header and library for `-tags libghostty`.

Typical Debian/Ubuntu setup:

```sh
sudo apt-get install libzmq5 build-essential
make check
make build
```

Typical macOS setup:

```sh
brew install zeromq
make check
make build
```

With Nix, enter the reproducible development shell instead:

```sh
nix develop
make check
make build
```

The flake supplies a current Go toolchain (compatible with the module's Go 1.23
minimum), ZeroMQ, pkg-config, and the native build tools for Linux and macOS.

`make check` includes `make shen` and therefore requires a Shen launcher. Install
`bifrost` with a compatible Shen implementation (the default is `shen-go`), or
set `SHEN_BIN` to a supported launcher. The check fails loudly when no launcher
is available; semantic verification is never silently skipped.

The normal build uses the dependency-free VT metadata tracker, replay journal,
and checked-in browser workspace component. Rust is only needed when that
component is regenerated:

```sh
go build -o bin/shenmux ./cmd/shenmux
go build -o bin/muxd ./cmd/muxd
go build -o bin/muxctl ./cmd/muxctl
go build -o bin/shenmux-web ./cmd/shenmux-web
```

A no-Cgo build also compiles, but its PTY and ZeroMQ constructors return an unsupported-platform error:

```sh
CGO_ENABLED=0 go build ./...
```

## Run

Start a local session around the default shell with the binary-first command:

```sh
./bin/shenmux run --session work
```

The legacy daemon command remains supported:

```sh
./bin/muxd -session work
```

With no command, `muxd` starts a login shell, preferring zsh, then fish, then
the `SHELL` environment value. Choose explicitly when desired:

```sh
./bin/muxd -session work -shell fish
./bin/muxd -session work -shell zsh
```

Or run a specific command after `--`:

```sh
./bin/muxd -session work -- bash -l
```

Attach from another terminal:

```sh
./bin/muxctl -session work
```

The local browser client is a small WebSocket gateway and embedded PixiJS
terminal. Run it beside the session, then open `http://localhost:8787`:

```sh
./bin/shenmux web -session work
```

The legacy `shenmux-web` executable remains supported as well.

See [docs/WEB.md](docs/WEB.md) for the prototype's protocol and UI notes.

Press **Ctrl-]** to detach locally. The default IPC endpoints are placed in an owner-only `0700` per-UID directory under the system temporary directory, then scoped by session name. Override them with `-control` and `-data` on both commands; filesystem IPC parents must be real directories owned by the daemon UID with no group/other access. Before removing any stale socket path, the daemon acquires a persistent `0600` advisory lock sentinel for each endpoint; a second daemon targeting the same control or data path fails without disturbing the live session.

For a noninteractive smoke test:

```sh
./bin/muxd -session smoke -- sh -c 'IFS= read -r x; printf "got:%s\n" "$x"' &
printf 'hello\n' | ./bin/muxctl -no-raw -session smoke
```

## Fly.io deployment (legacy local-IPC prototype)

The repository still includes a Dockerfile and `fly.toml` for the original
`muxd`/ZeroMQ prototype. It is not the V2 controller deployment and must not be
exposed as a public service: the TCP transport has no application-level
authentication. Use it only for local testing through `fly proxy`:

Install and authenticate with `flyctl`, change `app` in `fly.toml` to a unique
name, then create and deploy the app:

```sh
fly launch --no-deploy
fly deploy
```

Forward both private ports to the laptop:

```sh
fly proxy 15555:5555 -a <app-name>
fly proxy 15556:5556 -a <app-name>
```

Keep both proxy commands running. In another local terminal, attach with:

```sh
nix develop
./bin/muxctl -session work \
  -control tcp://127.0.0.1:15555 \
  -data tcp://127.0.0.1:15556
```

The default remote shell is zsh. Set `SHENMUX_SHELL=/usr/bin/fish` and
`SHENMUX_SHELL_ARGS=-il` with `fly secrets` or in `[env]` if the image should
start fish instead.

For V2 on Fly, run `shenmux controller` behind Fly's HTTPS ingress (or a TLS
proxy) and run `shenmux agent` on the Fly Machine. The agent then makes
outbound WSS connections; do not publish ports 5555/5556 for remote clients.
The same controller/agent arrangement applies on AWS, Hetzner, and a home
server, with only outbound HTTPS required from the agent host.

## Shen source of truth

The pure model is [specs/mux.shen](specs/mux.shen). It declares the control-plane values, commands, and transitions that determine:

- nonempty client IDs, nonnegative sequence numbers, positive dimensions, and snapshot envelopes;
- attached-client membership;
- the snapshot writer lock;
- input authorization;
- strict next-sequence acceptance;
- PTY, resize, exit, attach, and detach transitions;
- authorization and rejection reasons; and
- the declarative effects that Go must execute after each accepted command.

The generated Go-facing package is an adapter to that reducer, not an independent policy implementation. It carries opaque state and typed command/effect values across the host boundary while preserving Shen's authority. Go executes effects only after the reducer accepts a command; effect failures become explicit runtime failures.

The reducer deliberately does not own raw PTY bytes, terminal cell buffers, ZeroMQ sockets, goroutines, clocks, or snapshot compression. Those are host resources. Shen decides whether an operation is legal and which effect is required; Go performs the effect and commits the resulting journal/publication transaction.

```sh
make guards          # regenerate
make guard-check     # fail on drift
make audit           # guard drift + trusted-package closure
make shen            # execute the Shen reducer/spec checks
```

The project includes `sb.toml` and `bifrost.suite.json`. With `pyrex41/bifrost` and a Shen port such as `pyrex41/shen-go` installed:

```sh
make shen
make bifrost
# or, with Shen-Backpressure installed:
sb gates
```

The Go runtime invokes the generated reducer boundary for control-plane commands and events. It does not put sockets or terminal buffers into Shen; those remain Go-owned effect adapters.

The executable trace suite loads the annotated model with Shen runtime type
checking disabled. Current Shen ports exhaust their inference budget on the
heterogeneous seven-field session reducer. Datatype/function declarations,
required arities, generated-boundary decoding, and Go types remain enforced;
making the complete reducer pass portable `tc +` checking is tracked
separately.

## `libghostty-vt`

Build the server adapter after installing a compatible Ghostty C API:

```sh
go build -tags libghostty -o bin/muxd ./cmd/muxd
```

The adapter currently uses the public terminal lifecycle, VT write, resize, cursor, and active-screen APIs. Ghostty documents `libghostty-vt` as usable for C and Zig across major platforms, while also noting that API signatures are still in flux. Pin a Ghostty commit and adjust include/library paths when integrating it into a reproducible build.

The current public C API does **not** expose the full binary terminal export/import assumed by the initial sketch. Consequently, shenmux snapshots are gzip-compressed ordered replay archives. That is semantically correct because resize events share the same sequence as PTY bytes, but it costs replay time and memory proportional to session history. The `Terminal` interface and snapshot envelope are intentionally isolated so a future native Ghostty serializer can replace the payload without changing the control or data planes.

## Transport

The wire format is a small versioned JSON metadata frame plus an optional binary payload frame. PTY and snapshot bytes are never base64 encoded.

- Control: `ROUTER` server, `DEALER` clients. ROUTER identity is the authoritative client ID; an envelope ID must match it.
- Data: `XPUB` server, `SUB` clients.
- Broadcast topic: `session/<session>`.
- Subscription barrier: `ready/<client-id>`.
- Ordered event kinds: `pty`, `resize`, and `exit`.
- Targeted snapshot response: `attached`.
- Gap recovery: client requests `resync`; server returns a fresh archive at a sequence boundary.

## Tests and gates

```sh
make test            # unit + real PTY/ZMQ IPC integration test
make race            # race detector
make vet
make build
make guard-check
make audit
make check           # Go gates plus mandatory Shen verification
```

The integration test launches `/bin/sh` under `openpty`, attaches a real DEALER/SUB client over IPC sockets, sends input through ROUTER, observes ordered output over XPUB, and verifies the exit frame.

## Important limits

This is not yet a hardened remote service:

- Default IPC sockets are protected by an owner-only per-UID directory, but client identity is still routing identity rather than cryptographic authentication. Do not expose TCP endpoints to untrusted networks without CURVE or another authenticated transport policy.
- The replay journal is bounded by safety ceilings but is not compacted. Long-running noisy sessions should use native terminal snapshots or periodic checkpoint compaction.
- Any attached client can resize the authoritative PTY; the last serialized resize wins.
- The plain `muxctl` client replays bytes into the host terminal and cannot reproduce historical window sizes exactly. A graphical/libghostty client can replay into an off-screen emulator with each recorded resize before rendering.
- Ghostty effects that require responses to the child PTY need an explicit effect callback bridge before the `libghostty` build should be called a complete terminal frontend.
- Windows is not implemented by the PTY/TTY layer, although `libghostty-vt` itself is cross-platform.

## Layout

```text
specs/mux.shen                 Shen protocol and state machine
cmd/shenmux-gen/               fail-closed mux-specific guard emitter
internal/shenguard/            generated opaque types and transitions
internal/protocol/             wire framing and replay snapshot codec
internal/zmqx/                 minimal headerless libzmq C ABI binding
internal/ptyx/                 openpty, winsize, child process group
internal/term/                 basic and libghostty-vt state adapters
internal/server/               runtime, XPUB actor, ROUTER actor, daemon
client/                        reusable DEALER/SUB Go client
cmd/muxd/                      session daemon
cmd/muxctl/                    terminal attach client
bifrost.suite.json             cross-Shen-port agreement suite
sb.toml                        Shen-Backpressure gates
```
