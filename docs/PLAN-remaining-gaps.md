# Plan — the gaps left after the robustness work

Follow-on to `docs/PLAN-typing-and-robustness.md`, whose four items are done
(predictive echo was reverted; see that document and the revert commit). These
five came out of adversarial review and out of finishing the work. Every claim
below was checked against the source rather than carried over from the review.

**Status: all closed.** Items 1-5 are fixed, 6 is fixed at the root, and 7 was
decided against with the evidence recorded. Each section keeps its original
analysis, because the reasoning is the part worth re-reading, and states its
resolution at the top. Nothing here is outstanding.

---

## 1. A slow client can hang the control loop indefinitely

**CLOSED.** `server.boundedSender` bounds each reply and folds expiry into
`peerGone`, so the existing `dropClient` path frees the lease. The upstream fix
is prepared in `patches/` and is not consumed; see item 7 for why we did not
solve this by changing drivers instead.

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

### What the ZeroMQ guidance says, and why our driver is off-pattern

Checked against the canonical docs, because this is a well-known problem with a
well-known answer and it is worth knowing how far we have drifted from it.

Canonical libzmq ROUTER behaviour: on reaching a peer's high-water mark, a
ROUTER **silently drops** messages for that peer. Setting `ZMQ_ROUTER_MANDATORY`
converts dropping into "block or return `EAGAIN`" — `EAGAIN` when the send is
non-blocking. The guide is explicit about our exact failure: *"Using a single
ROUTER socket for output would be problematic, since any one blocked peer would
block outgoing traffic to all peers."* The recommended handling is a
non-blocking send plus `EAGAIN`, i.e. treat a peer that is not draining as a
peer to drop.

Three ways our stack diverges:

1. **The pure-Go driver's default is the dangerous mode.** `tomi77/zmq4`
   defaults to the `Block` overflow policy, so we get libzmq's
   `ROUTER_MANDATORY`-with-blocking semantics *by default*, where libzmq would
   have dropped. That is why this hangs rather than degrading.
2. **The safe mode is unusable as shipped.** `WithSndHWMPolicy(Drop)` exists,
   but a dropped message makes `p.send` return false, which `ROUTER.Send`
   reports as `ErrClosed` (`router.go:53`) — indistinguishable from a genuinely
   closed socket, and classified as fatal on our side. A full queue is not a
   closed socket; reporting it as one turns a slow client into a dead session.
3. **The canonical fix is not expressible.** A non-blocking send would be the
   textbook answer, but `zmqx.SendMultipart` rejects the `DontWait` flag
   (`flags&^SndMore`), and the driver's `ROUTER.Send` discards its context, so
   the trick `zmqx` already uses for non-blocking *receives* — a zero-deadline
   context — cannot work for sends.

**Live trap worth fixing regardless:** `zmqx.SetInt(SndTimeout, …)` is accepted
and silently does nothing. The `case Linger, RcvTimeout, SndTimeout, …` arm
comments that "context cancellation and explicit close provide these
semantics", which is true for receive (the driver's `Recv` honours its context)
and **false for send**. Anyone attacking this hang the obvious way — set a send
timeout — will get a clean `nil` and no behaviour change. Either implement it
or reject the option; do not keep accepting it.

### Preferred fix, revised

**Upstream the context.** The driver already threads a `context.Context` into
every `Send` and then ignores it. `p.send(msg, closeCh)` is called from six
places (`router.go`, `dealer.go`, `pair.go`, `push.go`, `rep.go`, `req.go`),
each passing `s.base.closeCh`; adding the context to that select is roughly ten
lines. That makes `zmqx.SendMultipartContext` behave as its own doc comment
already promises, and makes `DontWait`-on-send implementable symmetrically with
receive. We have precedent for upstreaming a small fix to a dependency.

Until that lands, bound it at our layer as described above. Prefer the bound
even after an upstream fix if the upstream release is slow — the two are
compatible, and the layer-level bound is what makes the behaviour ours to test.

**Not doing:** the guide's structural suggestion of avoiding a single ROUTER for
output (a socket per peer, or a proxy pair). It is the "correct" ZeroMQ answer
and it is a large change to a control plane that otherwise works; recording it
here as the road not taken, and as the reason per-client bounding is the
pragmatic substitute.

**Worth investigating separately:** ZMTP-level heartbeats
(`ZMQ_HEARTBEAT_IVL`/`TTL`/`TIMEOUT`) would let the transport notice a dead peer
rather than waiting for a send to wedge. I have not verified whether this driver
implements them — check before planning around it.

---

## 2. Three outcomes, one of them silent

**CLOSED.** A malformed frame count is answered: the check moved below identity
parsing and the receive cap was raised above the real limit so the sender can be
named, which costs no buffering because the driver has already read the whole
message. The one genuinely unanswerable case is documented in `PROTOCOL.md`.

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

**CLOSED.** `app.js` deleted. `web/bundle-audit.sh` runs in `make check` and
fails on drift; `check` runs it *instead of* `web-build`, since a rebuild there
would repair the drift and pass.

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

**CLOSED.** The pure handshake logic moved to `internal/webui/gateway.mjs` with
18 `node --test` cases wired in as `make web-test`.

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

**CLOSED.** The subtest now publishes a real frame first, so the assertion has
something to be wrong about instead of reading the helper's zero value.

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

---

## 6. The controller never tells the agent a browser is gone

**CLOSED.** `handleBrowser` now notifies the agent from its teardown defer,
registered before the OPEN is forwarded and after the stream is recorded -- so
every exit inherits it and a future `return` cannot forget. Suppressed by a
`sync.Once` shared with the clean-close path, and bounded by a write deadline so
a stalled agent delays only the frame it is owed, never the stream slot or the
lease. Closing a browser transport with no close frame now delivers a close to
the agent; the test fails against the previous implementation.

This also fixed a case not named below: **capability revocation**. Revoking a
capability closed the browser socket, and the agent was never told the stream
had been cut off.

**Severity: high — this is the root cause of item 1 in the previous plan**, which
was fixed at the symptom.

`Controller.handleBrowser` (`internal/relay/tunnel.go:428`) forwards a
`FrameClose` to the agent only when the browser *sends* one. Its read loop
returns on any read error with nothing but `defer conn.Close()`, and there is no
other notification path. A browser tab that closes abruptly — which is what
closing a tab normally does — leaves the agent holding a stream for a peer that
no longer exists.

That is why the lease leak happened at all. `internal/agent/bridge.go` now
releases the lease when a stream is torn down (`fea6cc0`), and the reducer now
ages ownership so a stale lease expires rather than wedging the session — but
both are downstream of a teardown that frequently never starts. The stream, its
muxd client and its attachment survive until the tunnel itself dies.

**Fix:** the controller must synthesise a close toward the agent when a browser
stream ends for any reason, not only on a clean `FrameClose`. The agent side
already does the right thing once it hears.

**Test:** kill a browser websocket without a close frame; the agent's stream must
be torn down and the client must leave the session's client list.

Found while extending the spec: no reducer rule can reach this, because the
session model never learns the stream existed. See `docs/WHAT-THE-SPEC-OWNS.md`.

---

## 7. Should we move to go-zeromq/zmq4? DECIDED: not now, on memory -- not on routing.

**Corrected.** An earlier version of this section called the silent drop
"disqualifying". That was wrong, and the correction matters because it changes
what the blocker actually is.

**The silent drop is ZeroMQ's documented default, not a defect.** `zmq_socket(3)`:
if a ROUTER's peer "does not exist anymore, or has never existed, the message
shall be silently discarded", unless `ZMQ_ROUTER_MANDATORY` is set, in which
case it fails with `EHOSTUNREACH`. Same at the high-water mark: drop by default,
block or error under mandatory. The rationale is sound -- ROUTER serves many
transient peers, and erroring by default would let one departed peer
head-of-line-block everyone else. libzmq behaves identically unless you opt in.
`go-zeromq/zmq4` implements the default faithfully and omits the opt-out: an
incompleteness, not a spec violation.

**And `EHOSTUNREACH` was never the guarantee I treated it as.** It reflects the
ROUTER's *local view* only: an identity is routable from the moment a peer's
connection registers until its disconnect is noticed, so a peer that is wedged
but still connected queues fine and reports success. The ZeroMQ architecture is
explicit that the transport guarantees atomicity and per-connection ordering,
never delivery; reliability belongs above the socket, in acks and heartbeats.

**We already built that.** The reducer now carries ownership-with-an-age: a
lease held by a client that has stopped proving liveness expires on its own.
That is the application-layer mechanism the guide prescribes, and it -- not
`ErrNoRoute` -- is what makes lease release correct. `ErrNoRoute` is a fast
local hint that lets us release early.

**So the hint is replaceable at our layer.** The session model already knows who
is attached (`mux.member?`), so a membership check before sending gives exactly
the same local-view signal `EHOSTUNREACH` would, without needing the driver to
provide it. If we ever switch, that is the substitute.

**The real blocker is memory.** `socket.SetOption` is `props[name] = value;
return nil` under a `FIXME(sbinet) different socket types support different
options`, and `OptionHWM` is read only by `pub.go`. ROUTER has no high-water
mark at all, so the 10_000 HWMs this control plane sets would silently do
nothing and per-peer queues would grow unbounded. That is a genuine regression
with no workaround at our layer, and it is the reason to stay put.

**And the prize has shrunk.** The attraction was the send deadline, which
`server.boundedSender` now provides. Switching would trade a bounded send we
control for an unbounded queue we cannot.

**Decision: stay on `tomi77/zmq4`.** Revisit if `go-zeromq/zmq4` gains ROUTER
HWM; mandatory routing would be welcome but is no longer the deciding factor.

### Could we adopt `shen.x.zmq` itself? Not today, and the blocker is structural

Checked properly rather than assumed, because "move the transport into Shen" is
the more interesting version of this question.

The waist cannot give a socket an identity. `shen.x.zmq.socket-host` has
**arity 1** -- a type symbol, nothing else -- and `go-zeromq/zmq4` accepts an
identity only at construction, through `WithID`. `setopt-host` passes unknown
options through, but there is nothing underneath to receive one, and identity
has to be set before the socket connects in any case.

That is fatal here rather than inconvenient. The control plane addresses every
reply by ROUTER identity: `client.go` sets a DEALER identity to the client id,
and `handleControl` reads `frames[0]` to know who asked and who to answer. With
no settable identity there is no addressing, so this is not a degradation to
weigh -- the protocol does not function.

Two smaller gaps point the same way: the waist's option set is
`rcvtimeo, sndtimeo, subscribe, unsubscribe, linger`, so `MaxMsgSize` and both
HWMs have nowhere to go either.

**What would unblock it,** and it is a small, honest contribution rather than a
rewrite: widen the waist to carry `identity` at socket construction and `hwm` as
an option, then implement both in the shen-go host. `identity` is a `WithID`
away; `hwm` needs ROUTER support that `go-zeromq/zmq4` does not have yet, which
puts it behind the same upstream work as the rest of item 7.

**Worth separating from the goal it serves.** The reason to want this is to put
distributed-systems logic under Shen where its invariants apply. That goal is
right, and it has already been served where it counts: ownership-with-an-age,
delivery classification and idempotent peer loss are in `specs/mux.shen` and run
at runtime. Socket primitives are not where the invariants live -- send and recv
have no interesting semantics to constrain -- and routing PTY deltas through a
Shen call per message puts the cost on the hottest path in the system. The
semantics moved; the syscalls need not follow.

**A constructive note for `shen-extensions`:** its waist could carry
`router-mandatory` as an int socket option, with libzmq-backed ports
implementing it natively and the pure-Go shen-go backend taking the existing
"unsupported on this backend" escape hatch until upstream grows it. That keeps
the contract honest per port, and tells ROUTER users on shen-go that they are
in fire-and-forget mode -- which is the same honesty this repo just applied to
`SndTimeout`.

**Separately, and still open as a question rather than a gap:** whether socket
I/O should route through the Shen waist at all. That is an architecture
decision whose cost lands on the PTY delta hot path — one Shen call per message
— and it is independent of which driver sits underneath. It should be measured,
not assumed, and it is not blocking anything today.
