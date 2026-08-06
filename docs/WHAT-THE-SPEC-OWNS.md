# What the Shen spec owns, and what it refused

`specs/mux.shen` grew by three rules. This note records why those three and not
the other four that were proposed, which bug in the ledger each one prevents,
and — the part that matters more — what is still Go's job and therefore still
has to be reviewed by hand.

The filter used throughout: **a rule earns its place only if it prevents a
defect that actually happened.** A spec that describes the system is
documentation with extra steps. This one is evaluated at runtime through
`shenguard.Reduce`, so anything in it is enforcement; anything in it that
nothing calls is worse than nothing, because it reads like enforcement.

---

## Refused first, because the refusals are the point

### 1. "A stream that ends must produce a detach" — not expressible. Don't fake it.

This was the headline request and the answer is no, for a structural reason
worth stating precisely rather than apologising for.

`mux.reduce` is a total function from `(session, clock, command)` to
`(state, effects)`. It runs when it is called. It has no way to observe that it
was *not* called, and therefore no way to require a call. A state machine over
commands can constrain what happens when you make a transition. It cannot
constrain its caller into making one. Every formulation that appears to do so —
an "obligation" field, a `pending-detach` marker, a `mux.stream-must-detach?`
predicate — turns out on inspection to be a fact the host writes into the state
and the host then has to remember to check. That is the same forgetting, moved.

So there is no rule in the spec that says a torn-down stream must detach. What
went in instead is the rule that makes forgetting survivable — see "Ownership is
an assertion with an age" below. The distinction is: don't add an obligation you
cannot enforce; remove the ability to be permanently wrong.

### 2. A liveness predicate. There is no `mux.online?` and there will not be one.

The controller's `GET /sessions` writes `Online: true` as a literal
(`internal/relay/tunnel.go`, `handleSessions`). It is a lie in three stacked
ways: the flag actually means "this device's websocket is in `c.agents`", it is
fanned out across every session that device declared, and those sessions are
strings from a CLI flag sent once at handshake and never revalidated. There is
no ping/pong and no idle read deadline, only an absolute credential deadline, so
a half-open TCP connection reads as online for hours.

None of that is reachable from `specs/mux.shen`. The controller is a different
process on a different host; the only path from it to a `shenguard.Session` is
websocket → agent bridge → ZMQ → daemon. There is no protocol frame that could
carry a per-session observation (`internal/relay/envelope.go` has nine frame
kinds and none of them is a metadata update), and `internal/relay` does not
import `shenguard` at all.

A `mux.observed-at` function added here would be a value nobody can read. It
would look like the sibling project's recency chain and prevent nothing. The
honest fix is a protocol change plus a controller-side `lastFrameAt`, and it is
named below as Go's job.

Recency did enter the model — but only at the one place the model can *act* on
it, which is the control lease. That is the whole difference between the
sibling's `presented-status` and a decorative timestamp: there, no constructor
produces "live"; here, no rule consults a clock except the one that has to.

### 3. Message-size, framing, and rate limits. Go's job, and they must stay there.

`zmqx.MaxMsgSize` was removed in `b8594f1` because it named a limit the adapter
did not enforce. Re-expressing it in Shen would repeat the mistake in a language
that makes it sound authoritative. The model sees payloads as opaque token ids
and must keep seeing them that way.

### 4. Publisher backpressure. Deliberately left alone — and it is a live gap.

The data plane's `ZMQPublisher.run` calls `socket.SendMultipart(frames, 0)` with
no deadline and no bounded sender, so a XPUB subscriber that stops draining can
park the publisher actor: bug 3's failure mode, on the other plane, still open.
That is a transport fix, not a semantics fix. Recorded here so it is not lost.

---

## What went in

### Rule A — Ownership is an assertion with an age

*Ledger: bug 1 (teardown obligation violated).*

The session gained one field, `Heard`: the instant at which the control owner
last **exercised** control. Reductions now carry a `clock` — `[Now Lease]`, with
`Lease > 0` verified by the datatype, so "the lease window is zero" (the shape
of a host that forgot to configure one, and which would otherwise make every
owner instantly stale) is not a representable clock.

The rule is one arm in `mux.reason-acquire`: `"control-owned"` is returned only
against an owner whose evidence is still inside the lease window. A stale owner
does not exclude a competing claim.

Two properties were chosen carefully and are tested:

**Only exercising control counts.** `Heard` is set by input, resize, and
acquisition. It is deliberately *not* set by attach, resync, PTY output, or
another client attaching — and there is no `touch`/keepalive command that could
set it. This is load-bearing: `client.Client` heartbeats every two seconds
whenever it is attached, and `handleControl` calls `runtime.Touch` on *any*
valid control message. In the leak that produced bug 1, the process holding the
lease was a relay proxy standing in for a browser that had closed, and its own
keepalive renewed the lease forever. `Runtime.ReapExpired` could never fire for
it. A keepalive proves a process is running; it does not prove an operator is
there.

**Preemption is not eviction.** `mux.reason-input` is clock-blind on purpose. A
quiet owner that types after an hour is accepted and re-proves its claim;
nothing kicks it out. Staleness is visible only to a competing claim. That keeps
the change free of a UX regression — no client had to gain a re-acquire path —
while still unwedging the session the moment a second browser asks.

**What this catches, honestly:** the *symptom* of bug 1, not its cause. The
leaked bridge stream and its muxd client still exist and stay attached; the
client list still grows across browser reconnects. What changes is that the
parked lease stops being an answer after the lease window, so the next browser
can take control instead of finding the session untypeable until the agent
restarts. If the fix in `fea6cc0` had never been written, this rule alone would
have turned a dead session into a two-second delay.

### Rule B — A failed delivery to one client is that client's problem

*Ledger: bugs 2 and 3 (departed peer killed the session; slow peer hung
everything).*

`mux.delivery-fatal?` is total, has one explicit arm (`"socket-closed"` → true),
and defaults to false. The Go side no longer decides; `deliveryOutcome` in
`internal/server/control.go` *names* an outcome in the model's vocabulary and
`mux.delivery-fatal?` classifies it.

The default arm is the entire point. The old code was a disjunction of
`errors.Is` sentinels, and everything it did not recognise fell through to
fatal — which is how bug 2 happened (routing errors unrecognised) and then
happened again in `7f608fb` (encode failures unrecognised). Two rounds of "we
missed one", both costing a whole session. The rule now defaults the other way,
and the reason it is safe to do so is written down where the decision lives:
this function is only reached from a send addressed to an identified client, and
nothing learned at such a send tells you anything about the socket. **The
socket's own death is observed on the receive path**, which classifies
independently (`zmqx.ErrClosed` is fatal there). A send never needs to conclude
it, and a send that concludes it wrongly costs everyone their shell.

Note this also gets bug 3's documented trap right for free: switching the driver
to the `Drop` overflow policy would surface a full queue as `ErrClosed`, which
the old classification called fatal — "strictly worse than the hang", per
`docs/PLAN-remaining-gaps.md`. Under the new rule an outcome learned at a
per-peer send is per-peer regardless of which errno it wore.

### Rule C — Teardown after a lost peer is idempotent

*Ledger: bugs 2 and 3, cleanup half.*

`mux.reduce-peer-lost` never rejects. If the client is attached it runs the
detach transition (membership removal and lease release commit together, through
the existing `mux.detach` → `mux.release-if-owner`); if it is not, it accepts
and changes nothing. `dropClient` previously had to swallow
`shenguard.ErrNotAttached` in Go — a judgement that had to be got right once per
failure path, and `7f608fb` exists because it was not.

`Runtime.PeerLost` replaces `Runtime.Detach` on that path. `Runtime.Detach`
remains for the explicit `KindDetach` request, where "not attached" *is* an
error worth reporting to the client that asked.

---

## What remains Go's job, and must stay carefully reviewed

1. **Making anyone detach.** Unchanged and unchangeable from here.
   `internal/agent/bridge.go`'s `closeActive` is still the only thing that
   releases and detaches on stream teardown, and it still runs only on paths
   that reach it. One path does not: the counter-replay branch in
   `handleMessageAccepted` opens a muxd client and then closes it inline without
   release/detach. Narrow, but real.

2. **The browser → agent close frame.** The actual root cause of bug 1. When a
   browser websocket dies, `Controller.handleBrowser`'s defer deletes the stream
   and releases the *controller's* policy lease, but never writes a
   `FrameClose` back to the agent. The agent therefore never learns the browser
   is gone. Agent-disconnect → browser is handled; browser-disconnect → agent is
   not. Rule A makes the resulting leak survivable; only this closes it.

3. **`/sessions` honesty.** Nothing in the model reaches it (see refusal 2). The
   minimum honest change is a controller-side `lastFrameAt` updated in the read
   loop and a rendered age instead of a boolean; the truthful version needs an
   idle read deadline with ping/pong, or a new agent→controller frame carrying
   per-session `Exited()`/`Clients()`.

4. **Naming delivery outcomes.** `deliveryOutcome` is still a hand-written
   `switch` over sentinel errors. What changed is its default: an unrecognised
   error becomes `DeliveryUnknown`, which the model calls per-peer, instead of
   falling through to fatal. A new transport error can now cost one client, not
   one session — but somebody still has to notice it and name it.

5. **The lease reaper's other clock.** `Runtime.ReapExpired` still decides
   staleness from `r.lastSeen`, which every control message refreshes, including
   keepalives. It was left alone on purpose: it is the mechanism that publishes a
   control event when a client goes silent altogether, and changing its input
   would have evicted idle operators. The two mechanisms now divide cleanly —
   the reaper handles "this client is not there at all", the model handles "this
   client is there but is not driving" — but they are two clocks, and that is
   the kind of thing that drifts. If `Touch` ever stops being called from
   `handleControl`, or `lastSeen` ever disagrees with `Clients()`, the reaper is
   where it will show.

6. **The publisher's unbounded send.** See refusal 4.

7. **Clock supply.** `Runtime.clockLocked` converts host time into the two
   numbers the model compares, anchored on `Runtime.started` so the readings are
   monotonic and nonnegative. The model cannot check that the host is telling
   the truth about the time. That is TCB, in the same sense as the sibling
   project's edge fsync — enumerated, not proven.
