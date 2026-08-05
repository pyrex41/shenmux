# shenmux vision

## Status

This is a north-star product document, not a description of the current
repository. The implemented local and development-controller paths are
documented in the [README](../README.md); production gaps and release gates are
tracked in [V1/V2-PLAN.md](V1-V2-PLAN.md).

## The product

shenmux is a durable, identity-aware work surface for terminals, coding
agents, background jobs, and production sessions.

The promise is simple:

1. Install one small binary on a server.
2. Sign in from a browser or native app.
3. See the server and its sessions.
4. Click a session and continue exactly where it was left.

The server should not need a public SSH port, an exposed ZeroMQ socket, a
reverse proxy, or a hand-written VPN configuration.

The terminal is the first surface. The long-term product is a persistent,
composable work session that can be observed and controlled by people and
software.

## North-star experience

```text
curl -fsSL https://get.shenmux.dev | sh
sudo shenmux agent install
shenmux login
```

Then open the web application, choose a device, and choose a session:

```text
prod-api-1
├── shell
├── deploy
└── incident-2026-08-03
```

The session survives a laptop sleep, a browser refresh, a network change, or
an agent reconnect. A new client receives an authoritative checkpoint and then
typed state transitions; it does not replay an unsafe terminal byte stream.

## System shape

```text
                 browser / native clients
                           │
              identity + encrypted session channel
                           │
               hosted or self-hosted controller
                 │       │       │
          identity     ACLs    relay coordination
                           │
                  outbound agent connection
                           │
                    shenmuxd on the server
             ┌─────────────┼─────────────┐
          PTYs       session supervisor   local admin API
       terminal VM   checkpoints/history   Unix socket
```

The controller coordinates identity, policy, presence, and connectivity. It
does not own PTYs or become the source of terminal truth. The agent owns the
interactive process and its canonical interpreted state.

## One binary on the server

The eventual server artifact is one statically distributed Go binary:

```text
shenmux agent       start/reconnect the server agent
shenmux login       enroll the device through an identity provider
shenmux status      inspect connectivity and sessions
shenmux session     create, attach, stop, and rename sessions
shenmux web         expose the local browser surface
```

The agent contains:

- a session supervisor for multiple named PTYs;
- the authoritative terminal/state runtime;
- bounded checkpoints and replay history;
- the outbound control/data tunnel;
- a local administrative socket;
- shell and prompt defaults that never modify a user's dotfiles.

The current `muxd` and browser gateway are useful prototypes of these pieces.
They should converge into the agent without changing the checkpoint/delta
protocol.

## Connectivity strategy

### First product release: outbound relay

Start with an outbound TLS connection from the agent. This is the shortest path
to “it just works” behind NAT, firewalls, and cloud security groups:

- the agent makes the connection;
- the browser connects to the service;
- the service multiplexes encrypted session frames;
- no inbound port is required on the user's server.

The relay must not become the source of terminal truth. V1 uses an explicitly
labelled trusted-server TLS mode so the first vertical slice can reuse the
current checkpoint/delta implementation; V2 adds blind end-to-end encrypted
session payloads between the authorized client and agent. The product must not
claim blind relay before that V2 mode passes its cryptographic and recovery
gates.

### Target architecture: direct encrypted paths with relay fallback

The longer-term transport should follow the proven shape of Tailscale:
WireGuard-based encrypted links, a coordination service for keys and policy,
direct UDP when possible, and encrypted relay fallback when it is not. Tailscale
describes this as direct, DERP-relayed, and peer-relayed connections, all with
WireGuard encryption ([connection types](https://tailscale.com/docs/reference/connection-types),
[system overview](https://tailscale.com/blog/how-tailscale-works)).

The transport interface should make this an upgrade rather than a rewrite:

```text
SessionTransport
├── TLS relay (initial)
├── direct WireGuard path
└── encrypted relay / DERP path
```

The browser can eventually use an ephemeral WebAssembly node for a direct
WireGuard session. Tailscale's browser SSH Console is a useful precedent: it
runs the client networking stack and SSH client in the browser and removes the
ephemeral node when the session ends ([browser console](https://tailscale.com/docs/features/tailscale-ssh/tailscale-ssh-console)).

We should use the open protocols and libraries as building blocks, not depend
on or imitate a private control-plane implementation.

## Identity and security

The hosted product should outsource user authentication to OIDC/OAuth or SAML
providers. The control plane needs to know who is operating a session, but the
agent's private device key must remain on the agent.

Enrollment should be a short-lived, single-use flow:

1. User signs in to the web app.
2. User creates an enrollment request and receives a short code or URL.
3. The server runs `shenmux login --code ...`.
4. The agent generates its key locally and exchanges only public material.
5. The code expires immediately after enrollment.

Every session operation is authorized by both identity and capability:

- read-only observation is the default;
- control is an explicit lease;
- input, resize, and destructive actions require control;
- organizations can restrict devices, users, and session names;
- high-risk sessions can require reauthentication or approval;
- connection and control events are auditable without recording terminal
  contents by default.

The controller should retain the minimum metadata needed for coordination:
device identity, session presence, policy decisions, and connection metadata.

## Browser surface

The web client should feel like a workspace, not a raw terminal emulator:

- PixiJS for a composited, GPU-friendly terminal and pane surface;
- ShenScript for session/pane state policy, commands, and composability;
- typed screen deltas on the hot path;
- reconnect from checkpoint before consuming live deltas;
- readable terminal widths by default, with explicit max-column settings;
- panes, tabs, scrollback, selection, search, and live sharing;
- native-looking controls for read-only observation and control transfer.

ShenScript should not run once per cell or once per frame. It belongs in the
state/command layer; PixiJS owns rendering and animation; the Go protocol owns
authoritative state.

## Configuration

Use one small TOML configuration with predictable precedence:

```text
CLI flags > environment > user config > built-in defaults
```

Example:

```toml
name = "prod-api-1"
control_url = "https://app.shenmux.dev"
shell = "auto"
prompt = "starship"
max_columns = 120
max_rows = 48
history_rows = 2000
```

The same binary should support hosted and self-hosted control planes through a
single `control_url` setting. A local-only mode should remain available for
development and air-gapped environments.

## Delivery plan

### Phase 0 — consolidate the prototype

- Fold the session daemon and web gateway behind one agent command.
- Keep ZeroMQ local-only or replace it with the local admin socket.
- Add multi-session supervision.
- Ship the reconnecting browser client and shell/prompt defaults.
- Preserve the canonical checkpoint/delta protocol.

### Phase 1 — hosted “just works” path

- Add OIDC login and device enrollment.
- Add an outbound agent tunnel and regional relay service.
- Add device/session inventory and ACLs.
- Add agent update checks and signed releases.
- Make the hosted browser path the default.

### Phase 2 — private encrypted networking

- Add WireGuard-based direct paths.
- Add encrypted relay fallback and endpoint health selection.
- Add ephemeral browser nodes for native end-to-end paths.
- Make the relay blind to terminal/session contents.

### Phase 3 — work surfaces

- Multiple panes and durable jobs in one session.
- Agent/background-job attachments.
- Live collaboration and control handoff.
- Native desktop/mobile clients.
- Policy-driven automation and audit integrations.

## Non-goals

shenmux is not initially:

- a general-purpose VPN;
- a replacement for every SSH workflow;
- a cloud-hosted PTY that loses state when a tab closes;
- a terminal byte replay system;
- a requirement to expose public TCP ports;
- a browser framework that runs ShenScript in the rendering hot loop.

The differentiator is durable, safe, composable interactive state with an
excellent client experience.

## Success criteria

The first product milestone is successful when a user can:

1. install the binary on a fresh Linux server without Docker;
2. enroll it using a browser login and one short-lived code;
3. open a named shell from the website without opening an inbound port;
4. close the browser, reconnect from another device, and resume state;
5. observe read-only from one client while another holds control;
6. revoke the server or user without rotating every other device;
7. run the same agent against a hosted or self-hosted controller.
