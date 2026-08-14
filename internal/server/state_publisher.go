package server

import (
	"errors"
	"fmt"
	"sync"

	"github.com/pyrex41/shenmux/protocol"
)

const (
	defaultStatePublishQueue = 4096
	maxQueuedStateBytes      = 64 << 20
)

var ErrStatePublishBackpressure = errors.New("state publication backpressure limit exceeded")

// statePublisher is the single ordered handoff from committed runtime state to
// the transport publisher. Runtime methods enqueue while holding the state
// mutex, so channel order is commit order; potentially blocking ZeroMQ sends
// happen on this actor instead of under the state lock.
//
// Enqueue failures are terminal. A state transition has already committed by
// the time it reaches this boundary, so continuing after a dropped publication
// would make every subscriber's sequence stream permanently incomplete. The
// actor therefore closes its input, reports the failure, and forces the daemon
// to terminate rather than pretending it can recover.
type statePublisher struct {
	publisher Publisher
	queue     chan protocol.Message
	done      chan struct{}
	errors    chan error
	maxBytes  int

	mu          sync.Mutex
	closed      bool
	queueClosed bool
	queuedBytes int
	runErr      error
	closeOnce   sync.Once
}

func newStatePublisher(publisher Publisher) (*statePublisher, error) {
	return newStatePublisherWithLimits(publisher, defaultStatePublishQueue, maxQueuedStateBytes)
}

func newStatePublisherWithLimits(publisher Publisher, queueCapacity, maxBytes int) (*statePublisher, error) {
	if publisher == nil {
		return nil, errors.New("nil state publisher")
	}
	if queueCapacity <= 0 {
		return nil, errors.New("state publisher queue capacity must be positive")
	}
	if maxBytes <= 0 {
		return nil, errors.New("state publisher byte limit must be positive")
	}
	actor := &statePublisher{
		publisher: publisher,
		queue:     make(chan protocol.Message, queueCapacity),
		done:      make(chan struct{}),
		errors:    make(chan error, 1),
		maxBytes:  maxBytes,
	}
	go actor.run()
	return actor, nil
}

func (p *statePublisher) Enqueue(msg protocol.Message) error {
	if msg.Meta.Seq == 0 {
		return p.abort(errors.New("state publication sequence must be positive"))
	}
	copyMsg := msg
	copyMsg.Payload = append([]byte(nil), msg.Payload...)
	size := publicationSize(copyMsg)

	p.mu.Lock()
	if p.closed {
		err := p.closedErrorLocked()
		p.mu.Unlock()
		return err
	}
	if size > p.maxBytes || p.queuedBytes > p.maxBytes-size {
		err := fmt.Errorf("queued state bytes would exceed %d: %w", p.maxBytes, ErrStatePublishBackpressure)
		p.abortLocked(err)
		p.mu.Unlock()
		return err
	}
	select {
	case p.queue <- copyMsg:
		p.queuedBytes += size
		p.mu.Unlock()
		return nil
	default:
		err := ErrStatePublishBackpressure
		p.abortLocked(err)
		p.mu.Unlock()
		return err
	}
}

func (p *statePublisher) Errors() <-chan error { return p.errors }

func (p *statePublisher) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.closeQueueLocked()
		p.mu.Unlock()
		<-p.done
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runErr
}

func (p *statePublisher) run() {
	defer close(p.done)
	var lastSeq uint64
	for msg := range p.queue {
		size := publicationSize(msg)
		if msg.Meta.Seq != lastSeq+1 {
			p.failFromRun(fmt.Errorf("state publication seq %d, expected %d", msg.Meta.Seq, lastSeq+1), size)
			return
		}
		if err := p.publisher.Publish(msg); err != nil {
			p.failFromRun(fmt.Errorf("publish state seq %d: %w", msg.Meta.Seq, err), size)
			return
		}
		lastSeq = msg.Meta.Seq
		p.mu.Lock()
		p.queuedBytes -= size
		p.mu.Unlock()
	}
}

func (p *statePublisher) abort(err error) error {
	p.mu.Lock()
	if !p.closed {
		p.abortLocked(err)
	} else {
		err = p.closedErrorLocked()
	}
	p.mu.Unlock()
	return err
}

// abortLocked transitions to terminal failure. The error is delivered before
// the queue is closed, so the run goroutine cannot close/exit ahead of the
// notification. The error channel intentionally remains open; consumers only
// need the first fatal error and this avoids send-vs-close races.
func (p *statePublisher) abortLocked(err error) {
	if p.runErr == nil {
		p.runErr = err
	}
	p.closed = true
	select {
	case p.errors <- p.runErr:
	default:
	}
	p.closeQueueLocked()
}

func (p *statePublisher) failFromRun(err error, consumedBytes int) {
	p.mu.Lock()
	p.queuedBytes -= consumedBytes
	p.abortLocked(err)
	p.mu.Unlock()
}

func (p *statePublisher) closeQueueLocked() {
	if !p.queueClosed {
		close(p.queue)
		p.queueClosed = true
	}
}

func (p *statePublisher) closedErrorLocked() error {
	if p.runErr != nil {
		return p.runErr
	}
	return errors.New("state publisher is closed")
}

func publicationSize(msg protocol.Message) int {
	return len(msg.Payload) + protocol.MaxMetadataSize
}
