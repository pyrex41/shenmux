# Plan — the gaps left after the robustness work

Follow-on to `docs/PLAN-typing-and-robustness.md`, whose four items are done
(predictive echo was reverted; see that document and the revert commit). These
five came out of adversarial review and out of finishing the work. Every claim
below was checked against the source rather than carried over from the review.

Ordered by severity. Items 1 and 2 are correctness; 3 is a footgun that has
already cost real time; 4 and 5 are hygiene.

---

## 1. A slow client can hang the control loop indefinitely

**Severity: high.** This is the departed-client failure again, upgraded from a
crash to a hang, and it survives the fix that landed for the crash.

`ROUTER.Send` takes a `context.Context` **and never reads it**
(`tomi77/zmq4@v1.0.0/router.go:45-57`). It calls `p.send(msg, closeCh)`, which
under the default `Block` overflow policy waits on exactly two things
(`pipe.go:110-127`):

```go
default: // Block
	select {
	case p.outCh <- msg:
		return true
	case <-closeCh:          // socket-level, not per-call
		return false
	}
```

`closeCh` is the socket's, so nothing short of closing the whole socket
unblocks it. With `SndHWM: 10_000` (`control.go:122`), a peer that stops
draining fills the queue and then wedges the control loop: no receives, no
lease reaping, no other client served, and `ControlServer.Close()` blocks
forever on `<-s.done`.

**Two obvious fixes are both traps.**

`zmqx.SendMultipartContext` exists and is documented as being for exactly this
("waits for a route to become available when the peer has disconnected"), but
it passes the context to a driver call that discards it. It cannot help here.

Switching the socket to the `Drop` overflow policy makes a full queue return
`ErrClosed` (`router.go:53`) — which our classification treats as **fatal**. A
slow client would then kill the session outright. Strictly worse than the hang.

**Do this instead:** bound the send at our layer, and treat exceeding the bound
as what it actually is — a client that is no longer taking delivery, i.e. the
same condition `peerGone` already describes. Run the send with a deadline; on
expiry, return a `clientFault` so the existing `dropClient` path detaches the
client and frees its lease. That keeps one concept ("the client is gone, clean
up after it") rather than adding a second.

The send goroutine may outlive the deadline — it unblocks when the socket
closes. That is acceptable and must be commented, not hidden: it is bounded by
socket lifetime and cannot accumulate faster than clients disconnect.

**Test:** a `controlSocket` stub whose `SendMultipart` blocks forever must not
stall the loop; the next client must still be served and the blocked client's
lease must come free.

---

## 2. Three outcomes, one of them silent

**Severity: medium.** `handleControl` can now reply, be fatal, or **silently
drop a message**. The third is neither documented on `Errors()` nor visible to
the client, which hangs until its own timeout with no idea why.

There are two distinct silent paths and they need different fixes, because they
differ in whether we still know who sent the message.

**(a) Malformed frame count — identity known, so reply.** The
`len(frames) < 3 || len(frames) > 4` path returns `nil` with no answer, but
`frames[0]` is the ROUTER identity and is right there. Reply with a
`KindError`. This is a small, clearly correct change.

**(b) Message refused by the receive limits — identity lost.**
`RecvMultipartLimit` consumes the message and *then* refuses it, so the caller
gets an error and no frames at all: there is no identity to answer. Replying is
impossible without a design change, namely receiving with a higher frame cap
and enforcing the real limit after parsing the identity frame.

Decide deliberately between:
- raise the receive cap, validate after reading identity, and reply; or
- keep dropping, and **document it** on `ControlServer.Errors()` and in the
  protocol docs as a known case where a client gets no answer.

Either is defensible. Silence that is neither implemented deliberately nor
written down is not. Note also that the receive-side reject budget was scope
creep in the crash-fix commit — this is the commit where it earns its own
reasoning.

---

## 3. `internal/webui/app.js` is dead code, and the bundle can silently go stale

**Severity: medium — it has already cost real time twice today.**

`app.js` is referenced by nothing: not `index.html`, not `files.go`, not
`web/build.mjs` (verified: zero matches in all three). The live client is
`pixi-client.js`, bundled into `app.bundle.js`, which is what `files.go`
embeds. Yet `app.js` keeps being maintained in parallel — the predictive-echo
work spent 83 lines "keeping it in sync" with a file no user loads, and left it
holding a top-level `import` that would be a syntax error under the plain
`<script defer>` tag `index.html` actually uses.

**Delete it.**

The related hazard is worse and is the one to actually fix: `app.bundle.js` is a
committed build artifact, and nothing checks that it matches its source. Editing
`pixi-client.js` without running `npm run build --prefix web` ships a page with
none of the change, silently. That happened during the gateway-token work: the
client half was dead until the bundle was regenerated, and the only reason it
was caught was grepping the shipped bundle for a string literal.

**Add a drift guard to `make check`:** rebuild the bundle into a temp file and
diff it against the committed one; fail on difference with "run
`npm run build --prefix web`". This is the same discipline `shenguard-audit.sh`
already applies to generated Go, so it fits the house style.

---

## 4. The browser client has no test coverage at all

**Severity: medium.** The JS test suite went away with the predictive-echo
revert, and there is now no `*.test.mjs` anywhere.

That matters more than it did before, because `pixi-client.js` has since gained
real logic: instance-id matching, token handling, and the `blockPage` path that
must stop reconnecting when the gateway will never accept this page. All of it
is currently verified only by me having clicked it once.

**Do:** reintroduce a small headless suite (`node --test`) covering the client
behaviours that are pure logic — the refusal handling, the meta-tag reading,
the decision to stop reconnecting — and wire it into `make check`, which today
is `web-build guard-check audit shen test vet` with no JS step. Keep the
modules importable without browser globals at import time, which is what made
the previous suite possible.

Prerequisite worth noting: this needs the module to be importable, so it may
mean extracting the pure logic out of `pixi-client.js` rather than testing the
renderer.

---

## 5. One echo test asserts nothing

**Severity: low.** In `internal/server/echo_test.go`, the
`TestEchoFailsClosed/"pty cannot report echo"` subtest ends with
`echoOfPublishedFrame(t, pub)`, which returns `false` when nothing was ever
published. Since the same subtest has just asserted that nothing *was*
published, that final check is reading the helper's default rather than the
code's behaviour, and would pass against an implementation that got this wrong.

The first assertion in that subtest (publication count unchanged) is the real
one. Either delete the second, or strengthen it by publishing a frame through
another path first so there is something for it to be wrong about.

---

## Sequencing

1. **Item 1** — the only one that loses a working session, and it reuses the
   teardown path that already exists.
2. **Item 2(a)** — small and clearly correct; take the decision on 2(b) at the
   same time and write it down either way.
3. **Item 3** — delete `app.js`, add the bundle drift guard. Cheap, and it
   closes a failure mode that has already bitten twice.
4. **Items 4 and 5** — test hygiene, in that order.

Items 3 and 5 are each under an hour. Item 1 is the one worth care: it needs a
test that genuinely hangs the stub, or it proves nothing.
