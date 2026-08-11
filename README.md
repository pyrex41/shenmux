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
local browser gateway, enrolls itself, and prints the URL to open. Open the URL
it prints, not a bare `127.0.0.1:8787` — it carries an access token, and the
port moves when something else already has 8787. That is a real shell in a real
terminal renderer. Type in it.

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

It prints a URL containing an access token, e.g. `shenmux web
url=http://127.0.0.1:8787/?token=...`. Open that, not the bare address: a
loopback port is not an authorization boundary, so without a token any local
process can attach and take the input lease. `--no-token` opts out if you want
that.

`shenmux run` starts a login shell and restarts it when it exits. Pass a command
after `--` for one shot, or `--keepalive=false`. `shenmux status` prints the
config, the state directory, and the sessions this binary has started.

### Control is exclusive and cannot be stolen

One client holds the input lease at a time. If another client has it, your
"take control" request fails and you get a session you can watch but not type
into. That is deliberate, not a bug. Detach the other client, or release control
from it, and try again.

A browser tab left open from an earlier `shenmux web` process cannot reattach
and grab the lease out from under you: every gateway process stamps an
instance id into the page it serves and refuses a handshake carrying a
different one. The stale tab gets a plain "reload the page" error instead of
silently attaching.

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

The controller serves no browser UI. `GET /sessions` lists what the connected
agents advertise — session names and whatever opaque `--label key=value` pairs
the agent was started with — and `/capabilities` and `/browser` are there for a
client to use. Teaching the real client (`shenmux web`) to attach through a
controller is not done yet.

`shenmux login` refuses to enroll over an existing device identity: it names
the device id at stake and stops rather than overwriting
`~/.local/state/shenmux/state.json`. Point it at a separate state directory to
enroll alongside what you already have:

```sh
./bin/shenmux login --controller http://127.0.0.1:8788 --code CODE \
  --state-dir /tmp/other-enrollment
```

`--force` replaces the existing enrollment instead — this destroys its
keypair, and the old device stays registered on the controller with nothing
left here that can act as it. `SHENMUX_STATE_DIR` and `SHENMUX_CONFIG_FILE`
do the same job as `--state-dir`/`--config`, but for every client command in a
session at once, which is why `make demo` sets them rather than passing flags
to each one.

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
  makes the agent reject every trusted stream, which is the point, but it
  leaves you with no browser that can reach it through a controller.
- **No production hosting and no multi-user story.** The browser identity
  fallback trusts an `X-Shenmux-Subject` header. There is no TLS, no origin
  policy, no rate limiting, no admin workflow for grants or revocation. The
  checked-in Dockerfile builds `shenmux`, but its entrypoint still starts the
  legacy `muxd` shape and no deployment of it is tested.
  See [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md).
- **The controller's session list is memory-only and believes the agent.** It
  lists the names passed to `shenmux agent --session/--sessions`. Nothing
  reconciles that against processes that are actually running, and it is gone
  when the controller restarts.
- **No Windows PTY.** Linux and macOS, Go 1.26+.
- **Nothing survives the session.** Kill `shenmux run` or reboot its host and
  the PTY is gone. Checkpoints and deltas are in memory and are never written
  to disk. Earlier builds did write them: every accepted event was archived
  under `<state-dir>/history`, which defaulted to
  `~/.local/state/shenmux/history`. That is gone. If a previous build left
  archives there, `shenmux run` names the directory once on startup and
  otherwise ignores it; nothing can restore one into a live session, so delete
  it when you see it.

The default build is pure Go: no CGO, no libzmq, no C toolchain. `-tags
libghostty` swaps in the libghostty-vt terminal adapter and needs both.

## Tests

```sh
nix develop --command make check   # web bundle drift guard, JS tests, Shen guards + gate, go test, vet, both builds
nix develop --command make race
```

`internal/webui/app.bundle.js` is committed and embedded in the binary, so an
edit to the client source that skips `npm run build` ships nothing. `make
check` catches that drift instead of quietly fixing it for you.

`make test-deploy` is a compile check, not a deployment test.

## More

The Shen spec isn't a build-time check that disappears once code compiles:
`internal/shenguard/guards_gen.go` calls the generated model at runtime
(`semanticModel.Call`) for every session transition — attach, control, input,
delivery — so it decides these outcomes as the session runs, not just checks
them ahead of time.

- [docs/WEB.md](docs/WEB.md) — both browser clients in detail, and the latency benchmark
- [docs/WHAT-THE-SPEC-OWNS.md](docs/WHAT-THE-SPEC-OWNS.md) — what the spec owns, what was deliberately refused, and what remains Go's job
- [docs/TRUST-MODEL.md](docs/TRUST-MODEL.md) — what is guaranteed, what is not
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — how run/web/agent/controller fit together
- [docs/PROTOCOL.md](docs/PROTOCOL.md) — the wire format
