// Package zmqx exposes the small ZeroMQ surface used by shenmux.
//
// The implementation is pure Go and speaks ZMTP 3.1 through tomi77/zmq4.
// Keeping this adapter local lets the session/client code retain its existing
// multipart API while the project remains free of a libzmq or CGO runtime
// dependency.
package zmqx

import (
	"context"
	"errors"
	"fmt"
	"sync"

	zmq "github.com/tomi77/zmq4"
)

type SocketType int

const (
	Pair SocketType = iota
	Pub
	Sub
	_ // req
	_ // rep
	Dealer
	Router
	Pull
	Push
	XPub
)

const (
	DontWait = 1
	SndMore  = 2
)

// These values retain the stable libzmq option numbers used by the callers.
// The pure-Go adapter translates the options that affect shenmux behavior and
// enforces receive message-size limits locally. Linger, Immediate and
// XPubVerbose are harmless compatibility no-ops. The two timeouts are not:
// SndTimeout and RcvTimeout are rejected outright rather than accepted and
// ignored, because a socket option that returns nil and changes nothing is
// worse than one that is missing -- see SetInt.
const (
	Identity    = 5
	Subscribe   = 6
	Unsubscribe = 7
	RcvMore     = 13
	Linger      = 17
	MaxMsgSize  = 22
	SndHWM      = 23
	RcvHWM      = 24
	RcvTimeout  = 27
	SndTimeout  = 28
	Immediate   = 39
	XPubVerbose = 40
)

const (
	defaultMaxFrame  = 256 << 20
	defaultMaxFrames = 16

	// WireFrameLimit is the largest frame body a peer can make this process
	// allocate, enforced by the driver's frame reader before any socket-level
	// limit is consulted. It is the honest ceiling: MaxMsgSize decides what a
	// socket keeps, this decides what it buffers.
	//
	// It mirrors wire.MaxFrameBodySize in the driver, which is unexported and
	// has no socket option to lower it. If the driver changes it, the constant
	// here is wrong and TestWireFrameLimitMatchesTheDriver fails.
	WireFrameLimit = 32 << 20
)

var (
	ErrWouldBlock = errors.New("zmq operation would block")
	ErrClosed     = errors.New("zmq object is closed")

	// ErrSendTimeoutUnsupported rejects SndTimeout. The pure-Go driver has no
	// way to abandon a send: ROUTER.Send and its siblings take a context and
	// never read it, so a send blocks until the peer drains or the socket
	// closes. Silently accepting a send timeout would hand callers a guarantee
	// this adapter cannot keep.
	ErrSendTimeoutUnsupported = errors.New("zmq send timeout is unsupported: this driver cannot abandon a send, bound it in the caller")

	// ErrRecvTimeoutUnsupported rejects RcvTimeout for the same reason
	// SndTimeout is rejected: this adapter never applied it. Receive deadlines
	// are expressed per call -- DontWait, or the socket context -- both of
	// which the driver's Recv honours. Accepting a socket-level receive
	// timeout and ignoring it would be the same silent lie.
	ErrRecvTimeoutUnsupported = errors.New("zmq receive timeout is unsupported: pass DontWait or use the socket context per call")

	// ErrNoRoute and ErrNoIdentity are re-exported so callers can tell a
	// per-peer delivery failure from a dead socket without importing zmq4
	// past this adapter. mapError passes both through untouched, so
	// errors.Is works on anything zmqx returns.
	ErrNoRoute    = zmq.ErrNoRoute
	ErrNoIdentity = zmq.ErrNoIdentity
)

type Context struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	sockets map[*Socket]struct{}
	closed  bool
}

func NewContext() (*Context, error) {
	ctx, cancel := context.WithCancel(context.Background())
	return &Context{ctx: ctx, cancel: cancel, sockets: make(map[*Socket]struct{})}, nil
}

func (c *Context) Socket(kind SocketType) (*Socket, error) {
	if c == nil {
		return nil, ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	s := &Socket{ctx: c, kind: kind, maxFrame: defaultMaxFrame}
	c.sockets[s] = struct{}{}
	return s, nil
}

// Shutdown cancels operations using this context without eagerly closing the
// sockets. Callers normally close sockets and then Close the context.
func (c *Context) Shutdown() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.cancel()
	}
	return nil
}

func (c *Context) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.cancel()
	sockets := make([]*Socket, 0, len(c.sockets))
	for s := range c.sockets {
		sockets = append(sockets, s)
	}
	c.mu.Unlock()
	for _, s := range sockets {
		_ = s.Close()
	}
	return nil
}

type rawSocket interface {
	Bind(context.Context, string) error
	Connect(context.Context, string) error
	Close() error
}

type rawSender interface {
	Send(context.Context, zmq.Message) error
}

type rawReceiver interface {
	Recv(context.Context) (zmq.Message, error)
}

type Socket struct {
	mu       sync.Mutex
	ctx      *Context
	kind     SocketType
	raw      rawSocket
	closed   bool
	identity []byte
	sndHWM   int
	rcvHWM   int
	maxFrame int
	subs     []struct {
		topic string
		add   bool
	}
}

func (s *Socket) ensureRaw() (rawSocket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx == nil {
		return nil, ErrClosed
	}
	if s.raw != nil {
		return s.raw, nil
	}
	opts := make([]zmq.Option, 0, 3)
	if len(s.identity) > 0 {
		opts = append(opts, zmq.WithIdentity(s.identity))
	}
	if s.sndHWM > 0 {
		opts = append(opts, zmq.WithSndHWM(s.sndHWM))
	}
	if s.rcvHWM > 0 {
		opts = append(opts, zmq.WithRcvHWM(s.rcvHWM))
	}
	switch s.kind {
	case Pair:
		s.raw = zmq.NewPAIR(opts...)
	case Pub:
		s.raw = zmq.NewPUB(opts...)
	case Sub:
		s.raw = zmq.NewSUB(opts...)
	case Dealer:
		s.raw = zmq.NewDEALER(opts...)
	case Router:
		s.raw = zmq.NewROUTER(opts...)
	case Pull:
		s.raw = zmq.NewPULL(opts...)
	case Push:
		s.raw = zmq.NewPUSH(opts...)
	case XPub:
		s.raw = zmq.NewXPUB(opts...)
	default:
		return nil, fmt.Errorf("unsupported ZeroMQ socket type %d", s.kind)
	}
	if sub, ok := s.raw.(*zmq.SUB); ok {
		for _, entry := range s.subs {
			var err error
			if entry.add {
				err = sub.Subscribe(entry.topic)
			} else {
				err = sub.Unsubscribe(entry.topic)
			}
			if err != nil {
				return nil, err
			}
		}
	}
	return s.raw, nil
}

func (s *Socket) Bind(endpoint string) error {
	raw, err := s.ensureRaw()
	if err != nil {
		return err
	}
	return raw.Bind(s.ctx.ctx, endpoint)
}

func (s *Socket) Connect(endpoint string) error {
	raw, err := s.ensureRaw()
	if err != nil {
		return err
	}
	return raw.Connect(s.ctx.ctx, endpoint)
}

func (s *Socket) SetInt(option, value int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if option == RcvTimeout {
		return ErrRecvTimeoutUnsupported
	}
	if option == SndTimeout {
		// Rejected rather than ignored. This driver cannot bound a send at all:
		// every Send takes a context.Context and discards it, and under the
		// default blocking overflow policy a full queue parks the caller until
		// the socket itself closes. Accepting the option would return a clean
		// nil and change nothing, which is the worst possible answer to give
		// someone reaching for it -- they are, by definition, trying to stop a
		// send from blocking forever. Bound the send in the caller instead; see
		// server.boundedSender.
		return ErrSendTimeoutUnsupported
	}
	if s.raw != nil && option != Linger && option != Immediate && option != XPubVerbose {
		return errors.New("zmq socket options must be set before bind or connect")
	}
	switch option {
	case SndHWM:
		if value <= 0 {
			return errors.New("zmq send HWM must be positive")
		}
		s.sndHWM = value
	case RcvHWM:
		if value <= 0 {
			return errors.New("zmq receive HWM must be positive")
		}
		s.rcvHWM = value
	case Linger, Immediate, XPubVerbose:
		// Accepted and inert, and unlike the rejected timeouts that is
		// defensible: this adapter closes explicitly rather than lingering,
		// and connects eagerly, so there is nothing for these to configure.
		// They stay accepted because callers set them (control.go,
		// publisher.go) and rejecting them would break working code to make a
		// point.
	default:
		return fmt.Errorf("unsupported ZeroMQ integer option %d", option)
	}
	return nil
}

// SetInt64 sets MaxMsgSize, which bounds what this socket will ACCEPT, not what
// a peer can make it ALLOCATE. The driver reads a frame off the wire in full
// and only then hands it over, so this limit is applied to a buffer that
// already exists: a socket configured for 1 KiB still allocates a 4 MiB frame
// before refusing it (measured, not inferred).
//
// The real allocation bound is WireFrameLimit, enforced by the driver's frame
// reader. Callers who need a tighter one cannot get it here -- the driver
// exposes no socket option for it. See patches/ for the change that would.
func (s *Socket) SetInt64(option int, value int64) error {
	if option != MaxMsgSize {
		return fmt.Errorf("unsupported ZeroMQ int64 option %d", option)
	}
	if value <= 0 || value > int64(defaultMaxFrame) {
		return errors.New("zmq maximum message size is out of range")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.maxFrame = int(value)
	return nil
}

func (s *Socket) SetBytes(option int, value []byte) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	switch option {
	case Identity:
		if len(value) == 0 || len(value) > 255 {
			s.mu.Unlock()
			return errors.New("zmq identity must contain 1..255 bytes")
		}
		if s.raw != nil {
			s.mu.Unlock()
			return errors.New("zmq identity must be set before bind or connect")
		}
		s.identity = append([]byte(nil), value...)
		s.mu.Unlock()
		return nil
	case Subscribe, Unsubscribe:
		if s.kind != Sub {
			s.mu.Unlock()
			return fmt.Errorf("ZeroMQ option %d requires a SUB socket", option)
		}
		s.subs = append(s.subs, struct {
			topic string
			add   bool
		}{topic: string(value), add: option == Subscribe})
		raw := s.raw
		s.mu.Unlock()
		if raw == nil {
			return nil
		}
		sub := raw.(*zmq.SUB)
		if option == Subscribe {
			return sub.Subscribe(string(value))
		}
		return sub.Unsubscribe(string(value))
	default:
		s.mu.Unlock()
		return fmt.Errorf("unsupported ZeroMQ byte option %d", option)
	}
}

func (s *Socket) SendMultipart(frames [][]byte, flags int) error {
	return s.SendMultipartContext(s.ctx.ctx, frames, flags)
}

// SendMultipartContext is SendMultipart with a caller-controlled cancellation
// context. It is important for DEALER sockets, whose pure-Go implementation
// waits for a route to become available when the peer has disconnected.
func (s *Socket) SendMultipartContext(ctx context.Context, frames [][]byte, flags int) error {
	if len(frames) == 0 {
		return errors.New("zmq multipart message needs at least one frame")
	}
	if flags&^SndMore != 0 {
		return fmt.Errorf("unsupported ZeroMQ send flags %d", flags)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	s.mu.Unlock()
	raw, err := s.ensureRaw()
	if err != nil {
		return err
	}
	sender, ok := raw.(rawSender)
	if !ok {
		return fmt.Errorf("ZeroMQ socket type %d cannot send", s.kind)
	}
	msg := make(zmq.Message, len(frames))
	for i, frame := range frames {
		msg[i] = append([]byte(nil), frame...)
	}
	if ctx == nil {
		ctx = s.ctx.ctx
	}
	if err := sender.Send(ctx, msg); err != nil {
		return mapError(err)
	}
	return nil
}

func (s *Socket) RecvMultipart(flags int) ([][]byte, error) {
	return s.RecvMultipartLimit(flags, defaultMaxFrame, defaultMaxFrames)
}

func (s *Socket) RecvMultipartLimit(flags, maxFrame, maxFrames int) ([][]byte, error) {
	if maxFrame <= 0 || maxFrames <= 0 {
		return nil, errors.New("zmq receive limits must be positive")
	}
	if flags&^DontWait != 0 {
		return nil, fmt.Errorf("unsupported ZeroMQ receive flags %d", flags)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	if s.maxFrame < maxFrame {
		maxFrame = s.maxFrame
	}
	s.mu.Unlock()
	raw, err := s.ensureRaw()
	if err != nil {
		return nil, err
	}
	receiver, ok := raw.(rawReceiver)
	if !ok {
		return nil, fmt.Errorf("ZeroMQ socket type %d cannot receive", s.kind)
	}
	recvCtx := s.ctx.ctx
	var cancel context.CancelFunc
	if flags&DontWait != 0 {
		recvCtx, cancel = context.WithTimeout(recvCtx, 0)
		defer cancel()
	}
	msg, err := receiver.Recv(recvCtx)
	if err != nil {
		if flags&DontWait != 0 && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			return nil, ErrWouldBlock
		}
		return nil, mapError(err)
	}
	if len(msg) > maxFrames {
		return nil, fmt.Errorf("zmq multipart message exceeds %d frames", maxFrames)
	}
	frames := make([][]byte, len(msg))
	for i, frame := range msg {
		if len(frame) > maxFrame {
			return nil, fmt.Errorf("zmq frame size %d exceeds limit %d", len(frame), maxFrame)
		}
		frames[i] = append([]byte(nil), frame...)
	}
	return frames, nil
}

func (s *Socket) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	raw := s.raw
	s.raw = nil
	s.mu.Unlock()
	if s.ctx != nil {
		s.ctx.mu.Lock()
		delete(s.ctx.sockets, s)
		s.ctx.mu.Unlock()
	}
	if raw == nil {
		return nil
	}
	if err := raw.Close(); err != nil {
		return mapError(err)
	}
	return nil
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, zmq.ErrClosed) || errors.Is(err, context.Canceled) {
		return ErrClosed
	}
	return err
}
