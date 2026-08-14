package server

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pyrex41/shenmux/protocol"
)

type blockingPublisher struct {
	started chan struct{}
	unblock chan struct{}
	once    sync.Once
	mu      sync.Mutex
	seqs    []uint64
}

func (p *blockingPublisher) Publish(msg protocol.Message) error {
	p.once.Do(func() { close(p.started) })
	<-p.unblock
	p.mu.Lock()
	p.seqs = append(p.seqs, msg.Meta.Seq)
	p.mu.Unlock()
	return nil
}

func TestStatePublisherBackpressureIsTerminal(t *testing.T) {
	pub := &blockingPublisher{started: make(chan struct{}), unblock: make(chan struct{})}
	actor, err := newStatePublisherWithLimits(pub, 1, 3*protocol.MaxMetadataSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := actor.Enqueue(protocol.Message{Kind: protocol.KindControl, Meta: protocol.Meta{Seq: 1}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pub.started:
	case <-time.After(time.Second):
		t.Fatal("publisher did not begin first send")
	}
	if err := actor.Enqueue(protocol.Message{Kind: protocol.KindControl, Meta: protocol.Meta{Seq: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := actor.Enqueue(protocol.Message{Kind: protocol.KindControl, Meta: protocol.Meta{Seq: 3}}); !errors.Is(err, ErrStatePublishBackpressure) {
		t.Fatalf("third enqueue error=%v", err)
	}
	select {
	case err := <-actor.Errors():
		if !errors.Is(err, ErrStatePublishBackpressure) {
			t.Fatalf("fatal error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("backpressure was not reported")
	}
	close(pub.unblock)
	if err := actor.Close(); !errors.Is(err, ErrStatePublishBackpressure) {
		t.Fatalf("close error=%v", err)
	}
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.seqs) != 2 || pub.seqs[0] != 1 || pub.seqs[1] != 2 {
		t.Fatalf("published sequences=%v", pub.seqs)
	}
}

func TestStatePublisherRejectsOutOfOrderSequence(t *testing.T) {
	pub := &recordPublisher{}
	actor, err := newStatePublisher(pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := actor.Enqueue(protocol.Message{Kind: protocol.KindControl, Meta: protocol.Meta{Seq: 2}}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-actor.Errors():
		if err == nil {
			t.Fatal("out-of-order publication produced nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("out-of-order publication was not rejected")
	}
	if err := actor.Close(); err == nil {
		t.Fatal("close did not report sequence failure")
	}
	if got := pub.Messages(); len(got) != 0 {
		t.Fatalf("out-of-order message reached transport: %+v", got)
	}
}
