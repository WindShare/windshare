package v2endpoint

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

func (*memorySocket) Ping(context.Context) error { return nil }

type silentHeartbeatConnection struct{ BinaryConnection }

func (*silentHeartbeatConnection) Ping(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }

func TestConnectionProbeBypassesSessionRouting(t *testing.T) {
	client, relay := newMemorySocketPair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ref, _ := v2route.NewConnectionRef("heartbeat")
	peer := newConnection(ref, relay, cancel)
	server := &Server{writeTimeout: time.Second}
	written := make(chan error, 1)
	go func() { written <- server.writeLoop(ctx, peer) }()
	probe, _ := (v2.ConnectionProbe{Nonce: 17}).MarshalBinary()
	if err := server.forwardFrame(ctx, peer, probe); err != nil {
		t.Fatal(err)
	}
	kind, encoded, err := client.Read(ctx)
	if err != nil || kind != websocket.MessageBinary {
		t.Fatal(err)
	}
	ack, err := v2.ParseConnectionProbeAck(encoded)
	if err != nil || ack.Nonce != 17 {
		t.Fatalf("ack=%+v error=%v", ack, err)
	}
	probe[5] = 1
	if err := server.forwardFrame(ctx, peer, probe); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	cancel()
	<-written
}

type delayedHeartbeatWrite struct {
	BinaryConnection
	delay time.Duration
}

func (socket *delayedHeartbeatWrite) Write(ctx context.Context, kind websocket.MessageType, data []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(socket.delay):
		return socket.BinaryConnection.Write(ctx, kind, data)
	}
}

func TestConnectionProbeWaitsForCurrentWriteAndOwnWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, relay := newMemorySocketPair()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ref, _ := v2route.NewConnectionRef("heartbeat")
		peer := newConnection(ref, &delayedHeartbeatWrite{BinaryConnection: relay, delay: 10 * time.Second}, cancel)
		server := &Server{writeTimeout: 15 * time.Second}
		writerDone := make(chan error, 1)
		go func() { writerDone <- server.writeLoop(ctx, peer) }()
		first := make(chan error, 1)
		go func() { first <- peer.sendControl(ctx, []byte("existing write")) }()
		synctest.Wait()
		time.Sleep(time.Second)
		probe, _ := (v2.ConnectionProbe{Nonce: 17}).MarshalBinary()
		if err := server.answerConnectionProbe(ctx, peer, probe); err != nil {
			t.Fatalf("healthy queued probe failed: %v", err)
		}
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		_, _, _ = client.Read(ctx)
		_, encoded, err := client.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ack, err := v2.ParseConnectionProbeAck(encoded); err != nil || ack.Nonce != 17 {
			t.Fatalf("ack=%+v error=%v", ack, err)
		}
		cancel()
		<-writerDone
	})
}

func TestSessionWriteYieldsToConnectionControl(t *testing.T) {
	client, relay := newMemorySocketPair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ref, _ := v2route.NewConnectionRef("heartbeat")
	peer := newConnection(ref, relay, cancel)
	server := &Server{writeTimeout: time.Second}
	ack, _ := (v2.ConnectionProbeAck{Nonce: 17}).MarshalBinary()
	control := controlWrite{data: ack, done: make(chan error, 1)}
	peer.control <- control
	if err := server.writeSessionData(ctx, peer, []byte("session frame")); err != nil {
		t.Fatal(err)
	}
	if err := <-control.done; err != nil {
		t.Fatal(err)
	}
	_, first, _ := client.Read(ctx)
	if parsed, err := v2.ParseConnectionProbeAck(first); err != nil || parsed.Nonce != 17 {
		t.Fatalf("session write overtook probe: %x", first)
	}
}

func TestEndpointHeartbeatCancelsOnlyItsConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, socket := newMemorySocketPair()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ref, _ := v2route.NewConnectionRef("heartbeat")
		peer := newConnection(ref, &silentHeartbeatConnection{socket}, cancel)
		var traces []HeartbeatTrace
		server := &Server{
			heartbeat:       HeartbeatConfig{Interval: time.Second, Timeout: 3 * time.Second},
			heartbeatTracer: HeartbeatTraceFunc(func(event HeartbeatTrace) { traces = append(traces, event) }),
		}
		done := make(chan error, 1)
		go func() { done <- server.heartbeatLoop(ctx, peer) }()
		err := <-done
		if !errors.Is(err, ErrHeartbeat) || !peer.closed.Load() || ctx.Err() == nil {
			t.Fatalf("heartbeat=%v", err)
		}
		if len(traces) != 2 || traces[1].Connection != ref || traces[1].Round != 1 {
			t.Fatalf("%+v", traces)
		}
	})
}
