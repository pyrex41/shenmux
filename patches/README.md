# patches/

Changes to dependencies that we want but do not yet consume. Nothing here is
applied automatically: `go.mod` has no `replace` directive and there is no
vendor directory, so these are proposals, not builds.

## zmq4-router-send-honours-context.patch

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
"Recv: context deadline exceeded"); the patched tree failed 3/5. That difference
is noise between two flaky samples, not evidence either way. Do not read the
single clean patched run as a pass, and do not read a failing run as a
regression — check whether the same test fails on an unpatched tree first. I
nearly attributed the baseline flake to this patch.

**Status.** Not submitted upstream. Not consumed here. The in-tree fix
(`server.boundedSender`) does not depend on it; if this ever lands upstream the
bound stays anyway, because it is what makes the behaviour ours to test.
