package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pyrex41/shenmux/internal/naming"
	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/screen"
	"github.com/pyrex41/shenmux/internal/shenguard"
	"github.com/pyrex41/shenmux/internal/zmqx"
)

const defaultHeartbeatInterval = 2 * time.Second

type Config struct {
	Session           string
	ClientID          string
	ControlEndpoint   string
	DataEndpoint      string
	EventBuffer       int
	HeartbeatInterval time.Duration
}

type Snapshot struct {
	Meta    protocol.Meta
	Archive protocol.Archive
	Current protocol.Checkpoint
}

// View is a client-side projection of the authoritative interpreted state.
// Applying publications never parses terminal bytes; it only applies typed
// screen deltas and control/exit metadata.
type View struct {
	Checkpoint   protocol.Checkpoint
	HistoryLimit int
}

func (s Snapshot) View() View {
	return View{Checkpoint: s.Current.Clone(), HistoryLimit: s.Archive.HistoryLimit}
}

func (v *View) Apply(msg protocol.Message) (*screen.Delta, error) {
	if v == nil {
		return nil, errors.New("nil client view")
	}
	if msg.Meta.Seq != v.Checkpoint.Seq+1 {
		return nil, fmt.Errorf("state sequence gap: got %d want %d", msg.Meta.Seq, v.Checkpoint.Seq+1)
	}
	var rendered *screen.Delta
	switch msg.Kind {
	case protocol.KindDelta:
		delta, err := protocol.DecodeDelta(msg.Payload)
		if err != nil {
			return nil, err
		}
		state, err := screen.Apply(v.Checkpoint.Screen, delta, v.HistoryLimit)
		if err != nil {
			return nil, err
		}
		v.Checkpoint.Screen = state
		rendered = &delta
	case protocol.KindControl:
		v.Checkpoint.ControlOwner = msg.Meta.ControlOwner
	case protocol.KindExit:
		v.Checkpoint.Exited = true
		v.Checkpoint.ExitCode = msg.Meta.ExitCode
	case protocol.KindPong:
		return nil, errors.New("control acknowledgement appeared on publication stream")
	default:
		return nil, fmt.Errorf("message kind %q is not a state publication", msg.Kind)
	}
	v.Checkpoint.Seq = msg.Meta.Seq
	return rendered, nil
}

func (v View) HasControl(clientID string) bool { return v.Checkpoint.ControlOwner == clientID }

func (s Snapshot) Render(w io.Writer, renderer *screen.Renderer) error {
	if renderer == nil {
		return errors.New("nil screen renderer")
	}
	return renderer.RenderFull(w, s.Current.Screen)
}

type Client struct {
	session string
	cid     shenguard.ClientID
	zctx    *zmqx.Context
	ctx     context.Context
	cancel  context.CancelFunc
	control *controlActor
	events  chan protocol.Message
	errors  chan error
	subDone chan struct{}
	hbDone  chan struct{}

	attached  atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func New(parent context.Context, cfg Config) (*Client, error) {
	if err := naming.ValidateSession(cfg.Session); err != nil {
		return nil, err
	}
	if cfg.ControlEndpoint == "" || cfg.DataEndpoint == "" {
		return nil, errors.New("client endpoints must not be empty")
	}
	cid, err := shenguard.NewClientID(cfg.ClientID)
	if err != nil {
		return nil, err
	}
	if cfg.EventBuffer <= 0 {
		cfg.EventBuffer = 1024
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = defaultHeartbeatInterval
	}
	zctx, err := zmqx.NewContext()
	if err != nil {
		return nil, err
	}
	cleanup := func(err error) (*Client, error) { _ = zctx.Close(); return nil, err }

	sub, err := zctx.Socket(zmqx.Sub)
	if err != nil {
		return cleanup(err)
	}
	for option, value := range map[int]int{zmqx.Linger: 0, zmqx.RcvHWM: 10_000} {
		if err := sub.SetInt(option, value); err != nil {
			_ = sub.Close()
			return cleanup(err)
		}
	}
	if err := sub.SetInt64(zmqx.MaxMsgSize, protocol.MaxPayloadSize); err != nil {
		_ = sub.Close()
		return cleanup(err)
	}
	if err := sub.SetBytes(zmqx.Subscribe, []byte("session/"+cfg.Session)); err != nil {
		_ = sub.Close()
		return cleanup(err)
	}
	if err := sub.SetBytes(zmqx.Subscribe, []byte(readyTopicPrefix+cid.String())); err != nil {
		_ = sub.Close()
		return cleanup(err)
	}
	if err := sub.Connect(cfg.DataEndpoint); err != nil {
		_ = sub.Close()
		return cleanup(err)
	}

	dealer, err := zctx.Socket(zmqx.Dealer)
	if err != nil {
		_ = sub.Close()
		return cleanup(err)
	}
	for option, value := range map[int]int{zmqx.Linger: 0, zmqx.SndHWM: 10_000, zmqx.RcvHWM: 10_000} {
		if err := dealer.SetInt(option, value); err != nil {
			_ = dealer.Close()
			_ = sub.Close()
			return cleanup(err)
		}
	}
	if err := dealer.SetInt64(zmqx.MaxMsgSize, protocol.MaxPayloadSize); err != nil {
		_ = dealer.Close()
		_ = sub.Close()
		return cleanup(err)
	}
	if err := dealer.SetBytes(zmqx.Identity, []byte(cid.String())); err != nil {
		_ = dealer.Close()
		_ = sub.Close()
		return cleanup(err)
	}
	if err := dealer.Connect(cfg.ControlEndpoint); err != nil {
		_ = dealer.Close()
		_ = sub.Close()
		return cleanup(err)
	}

	ctx, cancel := context.WithCancel(parent)
	c := &Client{
		session: cfg.Session, cid: cid, zctx: zctx, ctx: ctx, cancel: cancel,
		events: make(chan protocol.Message, cfg.EventBuffer),
		errors: make(chan error, 16), subDone: make(chan struct{}), hbDone: make(chan struct{}),
	}
	c.control = newControlActor(ctx, dealer, c.errors)
	go c.readPublications(ctx, sub)
	go c.heartbeat(ctx, cfg.HeartbeatInterval)
	return c, nil
}

const readyTopicPrefix = "ready/"

func (c *Client) Attach(ctx context.Context) (Snapshot, error) {
	snapshot, err := c.snapshotCall(ctx, protocol.KindAttach)
	if err == nil {
		c.attached.Store(true)
	}
	return snapshot, err
}

func (c *Client) Resync(ctx context.Context) (Snapshot, error) {
	return c.snapshotCall(ctx, protocol.KindResync)
}

func (c *Client) snapshotCall(ctx context.Context, kind protocol.Kind) (Snapshot, error) {
	reply, err := c.control.Call(ctx, c.message(kind, nil))
	if err != nil {
		return Snapshot{}, err
	}
	if reply.Kind == protocol.KindError {
		return Snapshot{}, errors.New(reply.Meta.Error)
	}
	if reply.Kind != protocol.KindAttached {
		return Snapshot{}, fmt.Errorf("unexpected attach response %q", reply.Kind)
	}
	archive, err := protocol.DecodeArchive(reply.Payload)
	if err != nil {
		return Snapshot{}, err
	}
	current, err := archive.Current()
	if err != nil {
		return Snapshot{}, err
	}
	if current.Seq != reply.Meta.Seq {
		return Snapshot{}, fmt.Errorf("snapshot sequence mismatch: archive=%d envelope=%d", current.Seq, reply.Meta.Seq)
	}
	if archive.Checkpoint.Seq != reply.Meta.CheckpointSeq {
		return Snapshot{}, fmt.Errorf("checkpoint sequence mismatch: archive=%d envelope=%d", archive.Checkpoint.Seq, reply.Meta.CheckpointSeq)
	}
	if current.ControlOwner != reply.Meta.ControlOwner {
		return Snapshot{}, fmt.Errorf("control owner mismatch: archive=%q envelope=%q", current.ControlOwner, reply.Meta.ControlOwner)
	}
	return Snapshot{Meta: reply.Meta, Archive: archive, Current: current}, nil
}

func (c *Client) AcquireControl(ctx context.Context) (protocol.Meta, error) {
	return c.callAck(ctx, protocol.KindAcquireControl, protocol.Meta{}, nil)
}

func (c *Client) ReleaseControl(ctx context.Context) (protocol.Meta, error) {
	return c.callAck(ctx, protocol.KindReleaseControl, protocol.Meta{}, nil)
}

func (c *Client) Input(ctx context.Context, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	if len(payload) > 1<<20 {
		return fmt.Errorf("input exceeds %d bytes", 1<<20)
	}
	_, err := c.callAck(ctx, protocol.KindInput, protocol.Meta{}, append([]byte(nil), payload...))
	return err
}

func (c *Client) Resize(ctx context.Context, cols, rows int) error {
	dim, err := shenguard.NewDimensions(cols, rows)
	if err != nil {
		return err
	}
	_, err = c.callAck(ctx, protocol.KindResize, protocol.Meta{Cols: dim.Cols(), Rows: dim.Rows()}, nil)
	return err
}

func (c *Client) Ping(ctx context.Context) (protocol.Meta, error) {
	return c.callAck(ctx, protocol.KindPing, protocol.Meta{}, nil)
}

func (c *Client) Detach(ctx context.Context) error {
	_, err := c.callAck(ctx, protocol.KindDetach, protocol.Meta{}, nil)
	if err == nil {
		c.attached.Store(false)
	}
	return err
}

func (c *Client) callAck(ctx context.Context, kind protocol.Kind, meta protocol.Meta, payload []byte) (protocol.Meta, error) {
	msg := c.message(kind, payload)
	msg.Meta.Cols = meta.Cols
	msg.Meta.Rows = meta.Rows
	reply, err := c.control.Call(ctx, msg)
	if err != nil {
		return protocol.Meta{}, err
	}
	if reply.Kind == protocol.KindError {
		return protocol.Meta{}, errors.New(reply.Meta.Error)
	}
	if reply.Kind != protocol.KindPong {
		return protocol.Meta{}, fmt.Errorf("unexpected %s response %q", kind, reply.Kind)
	}
	return reply.Meta, nil
}

func (c *Client) message(kind protocol.Kind, payload []byte) protocol.Message {
	return protocol.Message{
		Kind:    kind,
		Meta:    protocol.Meta{Version: protocol.Version, Session: c.session, ClientID: c.cid.String()},
		Payload: payload,
	}
}

func (c *Client) Events() <-chan protocol.Message { return c.events }
func (c *Client) Errors() <-chan error            { return c.errors }
func (c *Client) ID() string                      { return c.cid.String() }

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		if c.attached.Load() {
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			_ = c.Detach(ctx)
			cancel()
		}
		c.cancel()
		c.control.Wait()
		<-c.subDone
		<-c.hbDone
		c.closeErr = c.zctx.Close()
		close(c.errors)
	})
	return c.closeErr
}

func (c *Client) heartbeat(ctx context.Context, interval time.Duration) {
	defer close(c.hbDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !c.attached.Load() {
				continue
			}
			pingCtx, cancel := context.WithTimeout(ctx, interval)
			_, err := c.Ping(pingCtx)
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				c.report(fmt.Errorf("heartbeat: %w", err))
			}
		}
	}
}

func (c *Client) readPublications(ctx context.Context, socket *zmqx.Socket) {
	defer close(c.subDone)
	defer close(c.events)
	defer socket.Close()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	wantTopic := "session/" + c.session
	for {
		for {
			frames, err := socket.RecvMultipartLimit(zmqx.DontWait, protocol.MaxPayloadSize, 4)
			if errors.Is(err, zmqx.ErrWouldBlock) {
				break
			}
			if err != nil {
				c.report(fmt.Errorf("receive publication: %w", err))
				return
			}
			topic, msg, err := protocol.DecodePublished(frames)
			if err != nil {
				c.report(err)
				continue
			}
			if topic != wantTopic || msg.Meta.Session != c.session {
				c.report(fmt.Errorf("publication session mismatch: topic=%q session=%q", topic, msg.Meta.Session))
				continue
			}
			select {
			case c.events <- msg:
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Client) report(err error) {
	if err == nil {
		return
	}
	select {
	case c.errors <- err:
	default:
	}
}
