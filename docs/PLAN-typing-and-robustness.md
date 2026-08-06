# Plan — typing feel, and three robustness bugs

Four fixes, from four things observed by hand while getting the demo working.
The headline: **a user typing should see what they type as they go.** Today
every keystroke costs a full round trip before a single character appears.

## 1. Predictive echo (the headline)

### What is true today

`internal/webui/app.js` sends and renders nothing:

```js
canvas.addEventListener("keydown", (event) => {
  const data = keyData(event);
  if (!data) return;
  event.preventDefault(); send({ type: "input", data });
});
```

There is no prediction anywhere in the client — grep for
`predict|speculat|pending|echo|optimis|shadow` in `internal/webui/` returns
nothing. The only path that puts a glyph on screen is `applyDelta`, driven
exclusively by a server `delta`. So the latency to first paint of a character
is: browser → websocket → gateway → ZMQ IPC → daemon → PTY → shell echo →
daemon → delta → gateway → websocket → render.

Rendering itself is already fast (PixiJS on the GPU, batched through
`requestAnimationFrame`). The problem is not drawing; it is waiting.

Locally that round trip is imperceptible. Over the relay path to another
machine it is one full RTT per keystroke, which is the problem mosh exists to
solve.

### Why naive local echo is wrong

Echoing every keystroke is actively harmful. In vim's normal mode `j` does not
produce a `j`. At a password prompt nothing should appear at all. Predictions
must be *conditional, reconciled, and revocable*.

### The one thing we have that mosh does not

Mosh infers when prediction is safe, because it cannot see the remote terminal's
state. **We own the PTY.** `internal/ptyx/pty_unix.go` holds the master fd, so
the daemon can read termios directly and publish the authoritative `ECHO` flag.
That turns the password-prompt case from a heuristic into a fact.

`screen.InputModes` today carries `Mouse`, `MouseSGR`, `BracketedPaste`,
`FocusEvents` — no echo. Adding it is the prerequisite for everything else here.

### Design

**Server (prerequisite, small):**
- Add `Echo bool` to `screen.InputModes`, sourced from a new
  `(*PTY).EchoEnabled()` that does `unix.IoctlGetTermios` on the master and
  tests the `ECHO` bit.
- Republish when it changes; it already flows to clients because `InputModes`
  is part of `Frame` and participates in `Frame` equality (`types.go:348`), so
  a change produces a delta with no new plumbing.

**Client (the work):**
- A prediction *overlay*, never a mutation of the authoritative frame. The
  server's frame stays the single source of truth; predictions are a layer the
  renderer composites on top.
- **Predict only when all of these hold:** echo is on; `Frame.AltScreen` is
  false (no full-screen app); this client holds the control lease; measured RTT
  is above a floor (below ~5 ms, prediction is pointless and only adds risk —
  skip it entirely, which means the local demo path is unaffected).
- **Predict only** printable characters and Backspace. Never arrows, control
  chars, escape sequences, or pastes (`BracketedPaste` is in `InputModes` and a
  paste's effect is not knowable client-side). Any non-predictable key flushes
  the overlay.
- **Reconcile on `Checkpoint.Seq`.** Each prediction records the seq it was made
  against. When a delta arrives with a higher seq touching that cell, compare:
  match → confirm and drop from the overlay; mismatch → discard the whole
  epoch and repaint from server state.
- **Time out.** A prediction unconfirmed after ~2×RTT is discarded. This is the
  belt to the echo flag's braces: if the server never echoes, the glyph
  disappears on its own.
- **Adapt.** Track the confirmation rate. Below a threshold, disable prediction
  for a cooldown and try again later — a mispredicting terminal is worse than a
  slow one.
- Any resize invalidates the whole overlay.

**Rendering choice.** mosh underlines predictions to signal "not confirmed".
Recommendation: render predictions identically to real text, because with a
server-published echo flag plus seq reconciliation the mispredict rate should be
very low, and the whole point is that typing feels immediate. Keep the underline
behind a strict/debug flag. This is a deliberate trade: better feel, and when we
do mispredict the correction is visible as a flicker rather than as underlined
text resolving.

**Where.** `app.js` and `pixi-client.js` each have their own `applyDelta`. Put
the prediction state machine in one shared module both import, rather than
implementing it twice and having them drift.

**Testing.** The state machine must be unit-testable headlessly, separate from
the renderer: feed it keystrokes and synthetic deltas, assert the overlay
contents. Cases that must be covered: confirmation, mismatch discarding the
epoch, timeout with no echo, echo-off suppressing prediction entirely,
alt-screen suppressing it, and a resize clearing it.

## 2. A vanished client kills the whole session (highest severity)

Observed once, with evidence, and not reproduced in two directed attempts —
recorded here because the failure is severe and the cause is visible in the
code regardless.

The PTY died with:

```
shenmux: control server: zmq4: no route to peer: identity web-e91ea4799bb00e0ac4b18599
```

`internal/server/daemon.go:169`:

```go
case err, ok := <-control.Errors():
    if ok && err != nil {
        return fmt.Errorf("control server: %w", err)
    }
```

Any error on that channel returns from the daemon's run loop and takes the whole
session — the shell, the scrollback, everything — with it. A failure to route a
reply to one departed client is treated exactly like a failure to bind the
listener.

**Fix:** classify errors at the control-server boundary. A per-peer send or
route failure is a *client* error: drop that client, run the same teardown that
a clean detach runs (detach, release its lease), log it, and keep serving.
Reserve fatality for errors that genuinely invalidate the server (listener
death, socket close).

This composes with the lease fix already landed in `internal/agent/bridge.go` —
both are "the client is gone, clean up after it" and should converge on one
teardown path.

**Test:** a client that disappears mid-reply must leave the session running and
its lease free.

## 3. A stale browser tab silently hijacks a session

The local gateway has no auth and no instance identity. Two consequences, one
of which bit us live: after the demo recycled a port, a tab left open from a
*previous* gateway process reconnected to the new one, attached, took the
control lease, and resized the session.

**Fix (cheap, high value):** give each `shenmux web` process an instance id
generated at startup. The page embeds it; the websocket handshake carries it;
a mismatch is refused with a message that says the page belongs to a previous
session and to reload. That removes the silent-takeover mode entirely.

**Fix (defence in depth):** an auto-generated per-instance token in the URL, the
way the controller already uses a subject. The gateway is loopback-only by
default so the threat model is other local processes and stale tabs rather than
the internet, but it is cheap and it makes the demo's printed URL the only way
in.

## 4. `shenmux login` destroys an existing enrollment

`login` resolves state through `appstate.ResolvePaths()` and writes `device_id`,
`device_public_key`, `device_private_key` and `device_token` into the shared
`~/.local/state/shenmux/state.json`, replacing whatever was there. There is no
flag to prevent it; the only isolation is the `SHENMUX_STATE_DIR` and
`SHENMUX_CONFIG_FILE` environment variables. Anyone following the quickstart
loses an enrollment they cared about, silently.

**Fix:**
- Add `--state-dir` to `login` (the controller already has one, so this is
  consistency, not novelty).
- Refuse rather than destroy: if an enrollment already exists, print the device
  id that would be replaced and require `--force`. Silent destruction of a
  keypair is not an acceptable default.

## Sequencing

1. **Fix 2** first — it is the only one that loses a running session, and it is
   a contained change at one call site.
2. **Fixes 3 and 4** next — both small, both remove a silent-destruction or
   silent-takeover mode, neither depends on anything else.
3. **Fix 1** last and largest, in two steps: publish the echo flag (server,
   small, independently useful), then the client prediction layer behind a flag
   so it can be turned off if it misbehaves in the wild.

Fix 1 is the one a user feels. Fixes 2–4 are the ones that stop a demo from
embarrassing us.
