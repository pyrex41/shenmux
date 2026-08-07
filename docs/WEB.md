# Browser clients

shenmux contains two different browser paths. Both are development interfaces,
and neither should be exposed directly to untrusted networks.

## Local browser gateway

`shenmux web` connects to one already-running local session over ZeroMQ and
serves an HTTP/WebSocket UI. It defaults to `127.0.0.1:8787`.

```sh
./bin/shenmux run --session work
./bin/shenmux web --session work
```

Open <http://127.0.0.1:8787>. Use matching `--control` and `--data` values on
both commands only when overriding the default IPC endpoints.

The gateway has no login, authorization middleware, TLS, or cross-user
boundary. Binding `--listen` to a non-loopback address makes control of the PTY
available to anyone who can reach it.

The embedded UI receives a canonical checkpoint and typed screen deltas; it
does not parse raw PTY escape bytes. PixiJS renders the screen. The JavaScript
client handles ordinary keyboard input, resize, paste, focus reporting, and
DEC mouse modes when the terminal application enables them. It can acquire or
release the local runtime's control lease and request a resync after a gap.

The checked-in browser bundle is rebuilt with:

```sh
npm install --prefix web
npm run build --prefix web
```

`make build` and `make check` also run the web build. Use the locked dependency
versions in `web/package-lock.json` for reproducible output.

## Controller workspace

`shenmux controller` serves a separate, smaller workspace at `/workspace`.
It lists session metadata advertised by connected agents, requests a control
capability, and relays the local protocol through `/browser`.

For local development:

The demo shell also removes an inherited `NO_COLOR=1` setting and advertises
`COLORTERM=truecolor`; otherwise applications such as Claude Code may disable
color before the browser ever receives a styled cell.

```sh
./bin/shenmux controller \
  --listen 127.0.0.1:8788 \
  --origin http://127.0.0.1:8788 \
  --dev-browser-subject local-test
```

After enrolling an agent and advertising a live session, open:

```text
http://127.0.0.1:8788/workspace?subject=local-test
```

The `subject` query parameter works only when it exactly matches
`--dev-browser-subject`. Enabling it creates a wildcard observe/control grant
and is intentionally unsafe. Without an application-provided authentication
hook, the controller also accepts `X-Shenmux-Subject`; a production reverse
proxy would have to remove all client-supplied copies and inject a verified
identity. This integration is not provided in the command.

The controller workspace currently:

- uses trusted relay mode only;
- requests control permission rather than offering an observe-only choice;
- supports basic printable keys, Enter, and Backspace;
- does not send browser resize, mouse, paste, focus, or complete special-key
  sequences;
- does not automatically reconnect and reattach after a controller, agent, or
  network interruption;
- renders a simple DOM representation rather than the local PixiJS client.

It is useful for exercising enrollment, discovery, capabilities, relay
framing, and checkpoint/delta flow. It is not the production version of the
local browser UI.

## Browser state and reconnects

An attach receives a compressed full archive containing a checkpoint and a
bounded delta tail. Live deltas then carry consecutive sequence numbers. A
client that sees a gap can ask the session for another full archive.

The local browser client has resync behavior while its WebSocket remains
connected. The bundled controller workspace does not implement automatic
socket reconnect. Refreshing it obtains current inventory and a new,
single-use capability, then attaches from a fresh archive.

Neither browser stores a durable terminal log. If `shenmux run` exits, there
is no PTY to reconnect to.

## Input latency benchmark

Start an echo-only local PTY and gateway on a separate port:

```sh
./bin/shenmux run --session bench --keepalive=false -- \
  sh -c 'stty -icanon -echo; exec cat'
./bin/shenmux web --session bench --listen 127.0.0.1:8789
nix develop --command node scripts/bench-web-latency.mjs \
  --url ws://127.0.0.1:8789/ws --count 200 --warmup 20
```

The result measures WebSocket input through the gateway and local daemon until
the first echoed screen delta. It excludes browser input processing and paint.
The harness can add an application-level delay for repeatable comparisons:

```sh
nix develop --command node scripts/bench-web-latency.mjs \
  --url ws://127.0.0.1:8789/ws \
  --count 40 --warmup 5 --simulated-rtt-ms 100
```

This is not wire-level network emulation; use `tc`, `dnctl`, or an equivalent
tool for end-to-end RTT measurements.

For controller exposure requirements and current blockers, see
[DEPLOYMENT.md](DEPLOYMENT.md).
