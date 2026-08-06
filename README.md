# shenmux

A terminal multiplexer. `shenmux run` owns a PTY on some machine; clients attach
to it by name — a native TTY client, a local browser terminal, or a browser on
another machine relayed through a controller. It is a development tool. Nothing
here is deployed to the public Internet.

## See it work

```sh
nix develop
make demo
```

It builds what it needs, starts a session, a controller, an agent and the
local browser gateway, enrolls itself, and prints the URL to open — usually
<http://127.0.0.1:8787>, or the next free port if something else already has
it. That is a real shell in a real terminal renderer. Type in it.

`make demo DEMO_ARGS=--detach` leaves it running; `scripts/demo.sh --stop`
stops it. The demo keeps its state in its own directory, so it will not
disturb an enrollment you already have.

## The pieces, by hand

`shenmux run` is the only thing that creates a PTY. Nothing else in this
repository does — not the gateway, not the controller, not the agent. Start it
first, on the machine that should own the shell:

```sh
./bin/shenmux run --session work
```

Attach from a terminal:

```sh
./bin/muxctl -session work            # -observe for read-only
```

Or attach from a browser on the same machine:

```sh
./bin/shenmux web --session work      # --listen 127.0.0.1:8787 by default
```

`shenmux run` starts a login shell and restarts it when it exits. Pass a command
after `--` for one shot, or `--keepalive=false`. `shenmux status` prints the
config, the state directory, and the sessions this binary has started.

### There are two browser UIs. Use the right one.

`shenmux web` serves the real one, from `internal/webui/`: PixiJS renderer,
full keyboard, resize, colour, paste, mouse modes, blinking cursor.

The controller's `/workspace` (`internal/relay/workspace.js`) is a different,
much smaller client that exists to exercise enrollment, capabilities and relay
framing. It handles printable keys, Enter and Backspace, sends no resize, and
paints plain DOM at whatever size the session already has (80x24 by default). If
you land there first you will conclude the terminal is broken. It isn't; you are
in the test harness.

### Control is exclusive and cannot be stolen

One client holds the input lease at a time. If another client has it, your
"take control" request fails and you get a session you can watch but not type
into. That is deliberate, not a bug. Detach the other client, or release control
from it, and try again.

## Remote through a controller

Private network only. This uses plain HTTP and turns on a deliberately insecure
browser-identity shortcut.

```sh
# controller host
./bin/shenmux controller --listen 127.0.0.1:8788 \
  --origin http://127.0.0.1:8788 --dev-browser-subject local-test
```

It prints one single-use enrollment code to stderr, good for ten minutes. On the
host that owns the PTY:

```sh
./bin/shenmux run --session work
./bin/shenmux login --controller http://127.0.0.1:8788 --code CODE
./bin/shenmux agent --controller http://127.0.0.1:8788 \
  --transport relay --session work
```

Open <http://127.0.0.1:8788/workspace?subject=local-test> — and re-read the
two-UIs warning above before you judge it.

`shenmux login` overwrites your device identity. It writes `device_id` and the
device keypair into `~/.local/state/shenmux/state.json`, replacing whatever was
there, and there is no flag to stop it. If you already have an enrollment you
care about, point the command somewhere else first:

```sh
SHENMUX_STATE_DIR=/tmp/other-enrollment \
SHENMUX_CONFIG_FILE=/tmp/other-enrollment/config.json \
  ./bin/shenmux login --controller http://127.0.0.1:8788 --code CODE
```

Those two environment variables are the only isolation available. `make demo`
sets them for you.

Across hosts, replace loopback with a reachable private address and keep
`--origin` equal to the URL the agent uses. `shenmux login` allows plain HTTP
only for `localhost` or a bare IP; a named host must be HTTPS. `--transport
tailscale` or `--transport auto --direct tailscale://HOST:8788/ws` sends the
agent over a tailnet instead of the relay; `auto` falls back to the relay when
the direct endpoint fails to connect, which is a reconnect, not stream
migration.

## What is actually true

Works: local sessions, the native client, the local browser terminal,
enrollment, the relay path, tailnet transport, reconnect with backoff.

Does not:

- **The controller can read your terminal.** Trusted relay mode is the default
  and the only mode any bundled client speaks. TLS would protect the wire, not
  the controller.
- **Blind mode has no client.** The handshake, encryption and tests exist as
  library code. Nothing shipped here initiates it. `login --trust-mode blind`
  makes the agent reject the bundled workspace, which is the point, but it
  leaves you with no working browser.
- **No production hosting and no multi-user story.** The browser identity
  fallback trusts an `X-Shenmux-Subject` header. There is no TLS, no origin
  policy, no rate limiting, no admin workflow for grants or revocation. The
  checked-in Dockerfile and `fly.toml` still package the legacy `muxd` shape.
  See [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md).
- **The controller's session list is memory-only and believes the agent.** It
  lists the names passed to `shenmux agent --session/--sessions`. Nothing
  reconciles that against processes that are actually running, and it is gone
  when the controller restarts.
- **No Windows PTY.** Linux and macOS, Go 1.26+.
- **Nothing survives the session.** Kill `shenmux run` or reboot its host and
  the PTY is gone. Checkpoints and deltas are in memory.

The default build is pure Go: no CGO, no libzmq, no C toolchain. `-tags
libghostty` swaps in the libghostty-vt terminal adapter and needs both.

## Tests

```sh
nix develop --command make check   # web bundle, Shen guards + gate, go test, vet, both builds
nix develop --command make race
```

`make test-deploy` is a compile check, not a deployment test.

## More

- [docs/WEB.md](docs/WEB.md) — both browser clients in detail, and the latency benchmark
- [docs/TRUST-MODEL.md](docs/TRUST-MODEL.md) — what is guaranteed, what is not
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — how run/web/agent/controller fit together
- [docs/PROTOCOL.md](docs/PROTOCOL.md) — the wire format
