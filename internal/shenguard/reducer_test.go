package shenguard

import (
	"testing"

	"github.com/pyrex41/shenmux/internal/shenmodel"
)

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
	begin, err := Reduce(state, Command{Kind: CommandBeginSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Reduce(begin.State, Command{Kind: CommandProcessExit, Code: 7})
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
	result, err := Reduce(state, Command{Kind: CommandBeginAttach, Client: cid})
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
	started, err := Reduce(state, Command{Kind: CommandBeginAttach, Client: cid})
	if err != nil || !started.Accepted {
		t.Fatalf("begin attach: %#v %v", started, err)
	}
	snap, err := NewSnapshot(started.State.LastSeq(), dim, 0, 0, false, []byte("checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	finished, err := Reduce(started.State, Command{Kind: CommandFinishAttach, Client: cid, Snapshot: snap})
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
	attached, err := Reduce(state, Command{Kind: CommandAttach, Client: cid})
	if err != nil || !attached.Accepted {
		t.Fatalf("initial attach: %#v %v", attached, err)
	}
	started, err := Reduce(attached.State, Command{Kind: CommandBeginAttach, Client: cid})
	if err != nil || !started.Accepted {
		t.Fatalf("begin resync barrier: %#v %v", started, err)
	}
	snap, err := NewSnapshot(started.State.LastSeq(), dim, 0, 0, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	finished, err := Reduce(started.State, Command{Kind: CommandFinishAttach, Client: cid, Snapshot: snap})
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
