# Architecture as implemented

This document describes the code in this repository. Future production work is
kept in [V1/V2-PLAN.md](V1-V2-PLAN.md).

## Local session

```text
shell or command
      ↕ PTY
shenmux run / muxd
  ├─ ZeroMQ ROUTER  ← commands ─ local clients
  └─ ZeroMQ XPUB    ─ deltas  → local clients
                              └─ shenmux web → local browser
```

`shenmux run` is the terminal server. It owns the operating-system PTY, the
authoritative terminal model, the current control owner, and a bounded
in-memory checkpoint plus delta tail. Neither a client, gateway, controller,
nor agent owns the PTY.

Default IPC endpoints live below `/tmp/shenmux-<effective-uid>/`. The directory
must be owned by that user and have no group/other permission bits. Each socket
also has a persistent `0600` lock sentinel; the daemon obtains an exclusive
advisory lock before removing a stale socket or binding. Custom IPC endpoint
parents must satisfy the same checks.

The local gateway (`shenmux web`) and `muxctl` are clients of these IPC
endpoints. They must run as a user that can access them. IPC ownership is the
local security boundary; ZeroMQ messages themselves are not authenticated or
encrypted.

## Remote development path

```text
browser client
      ↕ WebSocket /browser
development controller
      ↕ WebSocket /ws (outbound from agent)
shenmux agent
      ↕ local ZeroMQ IPC
shenmux run / muxd ↔ PTY
```

The processes have separate responsibilities:

- The controller consumes enrollment codes, authenticates an agent's device
  credential and Ed25519 challenge proof, records policy state, lists sessions
  advertised by connected agents, issues short-lived browser capabilities,
  tracks control leases, and forwards stream envelopes.
- The agent reconnects with bounded exponential backoff and maps each remote
  stream to the named local IPC endpoints. It advertises only session names and
  the opaque `--label key=value` pairs supplied on its command line, which
  nothing in shenmux reads.
- The browser asks the controller for a capability, opens a stream, attaches
  to a named session, and consumes the same inner protocol as a local client.

The controller does not start sessions and cannot recover one. Start
`shenmux run --session NAME` separately on the agent host, and advertise the
same name with `shenmux agent --session NAME` or `--sessions A,B`.

Controller restart closes all live agent and browser connections. The agent
reconnects; a browser stream has to be reopened by its client. The PTY
continues only while its separate `shenmux run` process remains alive.

## Control plane

The daemon owns one ZeroMQ ROUTER socket. Each client owns one DEALER socket
whose routing identity is its validated client ID. The ROUTER serializes
attach, input, resize, control acquisition/release, detach, ping, and resync
requests.

After decoding and identity checks, the runtime submits a typed command to the
Shen-derived reducer boundary. The reducer owns membership, control ownership,
exit state, writer-lock state, sequence allocation, rejection reasons, and the
declarative effect list. Go executes effects such as PTY writes, resize,
publication, snapshot capture, and replies.

Only an attached client that owns control may write or resize. Remote
capabilities and controller leases add an outer authorization layer; they do
not replace the local reducer's control ownership.

## Data plane and resynchronization

The daemon owns one XPUB socket and local clients own SUB sockets. PTY output,
resize, control-owner, and exit changes form one ordered state stream. PUB/SUB
may drop messages under pressure, so clients check sequence numbers instead of
treating it as a durable queue.

Attach avoids the usual subscribe/snapshot race:

1. The client subscribes to the session topic and a unique ready topic.
2. XPUB observes that ready subscription before ROUTER completes attach.
3. Under the runtime lock, the server freezes the current checkpoint and tail
   at sequence `S` and records the attached client.
4. The server encodes the archive outside the lock and replies with
   `attached(S, archive)`.
5. The client applies buffered events above `S`. Duplicates are ignored; a
   forward gap causes `resync`, which returns a new full archive.

The archive is gzip-compressed JSON format 2. It contains a canonical screen
checkpoint and a bounded ordered event tail. This is an in-memory reconnect
mechanism, not an on-disk session journal. See [PROTOCOL.md](PROTOCOL.md) for
wire limits.

## Runtime ordering

For each accepted command or PTY event, the runtime:

1. asks the reducer for the next state and effects while holding the runtime
   state lock;
2. executes required synchronous effects, including PTY/terminal resize and
   rollback handling;
3. appends the immutable event and commits the terminal model;
4. releases the lock and hands publication to the XPUB actor.

PTY input is handed to a single writer actor, so a blocked PTY write does not
hold the runtime state lock. Publication failure cannot undo an already
committed PTY or model change; sequence-gap resync is the recovery path.

ZeroMQ sockets are not thread-safe. Each ROUTER, XPUB, DEALER, and SUB socket
is confined to its owning goroutine.

## Terminal model

The default build uses the basic Go terminal implementation. It maintains the
canonical cells and modes consumed by the browser and supports common shell and
TUI behavior, including alternate screen, truecolor, bracketed paste, focus,
and DEC mouse modes. It is not claimed to be a complete terminal emulator.

An optional dynamically linked `libghostty-vt` adapter exists behind the
`libghostty` build tag. Compatible external headers and a library are required;
they are not vendored or installed by the normal build. There is no native
Ghostty snapshot import/export integration.

## Relay envelopes and trust

Remote traffic wraps the encoded local protocol frames in a bounded binary
envelope with routing fields and a monotonically checked outer counter. The
inner checkpoint and delta protocol remains authoritative.

In trusted mode the controller decodes browser-to-agent inner frames to apply
capability and lease policy. In blind mode endpoint helpers establish an
authenticated stream cipher and the controller forwards opaque data frames.
Every bundled client implements trusted mode only. See
[TRUST-MODEL.md](TRUST-MODEL.md).

The optional direct/Tailscale path changes which address the agent uses for
the same controller WebSocket handshake. It does not bypass the controller
protocol or connect a browser directly to an agent.

## Persistence boundaries

The following survive a normal process restart:

- user configuration and agent device credentials in the user state paths;
- controller enrollment/device records and policy records in the controller
  state directory.

The following are memory-only:

- the PTY and its checkpoint/tail;
- connected-agent inventory and live stream routing;
- the active browser connection.

`shenmux status` reads recorded metadata. It does not probe a PID, inspect IPC
health, or restore sessions.
