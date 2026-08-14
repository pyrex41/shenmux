package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pyrex41/shenmux/protocol"
	"github.com/pyrex41/shenmux/internal/shenguard"
	"github.com/pyrex41/shenmux/internal/zmqx"
)

const readyTopicPrefix = "ready/"

type publishRequest struct {
	frames [][]byte
	reply  chan error
}

type readyRequest struct {
	cid   string
	reply chan error
}

// ZMQPublisher is a single-owner XPUB actor. Besides broadcasting ordered
// session frames, it observes each client's unique ready/<id> subscription.
// The control plane waits for this observation before returning an attach
// snapshot, eliminating the ordinary PUB/SUB slow-joiner race.
type ZMQPublisher struct {
	cancel    context.CancelFunc
	done      chan struct{}
	publish   chan publishRequest
	waitReady chan readyRequest

	mu     sync.Mutex
	runErr error
}

func NewZMQPublisher(parent context.Context, zctx *zmqx.Context, endpoint, session string) (*ZMQPublisher, error) {
	if zctx == nil {
		return nil, errors.New("nil ZeroMQ context")
	}
	if endpoint == "" || session == "" {
		return nil, errors.New("publisher endpoint and session must not be empty")
	}
	socket, err := zctx.Socket(zmqx.XPub)
	if err != nil {
		return nil, err
	}
	cleanup := func(err error) (*ZMQPublisher, error) {
		_ = socket.Close()
		return nil, err
	}
	for option, value := range map[int]int{
		zmqx.Linger: 0, zmqx.SndHWM: 10_000, zmqx.XPubVerbose: 1,
	} {
		if err := socket.SetInt(option, value); err != nil {
			return cleanup(err)
		}
	}
	if err := socket.SetInt64(zmqx.MaxMsgSize, 1024); err != nil {
		return cleanup(err)
	}
	if err := socket.Bind(endpoint); err != nil {
		return cleanup(err)
	}
	ctx, cancel := context.WithCancel(parent)
	publisher := &ZMQPublisher{
		cancel:    cancel,
		done:      make(chan struct{}),
		publish:   make(chan publishRequest),
		waitReady: make(chan readyRequest),
	}
	go publisher.run(ctx, socket, "session/"+session)
	return publisher, nil
}

func (p *ZMQPublisher) Publish(msg protocol.Message) error {
	frames, err := protocol.EncodePublished("session/"+msg.Meta.Session, msg)
	if err != nil {
		return err
	}
	req := publishRequest{frames: frames, reply: make(chan error, 1)}
	select {
	case p.publish <- req:
	case <-p.done:
		return p.errOrClosed()
	}
	select {
	case err := <-req.reply:
		return err
	case <-p.done:
		return p.errOrClosed()
	}
}

func (p *ZMQPublisher) WaitReady(ctx context.Context, cid shenguard.ClientID) error {
	req := readyRequest{cid: cid.String(), reply: make(chan error, 1)}
	select {
	case p.waitReady <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return p.errOrClosed()
	}
	select {
	case err := <-req.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return p.errOrClosed()
	}
}

func (p *ZMQPublisher) Close() error {
	p.cancel()
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runErr
}

func (p *ZMQPublisher) errOrClosed() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runErr != nil {
		return p.runErr
	}
	return errors.New("publisher is closed")
}

func (p *ZMQPublisher) run(ctx context.Context, socket *zmqx.Socket, topic string) {
	defer close(p.done)
	defer socket.Close()
	ready := make(map[string]int)
	waiters := make(map[string][]chan error)
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()

	notifyWaiters := func(err error) {
		for _, replies := range waiters {
			for _, reply := range replies {
				reply <- err
			}
		}
		waiters = map[string][]chan error{}
	}
	fail := func(err error) {
		p.mu.Lock()
		p.runErr = err
		p.mu.Unlock()
		notifyWaiters(err)
	}

	for {
		if err := drainSubscriptions(socket, ready, waiters); err != nil {
			fail(err)
			return
		}
		select {
		case <-ctx.Done():
			notifyWaiters(errors.New("publisher closed"))
			return
		case req := <-p.publish:
			// Runtime messages must use this actor's session topic. Encode again
			// only if a caller constructed a mismatched topic.
			frames := req.frames
			if len(frames) == 0 || string(frames[0]) != topic {
				req.reply <- fmt.Errorf("publisher topic mismatch: got %q want %q", firstFrame(frames), topic)
				continue
			}
			req.reply <- socket.SendMultipart(frames, 0)
		case req := <-p.waitReady:
			if ready[req.cid] > 0 {
				req.reply <- nil
			} else {
				waiters[req.cid] = append(waiters[req.cid], req.reply)
			}
		case <-ticker.C:
		}
	}
}

func drainSubscriptions(socket *zmqx.Socket, ready map[string]int, waiters map[string][]chan error) error {
	for {
		frames, err := socket.RecvMultipartLimit(zmqx.DontWait, 1024, 1)
		if errors.Is(err, zmqx.ErrWouldBlock) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(frames) != 1 || len(frames[0]) < 2 {
			continue
		}
		action := frames[0][0]
		topic := string(frames[0][1:])
		if !strings.HasPrefix(topic, readyTopicPrefix) {
			continue
		}
		cid := strings.TrimPrefix(topic, readyTopicPrefix)
		if cid == "" {
			continue
		}
		switch action {
		case 1:
			ready[cid]++
			for _, reply := range waiters[cid] {
				reply <- nil
			}
			delete(waiters, cid)
		case 0:
			if ready[cid] <= 1 {
				delete(ready, cid)
			} else {
				ready[cid]--
			}
		}
	}
}

func firstFrame(frames [][]byte) string {
	if len(frames) == 0 {
		return ""
	}
	return string(frames[0])
}
