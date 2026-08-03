package shenguard

import "testing"

func mustDim(t *testing.T, cols, rows int) Dimensions {
	t.Helper()
	d, err := NewDimensions(cols, rows)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustCID(t *testing.T, id string) ClientID {
	t.Helper()
	c, err := NewClientID(id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSessionTransitions(t *testing.T) {
	dim := mustDim(t, 80, 24)
	s, err := NewSession(dim)
	if err != nil {
		t.Fatal(err)
	}
	cid := mustCID(t, "client-1")
	if !AttachOK(s, cid) {
		t.Fatal("new client should be attachable")
	}

	s, err = BeginSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	if AcceptInput(s, cid) {
		t.Fatal("input must be rejected while snapshot lock is held")
	}
	snap, err := NewSnapshot(s.LastSeq(), dim, 0, 0, false, []byte("snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	s, err = EndSnapshot(s, snap)
	if err != nil {
		t.Fatal(err)
	}
	s, err = Attach(s, cid)
	if err != nil {
		t.Fatal(err)
	}
	if AcceptInput(s, cid) {
		t.Fatal("attached observer should not be accepted before control acquisition")
	}
	s, err = AcquireControl(s, cid)
	if err != nil {
		t.Fatal(err)
	}
	if !AcceptInput(s, cid) || !AcceptResize(s, cid) {
		t.Fatal("controller should be accepted")
	}

	seq, err := NextSeq(s.LastSeq())
	if err != nil {
		t.Fatal(err)
	}
	s, err = ApplyEvent(s, seq, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if s.LastSeq() != seq {
		t.Fatal("sequence did not advance")
	}

	newDim := mustDim(t, 100, 40)
	seq, _ = NextSeq(s.LastSeq())
	s, err = ApplyEvent(s, seq, &newDim, false)
	if err != nil {
		t.Fatal(err)
	}
	if s.Dim() != newDim {
		t.Fatal("resize did not update dimensions")
	}

	s, err = Detach(s, cid)
	if err != nil {
		t.Fatal(err)
	}
	if AcceptInput(s, cid) {
		t.Fatal("detached client should be rejected")
	}
}

func TestRejectsInvalidValuesAndSequenceGaps(t *testing.T) {
	if _, err := NewClientID(""); err == nil {
		t.Fatal("empty client id accepted")
	}
	if _, err := NewDimensions(0, 24); err == nil {
		t.Fatal("zero columns accepted")
	}

	s, err := NewSession(mustDim(t, 80, 24))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyEvent(s, NewSeqNo(2), nil, false); err == nil {
		t.Fatal("sequence gap accepted")
	}
}

func TestExclusiveControlOwnership(t *testing.T) {
	s, err := NewSession(mustDim(t, 80, 24))
	if err != nil {
		t.Fatal(err)
	}
	first := mustCID(t, "first")
	second := mustCID(t, "second")
	for _, cid := range []ClientID{first, second} {
		s, err = Attach(s, cid)
		if err != nil {
			t.Fatal(err)
		}
	}
	s, err = AcquireControl(s, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireControl(s, second); err != ErrControlOwned {
		t.Fatalf("second acquisition error = %v", err)
	}
	if AcceptInput(s, second) || !AcceptInput(s, first) {
		t.Fatal("control predicate is not exclusive")
	}
	s, err = Detach(s, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ControlOwner(); ok {
		t.Fatal("detaching controller did not release ownership")
	}
	if s, err = AcquireControl(s, second); err != nil {
		t.Fatal(err)
	}
}
