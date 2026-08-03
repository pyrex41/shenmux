package server

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	defaultPTYWriteQueue = 256
	maxQueuedPTYBytes    = 8 << 20
)

var ErrPTYBackpressure = errors.New("PTY writer backpressure limit exceeded")

type ptyWriter struct {
	writer io.Writer
	queue  chan []byte
	done   chan struct{}
	errors chan error

	mu          sync.Mutex
	closed      bool
	queuedBytes int
	runErr      error
	closeOnce   sync.Once
}

func newPTYWriter(writer io.Writer) (*ptyWriter, error) {
	if writer == nil {
		return nil, errors.New("nil PTY writer")
	}
	actor := &ptyWriter{
		writer: writer, queue: make(chan []byte, defaultPTYWriteQueue),
		done: make(chan struct{}), errors: make(chan error, 1),
	}
	go actor.run()
	return actor, nil
}

// Enqueue copies bytes into the bounded writer actor. It never performs a PTY
// write while the runtime state mutex is held.
func (w *ptyWriter) Enqueue(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	copyPayload := append([]byte(nil), payload...)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		if w.runErr != nil {
			return w.runErr
		}
		return errors.New("PTY writer is closed")
	}
	if len(copyPayload) > maxQueuedPTYBytes || w.queuedBytes > maxQueuedPTYBytes-len(copyPayload) {
		return fmt.Errorf("queued PTY bytes would exceed %d: %w", maxQueuedPTYBytes, ErrPTYBackpressure)
	}
	select {
	case w.queue <- copyPayload:
		w.queuedBytes += len(copyPayload)
		return nil
	default:
		return ErrPTYBackpressure
	}
}

func (w *ptyWriter) Errors() <-chan error { return w.errors }

func (w *ptyWriter) Close() error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		close(w.queue)
		w.mu.Unlock()
		<-w.done
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.runErr
}

func (w *ptyWriter) run() {
	defer close(w.done)
	defer close(w.errors)
	for payload := range w.queue {
		err := writeAll(w.writer, payload)
		w.mu.Lock()
		w.queuedBytes -= len(payload)
		if err != nil && w.runErr == nil {
			w.runErr = err
			w.closed = true
		}
		w.mu.Unlock()
		if err != nil {
			select {
			case w.errors <- err:
			default:
			}
			return
		}
	}
}

func writeAll(w io.Writer, payload []byte) error {
	for len(payload) != 0 {
		n, err := w.Write(payload)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(payload) {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}
