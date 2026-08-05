//go:build linux || darwin

package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/shenguard"
	"github.com/pyrex41/shenmux/internal/term"
	"github.com/pyrex41/shenmux/internal/zmqx"
)

func TestControlAttachAcquireAndInput(t *testing.T) {
	zctx, err := zmqx.NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer zctx.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stamp := time.Now().UnixNano()
	dataEndpoint := fmt.Sprintf("inproc://control-data-%d", stamp)
	controlEndpoint := fmt.Sprintf("inproc://control-router-%d", stamp)
	publisher, err := NewZMQPublisher(ctx, zctx, dataEndpoint, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	dim, _ := shenguard.NewDimensions(80, 24)
	pty := &fakePTY{dim: dim}
	runtime, err := NewRuntime(RuntimeConfig{
		Session: "test", Dimensions: dim, PTY: pty, Terminal: term.NewBasic(dim),
		Publisher: publisher, StoreLimits: protocol.DefaultStoreLimits(), ControlLease: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	control, err := NewControlServer(ctx, zctx, controlEndpoint, "test", runtime, publisher)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()

	sub, err := zctx.Socket(zmqx.Sub)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if err := sub.Connect(dataEndpoint); err != nil {
		t.Fatal(err)
	}
	if err := sub.SetBytes(zmqx.Subscribe, []byte("session/test")); err != nil {
		t.Fatal(err)
	}
	if err := sub.SetBytes(zmqx.Subscribe, []byte("ready/client-1")); err != nil {
		t.Fatal(err)
	}

	dealer, err := zctx.Socket(zmqx.Dealer)
	if err != nil {
		t.Fatal(err)
	}
	defer dealer.Close()
	if err := dealer.SetBytes(zmqx.Identity, []byte("client-1")); err != nil {
		t.Fatal(err)
	}
	if err := dealer.Connect(controlEndpoint); err != nil {
		t.Fatal(err)
	}

	attached := controlCall(t, dealer, protocol.Message{Kind: protocol.KindAttach, Meta: protocol.Meta{Version: protocol.Version, Session: "test", RequestID: 1}})
	if attached.Kind != protocol.KindAttached || attached.Meta.RequestID != 1 || attached.Meta.HasControl {
		t.Fatalf("unexpected attach reply: %+v", attached)
	}
	if _, err := protocol.DecodeArchive(attached.Payload); err != nil {
		t.Fatal(err)
	}

	acquired := controlCall(t, dealer, protocol.Message{Kind: protocol.KindAcquireControl, Meta: protocol.Meta{Version: protocol.Version, Session: "test", RequestID: 2}})
	if acquired.Kind != protocol.KindPong || !acquired.Meta.HasControl || acquired.Meta.ControlOwner != "client-1" {
		t.Fatalf("unexpected acquire reply: %+v", acquired)
	}

	input := controlCall(t, dealer, protocol.Message{Kind: protocol.KindInput, Meta: protocol.Meta{Version: protocol.Version, Session: "test", RequestID: 3}, Payload: []byte("abc")})
	if input.Kind != protocol.KindPong || input.Meta.RequestID != 3 {
		t.Fatalf("unexpected input reply: %+v", input)
	}
	eventually(t, func() bool { return pty.String() == "abc" })
}

func controlCall(t *testing.T, socket *zmqx.Socket, msg protocol.Message) protocol.Message {
	t.Helper()
	frames, err := protocol.Encode(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := socket.SendMultipart(frames, 0); err != nil {
		t.Fatal(err)
	}
	replyFrames, err := socket.RecvMultipartLimit(0, protocol.MaxPayloadSize, 3)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := protocol.Decode(replyFrames)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Kind == protocol.KindError {
		t.Fatalf("control error: %s", reply.Meta.Error)
	}
	return reply
}
