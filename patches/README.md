# patches/

Changes to dependencies that we want but do not yet consume. Nothing here is
applied automatically: `go.mod` has no `replace` directive and there is no
vendor directory, so these are proposals, not builds.

## zmq4-bound-router-sends-and-frame-allocation.patch

Against `github.com/tomi77/zmq4@v1.0.0`.

**What it fixes.** `ROUTER.Send` takes a `context.Context` and never reads it.
Under the default `Block` overflow policy `pipe.send` waits on the outbound
channel or the *socket-level* `closeCh`, so a peer that stops draining parks the
caller until the whole socket closes. For shenmux that means one browser tab
that stops reading can wedge the control loop: no receives, no lease reaping,
nobody else served. See `docs/PLAN-remaining-gaps.md` item 1.

**Shape.** `pipe.send` takes a context and adds it to the `Block` select. Only
`ROUTER` passes its real context; `DEALER`, `PAIR`, `PUSH`, `REP` and `REQ` pass
`context.Background()`, preserving their current behaviour exactly.

**Why only ROUTER.** Threading the caller's context through every socket type is
the tidier change and is what I tried first. It made `TestREPFairQueue` fail on a
tree where it had just passed, so REQ/REP have blocking behaviour that something
depends on. ROUTER is the socket shenmux needs bounded, so the narrow change is
both sufficient and much easier to defend upstream. Someone taking this further
should understand the REQ/REP dependency first rather than assume it is
incidental.

**Verification, and its limits.** The patched driver builds, and a direct test of
`pipe.send` under `Block` confirms it now returns on a context deadline instead
of blocking indefinitely.

The driver's own suite cannot certify it. That suite is **flaky at baseline**:
over five runs of the unpatched tree, 2/5 failed (`TestREPFairQueue`,
"Recv: context deadline exceeded"). Patched samples have landed at 2/5 and 3/5
across runs, which is noise between flaky samples rather than evidence either
way. Do not read a clean patched run as a pass, and do not read a failing run as
a regression — check whether the same test fails on an unpatched tree first. I
nearly attributed the baseline flake to this patch.

## Second change in the same patch: a settable frame ceiling

**What it fixes.** A socket-level message-size limit can only be applied *after*
the driver has read a frame in full, so it bounds what a socket keeps rather
than what a peer can make it allocate. Measured: a socket configured for 1 KiB
still allocates a 4 MiB frame and only then refuses it. The driver has a hard
ceiling of 32 MiB (`wire.MaxFrameBodySize`) and `conn.WithMaxFrameBodySize` to
lower it, but nothing plumbs that from socket options, so it is unreachable.

**Shape.** `socketConfig` gains `maxFrameBodySize`, a `WithMaxFrameBodySize`
socket option sets it, and both handshakes pass it down. The handshake functions
already accepted `...conn.Option`, so nothing else changed.

**Verified with a negative control.** A DEALER pushes 4 MiB at a ROUTER built
with a 64 KiB ceiling. With the option the frame does not arrive; without it,
4,194,309 bytes are delivered. The test fails without the change, which is what
makes it worth having.

## Status

Neither change is submitted upstream or consumed here: `go.mod` has no `replace`
directive. Regression rate against the flaky baseline is unchanged (2/5 runs
failed, same as unpatched).

The in-tree work does not depend on either. `server.boundedSender` bounds sends
regardless, and `zmqx.WireFrameLimit` documents the allocation ceiling we cannot
currently lower. If these land upstream, the bound stays anyway -- it is what
makes the behaviour ours to test -- and `zmqx` gains the ability to pass a real
ceiling down instead of naming one it cannot enforce.
