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

func TestConnectionProbeBypassesSessionRouting(t *testing.T) {
	client, relay := newMemorySocketPair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ref, _ := v2route.NewConnectionRef("liveness")
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
	// One pending acknowledgement may stand for earlier probes only if nonces
	// never repeat or move backwards on a connection.
	if err := server.forwardFrame(ctx, peer, probe); !errors.Is(err, ErrProtocol) {
		t.Fatal("repeated probe nonce accepted", err)
	}
	probe[5] = 1
	if err := server.forwardFrame(ctx, peer, probe); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	cancel()
	<-written
}

type delayedWrite struct {
	BinaryConnection
	delay time.Duration
}

func (socket *delayedWrite) Write(ctx context.Context, kind websocket.MessageType, data []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(socket.delay):
		return socket.BinaryConnection.Write(ctx, kind, data)
	}
}

func TestProbeAcknowledgementsCoalesceWithoutBlockingReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, relay := newMemorySocketPair()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ref, _ := v2route.NewConnectionRef("liveness")
		peer := newConnection(ref, &delayedWrite{BinaryConnection: relay, delay: 10 * time.Second}, cancel)
		server := &Server{writeTimeout: 15 * time.Second}
		writerDone := make(chan error, 1)
		go func() { writerDone <- server.writeLoop(ctx, peer) }()
		first := make(chan error, 1)
		go func() { first <- peer.sendControl(ctx, []byte("existing write")) }()
		synctest.Wait()
		for nonce := uint64(1); nonce <= 3; nonce++ {
			probe, _ := (v2.ConnectionProbe{Nonce: nonce}).MarshalBinary()
			if err := server.answerConnectionProbe(peer, probe); err != nil {
				t.Fatal(err)
			}
		}
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if _, existing, _ := client.Read(ctx); string(existing) != "existing write" {
			t.Fatalf("first write = %q", existing)
		}
		_, encoded, err := client.Read(ctx)
		if ack, parseErr := v2.ParseConnectionProbeAck(encoded); err != nil || parseErr != nil || ack.Nonce != 3 {
			t.Fatalf("ack=%+v error=%v parse=%v", ack, err, parseErr)
		}
		synctest.Wait()
		if pending := peer.probeAck.Load(); pending != 0 {
			t.Fatalf("coalesced probes left acknowledgement %d pending", pending)
		}
		cancel()
		<-writerDone
	})
}

func TestSessionWriteYieldsToConnectionControl(t *testing.T) {
	client, relay := newMemorySocketPair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ref, _ := v2route.NewConnectionRef("liveness")
	peer := newConnection(ref, relay, cancel)
	server := &Server{writeTimeout: time.Second}
	peer.probeAck.Store(17)
	if err := server.writeSessionData(ctx, peer, []byte("session frame")); err != nil {
		t.Fatal(err)
	}
	_, first, _ := client.Read(ctx)
	if parsed, err := v2.ParseConnectionProbeAck(first); err != nil || parsed.Nonce != 17 {
		t.Fatalf("session write overtook probe acknowledgement: %x", first)
	}
}

func TestInboundFramesKeepConnectionAliveUntilSilence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, relay := newMemorySocketPair()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ref, _ := v2route.NewConnectionRef("liveness")
		peer := newConnection(ref, relay, cancel)
		var traces []LivenessTrace
		server := &Server{
			writeTimeout: time.Second, livenessTimeout: 3 * time.Second,
			livenessTracer: LivenessTraceFunc(func(event LivenessTrace) { traces = append(traces, event) }),
		}
		go func() { _ = server.forwardLoop(ctx, peer) }()
		done := make(chan error, 1)
		go func() { done <- server.livenessLoop(ctx, peer) }()
		for nonce := uint64(1); nonce <= 5; nonce++ {
			time.Sleep(2 * time.Second)
			probe, _ := (v2.ConnectionProbe{Nonce: nonce}).MarshalBinary()
			if err := client.Write(ctx, websocket.MessageBinary, probe); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		if peer.closed.Load() {
			t.Fatal("probing client closed as silent")
		}
		err := <-done
		if !errors.Is(err, ErrLiveness) || !peer.closed.Load() || ctx.Err() == nil {
			t.Fatalf("liveness=%v", err)
		}
		if len(traces) != 1 || traces[0].Connection != ref ||
			traces[0].Silence != 3*time.Second || traces[0].Timeout != 3*time.Second {
			t.Fatalf("%+v", traces)
		}
	})
}

func TestLivenessTimeoutMustExceedClientProbeInterval(t *testing.T) {
	registry, err := v2route.New(t.Context(), v2route.Config{
		MaxRoutes: 1, MaxSessions: 1, MaxSessionsPerShare: 1,
		Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	challenges, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{Capacity: 1, Random: &sequenceReader{next: 1}})
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Registry: registry, Challenges: challenges, RelayIdentity: v2.RelayIdentity{1}}
	if _, err := New(config); err != nil {
		t.Fatal("default liveness rejected", err)
	}
	config.LivenessTimeout = v2.ConnectionProbeInterval
	if _, err := New(config); !errors.Is(err, ErrConfig) {
		t.Fatal("relay accepted a timeout that drops idle probing clients", err)
	}
}
