# Browser client prototype

The prototype keeps `muxd` unchanged and puts a small WebSocket gateway in
front of the existing Go client. The browser receives the same authoritative
checkpoint and typed screen deltas as a native client; it never parses a PTY
escape stream.

Start the daemon in one shell, then the gateway in another:

```sh
nix develop
./bin/muxd -session default -keepalive
go run ./cmd/shenmux-web -session default
```

Open <http://localhost:8787>. The client is shipped as one embedded browser
bundle: PixiJS owns the GPU-backed terminal scene, while ShenScript handles the
small command-policy layer without entering the per-cell render loop. The
gateway is the right place to add authentication, session discovery, and a
multi-pane layout before exposing it publicly.

The bundle is reproducible from the checked-in `web/package-lock.json`:

```sh
npm install --prefix web
npm run build --prefix web
```

When `muxd` starts without an explicit command it launches a login shell,
preferring zsh and then fish, with a private session-local prompt config. If
Starship is installed it initializes Starship; otherwise a compact colored
prompt is used. Select one explicitly with `muxd -shell zsh` or
`muxd -shell fish`.

ShenScript is intentionally limited to command policy for now; the hot path
stays in JavaScript and PixiJS. The same environment can grow into the
session/pane state layer without changing the wire protocol.

The browser also honors the terminal's typed interaction modes: alternate
screen, truecolor, bracketed paste, focus reporting, and DEC mouse protocols
(click, button tracking, any-event tracking, and SGR coordinates). Ordinary
shell sessions do not receive mouse bytes; they are emitted only after the
remote TUI enables the corresponding mode. PTY output is batched in short
windows before VT interpretation so redraw-heavy TUIs produce fewer redundant
screen publications.

The demo shell also removes an inherited `NO_COLOR=1` setting and advertises
`COLORTERM=truecolor`; otherwise applications such as Claude Code may disable
color before the browser ever receives a styled cell.

Use `-keepalive` for browser sessions: if the login shell exits (including
Ctrl-D), the PTY supervisor starts a fresh login shell without dropping the
session or browser connection. Explicit commands supplied after `--` are
still one-shot commands.

## Input latency benchmark

For a repeatable transport baseline, run an echo-only PTY and gateway on a
separate port, then use the checked-in Node harness:

```sh
./bin/muxd -session bench -- sh -c 'stty -icanon -echo; exec cat'
./bin/shenmux-web -session bench -listen 127.0.0.1:8789
nix develop --command node scripts/bench-web-latency.mjs \
  --url ws://127.0.0.1:8789/ws --count 200 --warmup 20
```

The result measures WebSocket input send through the gateway and muxd until
the first echoed screen delta arrives. It does not include browser input
processing or paint; use Chrome's Performance panel for those segments. Run
the same harness with a 50--100 ms network profile when comparing remote
deployments. When host-level network emulation is unavailable, the harness
also supports a reproducible application-level profile:

```sh
nix develop --command node scripts/bench-web-latency.mjs \
  --url ws://127.0.0.1:8789/ws --count 40 --warmup 5 --simulated-rtt-ms 100
```

The output labels this as `simulated_rtt_ms`; use `dnctl`, `tc`, or an
equivalent network emulator for wire-level RTT measurements.

For the egress-only hosted/self-hosted deployment contract (including Fly,
AWS, Hetzner, and home servers), see [DEPLOYMENT.md](DEPLOYMENT.md).
