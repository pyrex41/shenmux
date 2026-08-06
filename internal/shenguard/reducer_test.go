package shenguard

import (
	"testing"

	"github.com/pyrex41/shenmux/internal/shenmodel"
)

// testClock is the fixed clock the reducer tests reduce against. The lease is
// long enough that no test in this file crosses it by accident; preemption is
// tested explicitly with an advanced clock.
func testClock(t *testing.T) Clock {
	t.Helper()
	clock, err := NewClock(1000, 8000)
	if err != nil {
		t.Fatal(err)
	}
	return clock
}

func TestCommandValueUsesOpaqueTokenIDs(t *testing.T) {
	cid, err := NewClientID("c1")
	if err != nil {
		t.Fatal(err)
	}
	got := commandValue(Command{Kind: CommandInput, Client: cid, Token: 42})
	if len(got) != 3 || got[0] != "input" || got[1] != "c1" || got[2] != uint64(42) {
		t.Fatalf("command encoding = %#v", got)
	}
}

func TestProcessExitPreservesSignedCode(t *testing.T) {
	got := commandValue(Command{Kind: CommandProcessExit, Code: -1})
	if len(got) != 2 || got[1] != int(-1) {
		t.Fatalf("exit command encoding = %#v", got)
	}
	effects, err := effectsFromValue(shenmodel.List{
		shenmodel.List{"publish", uint64(1), "exit", int(-1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(effects) != 1 || effects[0].Code != -1 {
		t.Fatalf("exit effect = %#v", effects)
	}
}

func TestProcessExitRejectedDuringSnapshot(t *testing.T) {
	state, err := NewSession(mustDimensions(t, 80, 24))
	if err != nil {
		t.Fatal(err)
	}
	begin, err := Reduce(state, testClock(t), Command{Kind: CommandBeginSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Reduce(begin.State, testClock(t), Command{Kind: CommandProcessExit, Code: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsRejected() || result.Reason != ReasonWriterLocked {
		t.Fatalf("exit while locked = %#v", result)
	}
}

func TestAttachBarrierCommandEncoding(t *testing.T) {
	cid, err := NewClientID("c1")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := NewSnapshot(NewSeqNo(0), mustDimensions(t, 80, 24), 0, 0, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := commandValue(Command{Kind: CommandBeginAttach, Client: cid}); len(got) != 2 || got[0] != "begin-attach" || got[1] != "c1" {
		t.Fatalf("begin attach = %#v", got)
	}
	if got := commandValue(Command{Kind: CommandFinishAttach, Client: cid, Snapshot: snap}); len(got) != 3 || got[0] != "finish-attach" || got[1] != "c1" {
		t.Fatalf("finish attach = %#v", got)
	}
	if got := commandValue(Command{Kind: CommandBeginSnapshot, Client: cid}); len(got) != 1 || got[0] != "begin-snapshot" {
		t.Fatalf("begin snapshot = %#v", got)
	}
}

func mustDimensions(t *testing.T, cols, rows int) Dimensions {
	t.Helper()
	dim, err := NewDimensions(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	return dim
}

func TestResultFromValueAcceptedAndRejected(t *testing.T) {
	dim, err := NewDimensions(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewSession(dim)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := resultFromValue(shenmodel.List{"accepted", sessionValue(state), shenmodel.List{
		shenmodel.List{"write-pty", "c1", uint64(7)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.Accepted || len(accepted.Effects) != 1 || accepted.Effects[0].Token != 7 {
		t.Fatalf("accepted result = %#v", accepted)
	}
	rejected, err := resultFromValue(shenmodel.List{"rejected", "not-attached"})
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Accepted || rejected.Reason != ReasonNotAttached {
		t.Fatalf("rejected result = %#v", rejected)
	}
}

func TestReduceBeginAttachReturnsCaptureEffect(t *testing.T) {
	dim, err := NewDimensions(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewSession(dim)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := NewClientID("c1")
	if err != nil {
		t.Fatal(err)
	}
	result, err := Reduce(state, testClock(t), Command{Kind: CommandBeginAttach, Client: cid})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || len(result.Effects) != 1 || result.Effects[0].Kind != EffectCaptureSnapshot {
		t.Fatalf("result = %#v", result)
	}
	if pending, ok := result.State.PendingAttach(); !ok || pending != cid {
		t.Fatalf("pending attach = %v, %v", pending, ok)
	}
}

func TestReduceFinishAttachConsumesSnapshotBarrier(t *testing.T) {
	dim := mustDimensions(t, 80, 24)
	state, err := NewSession(dim)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := NewClientID("c1")
	if err != nil {
		t.Fatal(err)
	}
	started, err := Reduce(state, testClock(t), Command{Kind: CommandBeginAttach, Client: cid})
	if err != nil || !started.Accepted {
		t.Fatalf("begin attach: %#v %v", started, err)
	}
	snap, err := NewSnapshot(started.State.LastSeq(), dim, 0, 0, false, []byte("checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	finished, err := Reduce(started.State, testClock(t), Command{Kind: CommandFinishAttach, Client: cid, Snapshot: snap})
	if err != nil {
		t.Fatal(err)
	}
	if !finished.Accepted || len(finished.State.Clients()) != 1 {
		t.Fatalf("finish attach: %#v", finished)
	}
	if _, pending := finished.State.PendingAttach(); pending {
		t.Fatal("pending attach not cleared")
	}
}

func TestReduceAttachBarrierAlsoResynchronizesExistingClient(t *testing.T) {
	dim := mustDimensions(t, 80, 24)
	state, err := NewSession(dim)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := NewClientID("c1")
	if err != nil {
		t.Fatal(err)
	}
	attached, err := Reduce(state, testClock(t), Command{Kind: CommandAttach, Client: cid})
	if err != nil || !attached.Accepted {
		t.Fatalf("initial attach: %#v %v", attached, err)
	}
	started, err := Reduce(attached.State, testClock(t), Command{Kind: CommandBeginAttach, Client: cid})
	if err != nil || !started.Accepted {
		t.Fatalf("begin resync barrier: %#v %v", started, err)
	}
	snap, err := NewSnapshot(started.State.LastSeq(), dim, 0, 0, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	finished, err := Reduce(started.State, testClock(t), Command{Kind: CommandFinishAttach, Client: cid, Snapshot: snap})
	if err != nil || !finished.Accepted {
		t.Fatalf("finish resync barrier: %#v %v", finished, err)
	}
	if clients := finished.State.Clients(); len(clients) != 1 || clients[0] != cid {
		t.Fatalf("resync duplicated membership: %#v", clients)
	}
	if len(finished.Effects) != 1 || finished.Effects[0].Kind != EffectReply || finished.Effects[0].Client != cid {
		t.Fatalf("resync reply effect: %#v", finished.Effects)
	}
}

// A control lease parked on a client nobody can reach used to make a session
// untypeable for everyone else, permanently. Ownership is an assertion with an
// age: it stops excluding a competing claim once it is older than the lease.
func TestStaleOwnerDoesNotExcludeACompetingClaim(t *testing.T) {
	first, err := NewClientID("client-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewClientID("client-2")
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewSession(mustDimensions(t, 80, 24))
	if err != nil {
		t.Fatal(err)
	}
	for _, cid := range []ClientID{first, second} {
		result, err := Reduce(state, testClock(t), Command{Kind: CommandAttach, Client: cid})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Accepted {
			t.Fatalf("attach %s = %s", cid, result.Reason)
		}
		state = result.State
	}
	owned, err := Reduce(state, mustClock(t, 1000, 8000), Command{Kind: CommandAcquireControl, Client: first})
	if err != nil {
		t.Fatal(err)
	}
	if !owned.Accepted {
		t.Fatalf("acquire = %s", owned.Reason)
	}
	if owned.State.Heard() != 1000 {
		t.Fatalf("acquisition must be evidence of control, heard = %d", owned.State.Heard())
	}

	fresh, err := Reduce(owned.State, mustClock(t, 9000, 8000), Command{Kind: CommandAcquireControl, Client: second})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Accepted || fresh.Reason != ReasonControlOwned {
		t.Fatalf("a fresh owner must still exclude: %#v", fresh)
	}

	stale, err := Reduce(owned.State, mustClock(t, 9001, 8000), Command{Kind: CommandAcquireControl, Client: second})
	if err != nil {
		t.Fatal(err)
	}
	if !stale.Accepted {
		t.Fatalf("a stale owner must not exclude: %s", stale.Reason)
	}
	if owner, ok := stale.State.ControlOwner(); !ok || owner != second {
		t.Fatalf("control owner = %v/%v", owner, ok)
	}
	if stale.State.Heard() != 9001 {
		t.Fatalf("the new owner must start its own lease, heard = %d", stale.State.Heard())
	}

	// Preemption is not eviction: nothing displaces a quiet owner that is still
	// the only one asking.
	late, err := Reduce(owned.State, mustClock(t, 100000, 8000), Command{Kind: CommandInput, Client: first, Token: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !late.Accepted {
		t.Fatalf("a quiet owner must still be able to type: %s", late.Reason)
	}
	if late.State.Heard() != 100000 {
		t.Fatalf("typing must re-prove the claim, heard = %d", late.State.Heard())
	}
}

// Teardown after a per-peer delivery failure has to be safe to call from every
// failure path, including ones that never got as far as an attach.
func TestPeerLostIsIdempotent(t *testing.T) {
	cid, err := NewClientID("client-1")
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewSession(mustDimensions(t, 80, 24))
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := Reduce(state, testClock(t), Command{Kind: CommandPeerLost, Client: cid})
	if err != nil {
		t.Fatal(err)
	}
	if !stranger.Accepted || len(stranger.Effects) != 0 {
		t.Fatalf("losing a peer that never attached = %#v", stranger)
	}

	attached, err := Reduce(state, testClock(t), Command{Kind: CommandAttach, Client: cid})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := Reduce(attached.State, testClock(t), Command{Kind: CommandAcquireControl, Client: cid})
	if err != nil {
		t.Fatal(err)
	}
	lost, err := Reduce(owned.State, testClock(t), Command{Kind: CommandPeerLost, Client: cid})
	if err != nil {
		t.Fatal(err)
	}
	if !lost.Accepted {
		t.Fatalf("peer-lost = %s", lost.Reason)
	}
	if IsAttached(lost.State, cid) {
		t.Fatal("a lost peer must be detached")
	}
	if _, ok := lost.State.ControlOwner(); ok {
		t.Fatal("a lost peer's lease must be released")
	}
	if lost.State.Heard() != 0 {
		t.Fatalf("release must clear the evidence, heard = %d", lost.State.Heard())
	}
	if len(lost.Effects) != 1 || lost.Effects[0].Kind != EffectPublish || lost.Effects[0].EventKind != "control" {
		t.Fatalf("the release must be published: %#v", lost.Effects)
	}

	again, err := Reduce(lost.State, testClock(t), Command{Kind: CommandPeerLost, Client: cid})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Accepted || len(again.Effects) != 0 {
		t.Fatalf("losing the same peer twice = %#v", again)
	}
}

// A reply that cannot be delivered to one named client used to end the session
// for everyone. The default arm is the one that matters: an outcome nobody has
// named yet is one peer's problem, because it was learned at a send addressed
// to one peer.
func TestDeliveryFatalOnlyForTheSocket(t *testing.T) {
	if !DeliveryFatal(DeliverySocketClosed) {
		t.Fatal("a dead socket must end the session")
	}
	for _, outcome := range []string{
		DeliveryNoRoute, DeliveryNoIdentity, DeliveryNotDraining, DeliveryEncodeFault,
		DeliveryUnknown, "some-outcome-invented-later", "",
	} {
		if DeliveryFatal(outcome) {
			t.Fatalf("outcome %q must not end the session", outcome)
		}
	}
}

// Ownership evidence with no owner would let a later acquisition inherit a
// stranger's freshness, so the host may not hand such a session back.
func TestSessionRejectsEvidenceWithoutAnOwner(t *testing.T) {
	_, err := sessionFromValue(shenmodel.List{
		shenmodel.List{}, uint64(0),
		shenmodel.List{uint64(0), shenmodel.List{uint64(80), uint64(24)}, uint64(0), uint64(0), false, ""},
		shenmodel.List{uint64(80), uint64(24)}, false, false,
		shenmodel.List{shenmodel.List{}, shenmodel.List{}}, uint64(1000),
	})
	if err == nil {
		t.Fatal("control evidence with no owner must be refused")
	}
}

func mustClock(t *testing.T, now, lease uint64) Clock {
	t.Helper()
	clock, err := NewClock(now, lease)
	if err != nil {
		t.Fatal(err)
	}
	return clock
}

func TestClockRefusesAZeroLeaseWindow(t *testing.T) {
	if _, err := NewClock(1000, 0); err == nil {
		t.Fatal("a zero lease window would make every owner instantly stale")
	}
}
