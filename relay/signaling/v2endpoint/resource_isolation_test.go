package v2endpoint

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
	"github.com/windshare/windshare/transport/relayv2"
)

const isolationRelayBase = "https://relay.example/isolation"

func isolationServer(t *testing.T) (*Server, *relayv2.SenderConnection, endpointFixture) {
	t.Helper()
	endpoint, err := v2.NormalizeRelayEndpoint(isolationRelayBase)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := v2route.New(t.Context(), v2route.Config{
		MaxRoutes: 2, MaxSessions: 64, MaxSessionsPerShare: 4,
		Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{Capacity: 16, Random: &sequenceReader{next: 31}})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{Registry: registry, Challenges: ledger, RelayIdentity: endpoint.Identity})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newEndpointFixture(t)
	sender, err := relayv2.DialSender(t.Context(), relayv2.SenderConfig{
		RelayBaseURL: isolationRelayBase, Init: fixture.init, SenderPrivateKey: fixture.privateKey,
		Descriptor: fixture.descriptor, Dial: relayv2.DialOptions{SocketDialer: memoryServerDialer(server)},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close(); _ = server.Shutdown(context.Background()) })
	return server, sender, fixture
}

type gatedDestination struct {
	BinaryConnection
	gate <-chan struct{}
}

func (socket gatedDestination) Write(ctx context.Context, kind websocket.MessageType, frame []byte) error {
	if len(frame) >= 4 && string(frame[:4]) == "WS2O" {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-socket.gate:
		}
	}
	return socket.BinaryConnection.Write(ctx, kind, frame)
}

func TestSlowReceiverDoesNotBlockSiblingDataOrAdmission(t *testing.T) {
	for _, size := range []int{32, v2.MaxOpaqueCiphertextBytes} {
		t.Run(stringSize(size), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				server, sender, fixture := isolationServer(t)
				client, destination := newMemorySocketPair()
				defer client.Close(websocket.StatusNormalClosure, "")
				gate := make(chan struct{})
				var open sync.Once
				defer open.Do(func() { close(gate) })
				go func() { _ = server.Serve(context.Background(), gatedDestination{destination, gate}) }()
				join, _ := (v2.Join{ShareID: fixture.init.ShareID}).MarshalBinary()
				if err := client.Write(t.Context(), websocket.MessageBinary, join); err != nil {
					t.Fatal(err)
				}
				_, response, err := client.Read(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				delivery, err := v2.ParseDescriptorDelivery(response)
				if err != nil {
					t.Fatal(err)
				}
				hello, _ := (v2.OpaqueRoute{RelaySessionID: delivery.RelaySessionID, Ciphertext: []byte("hello")}).MarshalBinary()
				if err := client.Write(t.Context(), websocket.MessageBinary, hello); err != nil {
					t.Fatal(err)
				}
				slow, err := sender.Accept(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				assertFrame(t, slow.Recv(), "hello")
				if err := slow.ConfirmAdmission(t.Context()); err != nil {
					t.Fatal(err)
				}

				const total = v2.SenderWindowFrames*2 + 1
				done := make(chan error, 1)
				go func() {
					for index := range total {
						frame := bytes.Repeat([]byte{0x7a}, size)
						binary.BigEndian.PutUint32(frame, uint32(index))
						if err := slow.Send(t.Context(), frame); err != nil {
							done <- err
							return
						}
					}
					done <- nil
				}()
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("stalled destination did not exert session pressure: %v", err)
				default:
				}
				server.connections.mu.Lock()
				for _, peer := range server.connections.current {
					if peer.roleValue() == roleReceiver {
						peer.forwardMu.Lock()
						if peer.forwardFrames > MaximumSessionQueueFrames || peer.forwardBytes > MaximumSessionQueueBytes {
							t.Error("destination exceeded bounded storage")
						}
						peer.forwardMu.Unlock()
					}
				}
				server.connections.mu.Unlock()
				healthy := dialReceiver(t, isolationRelayBase, fixture.init.ShareID, memoryServerDialer(server))
				defer healthy.Close()
				fast := establishSession(t, sender, healthy, []byte("sibling hello"))
				assertFrame(t, fast.Recv(), "sibling hello")
				if err := fast.ConfirmAdmission(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := fast.Send(t.Context(), []byte("sibling signal and data")); err != nil {
					t.Fatal(err)
				}
				assertFrame(t, healthy.Channel().Recv(), "sibling signal and data")
				select {
				case <-sender.Done():
					t.Fatal("slow receiver closed sender")
				default:
				}

				open.Do(func() { close(gate) })
				for index := range total {
					_, encoded, err := client.Read(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					frame, err := v2.ParseOpaqueRoute(encoded)
					if err != nil || len(frame.Ciphertext) != size || binary.BigEndian.Uint32(frame.Ciphertext) != uint32(index) {
						t.Fatalf("resumed transfer lost order or content at %d: %v", index, err)
					}
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func stringSize(size int) string {
	if size == v2.MaxOpaqueCiphertextBytes {
		return "byte-budget"
	}
	return "frame-budget"
}

func TestProvisionalAdmissionExpiresButAuthenticatedIdleSessionSurvives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server, sender, fixture := isolationServer(t)
		dial := memoryServerDialer(server)
		silent := dialReceiver(t, isolationRelayBase, fixture.init.ShareID, dial)
		defer silent.Close()
		noise := dialReceiver(t, isolationRelayBase, fixture.init.ShareID, dial)
		defer noise.Close()
		unconfirmed := establishSession(t, sender, noise, []byte("untrusted bytes"))
		assertFrame(t, unconfirmed.Recv(), "untrusted bytes")
		active := dialReceiver(t, isolationRelayBase, fixture.init.ShareID, dial)
		defer active.Close()
		accepted := establishSession(t, sender, active, []byte("authenticated"))
		assertFrame(t, accepted.Recv(), "authenticated")
		if err := accepted.ConfirmAdmission(t.Context()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(v2route.SessionAdmissionTimeout - time.Nanosecond)
		// More traffic is not another admission lease.
		if err := noise.Channel().Send(t.Context(), []byte("still untrusted")); err != nil {
			t.Fatal(err)
		}
		assertFrame(t, unconfirmed.Recv(), "still untrusted")
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case <-silent.Done():
		default:
			t.Fatal("silent JOIN retained connection")
		}
		select {
		case <-noise.Done():
		default:
			t.Fatal("unconfirmed traffic renewed admission")
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		select {
		case <-active.Done():
			t.Fatal("idle authenticated session was evicted")
		default:
		}
		if err := accepted.Send(t.Context(), []byte("after idle")); err != nil {
			t.Fatal(err)
		}
		assertFrame(t, active.Channel().Recv(), "after idle")
		for range 3 {
			replacement := dialReceiver(t, isolationRelayBase, fixture.init.ShareID, dial)
			defer replacement.Close()
		}
	})
}

func TestConnectionFirstFrameAndRegistrationProofDeadlines(t *testing.T) {
	for _, registration := range []bool{false, true} {
		t.Run(map[bool]string{false: "first frame", true: "proof"}[registration], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				server, _, _ := isolationServer(t)
				client, relay := newMemorySocketPair()
				defer client.Close(websocket.StatusNormalClosure, "")
				done := make(chan error, 1)
				go func() { done <- server.Serve(context.Background(), relay) }()
				budget := connectionAdmissionTimeout
				if registration {
					fixture := newEndpointFixture(t)
					fixture.init.ShareInstance[0] ^= 1
					// Resume's credential read is also covered by the registration budget.
					fixture.init.Mode = v2.RegistrationResume
					frame, _ := fixture.init.MarshalBinary()
					if err := client.Write(t.Context(), websocket.MessageBinary, frame); err != nil {
						t.Fatal(err)
					}
					budget = v2route.JoinStartingGrace
				}
				synctest.Wait()
				time.Sleep(budget)
				synctest.Wait()
				select {
				case err := <-done:
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatal(err)
					}
				default:
					t.Fatal("unfinished connection admission retained resources")
				}
			})
		})
	}
}
