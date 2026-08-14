package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pyrex41/shenmux/protocol"
	"github.com/pyrex41/shenmux/internal/zmqx"
)

type controlResult struct {
	msg protocol.Message
	err error
}

type controlRequest struct {
	msg   protocol.Message
	wait  bool
	ctx   context.Context
	reply chan controlResult
}

type controlActor struct {
	requests chan controlRequest
	done     chan struct{}
}

func newControlActor(ctx context.Context, socket *zmqx.Socket, errorsOut chan<- error) *controlActor {
	a := &controlActor{requests: make(chan controlRequest), done: make(chan struct{})}
	go a.run(ctx, socket, errorsOut)
	return a
}

func (a *controlActor) Call(ctx context.Context, msg protocol.Message) (protocol.Message, error) {
	req := controlRequest{msg: msg, wait: true, ctx: ctx, reply: make(chan controlResult, 1)}
	select {
	case a.requests <- req:
	case <-ctx.Done():
		return protocol.Message{}, ctx.Err()
	case <-a.done:
		return protocol.Message{}, errors.New("control actor closed")
	}
	select {
	case result := <-req.reply:
		return result.msg, result.err
	case <-ctx.Done():
		return protocol.Message{}, ctx.Err()
	case <-a.done:
		return protocol.Message{}, errors.New("control actor closed")
	}
}

func (a *controlActor) Send(ctx context.Context, msg protocol.Message) error {
	req := controlRequest{msg: msg, ctx: ctx, reply: make(chan controlResult, 1)}
	select {
	case a.requests <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-a.done:
		return errors.New("control actor closed")
	}
	select {
	case result := <-req.reply:
		return result.err
	case <-ctx.Done():
		return ctx.Err()
	case <-a.done:
		return errors.New("control actor closed")
	}
}

func (a *controlActor) Wait() { <-a.done }

func (a *controlActor) run(ctx context.Context, socket *zmqx.Socket, errorsOut chan<- error) {
	defer close(a.done)
	defer socket.Close()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	pending := make(map[uint64]chan controlResult)
	var nextID uint64 = 1
	failPending := func(err error) {
		for id, reply := range pending {
			reply <- controlResult{err: err}
			delete(pending, id)
		}
	}
	for {
		for {
			frames, err := socket.RecvMultipartLimit(zmqx.DontWait, protocol.MaxPayloadSize, 3)
			if errors.Is(err, zmqx.ErrWouldBlock) {
				break
			}
			if err != nil {
				failPending(err)
				reportActorError(errorsOut, err)
				return
			}
			msg, err := protocol.Decode(frames)
			if err != nil {
				reportActorError(errorsOut, err)
				continue
			}
			id := msg.Meta.RequestID
			if reply, ok := pending[id]; ok && id != 0 {
				delete(pending, id)
				reply <- controlResult{msg: msg}
			} else if msg.Kind == protocol.KindError {
				reportActorError(errorsOut, errors.New(msg.Meta.Error))
			}
		}
		select {
		case <-ctx.Done():
			failPending(ctx.Err())
			return
		case req := <-a.requests:
			if req.wait {
				if nextID == 0 {
					req.reply <- controlResult{err: errors.New("request id exhausted")}
					continue
				}
				req.msg.Meta.RequestID = nextID
				pending[nextID] = req.reply
				nextID++
			}
			frames, err := protocol.Encode(req.msg)
			if err == nil {
				err = socket.SendMultipartContext(req.ctx, frames, 0)
			}
			if err != nil {
				if req.wait {
					delete(pending, req.msg.Meta.RequestID)
				}
				req.reply <- controlResult{err: err}
				continue
			}
			if !req.wait {
				req.reply <- controlResult{}
			}
		case <-ticker.C:
		}
	}
}

func reportActorError(out chan<- error, err error) {
	if err == nil {
		return
	}
	select {
	case out <- fmt.Errorf("control: %w", err):
	default:
	}
}
