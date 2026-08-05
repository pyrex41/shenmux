//go:build linux || darwin

package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pyrex41/shenmux/internal/protocol"
	"github.com/pyrex41/shenmux/internal/shenguard"
	"github.com/pyrex41/shenmux/internal/zmqx"
)

func TestPublisherReadyHandshakeAndBroadcast(t *testing.T) {
	zctx, err := zmqx.NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer zctx.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpoint := fmt.Sprintf("inproc://publisher-%d", time.Now().UnixNano())
	publisher, err := NewZMQPublisher(ctx, zctx, endpoint, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()

	sub, err := zctx.Socket(zmqx.Sub)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if err := sub.SetInt(zmqx.Linger, 0); err != nil {
		t.Fatal(err)
	}
	if err := sub.Connect(endpoint); err != nil {
		t.Fatal(err)
	}
	cid, _ := shenguard.NewClientID("client-1")
	if err := sub.SetBytes(zmqx.Subscribe, []byte("session/test")); err != nil {
		t.Fatal(err)
	}
	if err := sub.SetBytes(zmqx.Subscribe, []byte(readyTopicPrefix+cid.String())); err != nil {
		t.Fatal(err)
	}

	readyCtx, readyCancel := context.WithTimeout(context.Background(), time.Second)
	defer readyCancel()
	if err := publisher.WaitReady(readyCtx, cid); err != nil {
		t.Fatal(err)
	}
	msg := protocol.Message{Kind: protocol.KindControl, Meta: protocol.Meta{Version: protocol.Version, Session: "test", Seq: 1, ControlOwner: "client-1"}}
	if err := publisher.Publish(msg); err != nil {
		t.Fatal(err)
	}
	frames, err := sub.RecvMultipart(0)
	if err != nil {
		t.Fatal(err)
	}
	topic, got, err := protocol.DecodePublished(frames)
	if err != nil {
		t.Fatal(err)
	}
	if topic != "session/test" || got.Meta.Seq != 1 || got.Meta.ControlOwner != "client-1" {
		t.Fatalf("unexpected publication: topic=%q msg=%+v", topic, got)
	}
}

func TestPublisherCloseUnblocksReadyWaiter(t *testing.T) {
	zctx, err := zmqx.NewContext()
	if err != nil {
		t.Fatal(err)
	}
	defer zctx.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	endpoint := fmt.Sprintf("inproc://publisher-close-%d", time.Now().UnixNano())
	publisher, err := NewZMQPublisher(ctx, zctx, endpoint, "test")
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := shenguard.NewClientID("never-ready")
	result := make(chan error, 1)
	go func() { result <- publisher.WaitReady(context.Background(), cid) }()
	if err := publisher.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("ready waiter succeeded after publisher close")
		}
	case <-time.After(time.Second):
		t.Fatal("ready waiter did not unblock")
	}
}
