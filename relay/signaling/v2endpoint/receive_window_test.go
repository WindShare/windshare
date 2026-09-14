package v2endpoint

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

func TestReceiveCreditFollowsApplicationConsumptionAcrossSustainedBursts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), isolationScenarioTimeout)
		defer cancel()
		server, sender, fixture := isolationServer(t)
		receiver, err := dialIsolationReceiver(ctx, server, fixture.init.ShareID)
		if err != nil {
			t.Fatal(err)
		}
		defer receiver.Close()
		channel, err := establishIsolationSession(ctx, sender, receiver, []byte("hello"))
		if err != nil {
			t.Fatal(err)
		}
		assertFrame(t, channel.Recv(), "hello")
		if err := channel.ConfirmAdmission(ctx); err != nil {
			t.Fatal(err)
		}
		const total = v2.ReceiveWindowFrames * 3
		var sent atomic.Int32
		done := make(chan error, 1)
		go func() {
			for index := range total {
				payload := binary.BigEndian.AppendUint32(nil, uint32(index))
				if err := channel.Send(ctx, payload); err != nil {
					done <- err
					return
				}
				sent.Add(1)
			}
			done <- nil
		}()
		synctest.Wait()
		// Socket writes keep succeeding while the receiving application takes
		// nothing. Only its explicit delivery window can stop this producer.
		if got := int(sent.Load()); got != v2.ReceiveWindowFrames+v2.SenderWindowFrames {
			t.Fatalf("stalled receiver admitted %d frames", got)
		}
		if receiver.Channel().State() != framechannel.Open {
			t.Fatal("ordinary receiver pressure retired the channel")
		}
		sibling, err := dialIsolationReceiver(ctx, server, fixture.init.ShareID)
		if err != nil {
			t.Fatal(err)
		}
		defer sibling.Close()
		fast, err := establishIsolationSession(ctx, sender, sibling, []byte("sibling"))
		if err != nil {
			t.Fatal(err)
		}
		assertFrame(t, fast.Recv(), "sibling")
		if err := fast.Send(ctx, []byte("unblocked")); err != nil {
			t.Fatal(err)
		}
		assertFrame(t, sibling.Channel().Recv(), "unblocked")
		for index := range total {
			select {
			case payload, open := <-receiver.Channel().Recv():
				if !open || len(payload) != 4 || binary.BigEndian.Uint32(payload) != uint32(index) {
					t.Fatalf("frame %d: %x open=%v", index, payload, open)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if receiver.Channel().State() != framechannel.Open {
			t.Fatal("sustained receive lost its session")
		}
	})
}

func TestReceiveWindowRequiresBothBudgetsAndExplicitReplenishment(t *testing.T) {
	server, sender, receiver, id := receiveWindowFixture(t)
	wire, _ := (v2.OpaqueRoute{RelaySessionID: id, Ciphertext: []byte("response")}).MarshalBinary()
	for range 2 {
		if err := server.forwardFrame(t.Context(), sender, wire); err != nil {
			t.Fatal(err)
		}
	}
	assertBlocked := func() {
		t.Helper()
		if frame, ok := receiver.takeForward(); ok {
			t.Fatalf("delivered without complete receiver credit: %x", frame)
		}
	}
	grant := func(frames, size uint32) {
		t.Helper()
		encoded, _ := (v2.ReceiveCredit{RelaySessionID: id, Frames: frames, Bytes: size}).MarshalBinary()
		if err := server.forwardFrame(t.Context(), receiver, encoded); err != nil {
			t.Fatal(err)
		}
	}
	assertBlocked()
	grant(1, 0)
	assertBlocked()
	grant(0, uint32(len(wire)-1))
	assertBlocked()
	grant(0, 1)
	got, ok := receiver.takeForward()
	if !ok || !bytes.Equal(got, wire) {
		t.Fatal("credited frame was not forwarded")
	}
	server.completeForward(receiver, got)
	assertBlocked()
	grant(1, uint32(len(wire)))
	if got, ok := receiver.takeForward(); !ok || !bytes.Equal(got, wire) {
		t.Fatal("consumed capacity did not resume delivery")
	}
}

func TestReceiveWindowRejectsForeignAndExcessCreditWithoutRevivingRetirement(t *testing.T) {
	server, sender, receiver, id := receiveWindowFixture(t)
	full, _ := (v2.ReceiveCredit{RelaySessionID: id, Frames: v2.ReceiveWindowFrames, Bytes: v2.ReceiveWindowBytes}).MarshalBinary()
	if err := server.forwardFrame(t.Context(), sender, full); !errors.Is(err, ErrProtocol) {
		t.Fatal("sender granted receiver-owned storage", err)
	}
	if err := server.forwardFrame(t.Context(), receiver, []byte(v2.ReceiveCreditMagic)); !errors.Is(err, ErrProtocol) {
		t.Fatal("malformed grant accepted", err)
	}
	other := id
	other[0] ^= 0xff
	foreign, _ := (v2.ReceiveCredit{RelaySessionID: other, Frames: 1}).MarshalBinary()
	if err := server.forwardFrame(t.Context(), receiver, foreign); !errors.Is(err, ErrProtocol) {
		t.Fatal("unknown session granted", err)
	}
	if err := server.forwardFrame(t.Context(), receiver, full); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []v2.ReceiveCredit{
		{RelaySessionID: id, Frames: 1}, {RelaySessionID: id, Bytes: 1},
	} {
		encoded, _ := delta.MarshalBinary()
		if err := server.forwardFrame(t.Context(), receiver, encoded); !errors.Is(err, ErrProtocol) {
			t.Fatal("window overgrant accepted", err)
		}
	}
	if _, ended := server.endSession(id, receiver.ref); !ended {
		t.Fatal("session was not retired")
	}
	if err := server.forwardFrame(t.Context(), receiver, full); err != nil {
		t.Fatal("late retired credit was not ignored", err)
	}
	if len(receiver.receiveWindows) != 0 {
		t.Fatal("late credit recreated receiver storage")
	}
}

func TestReceiveWindowDoesNotDelayConnectionControls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		server, sender, receiver, id := receiveWindowFixture(t)
		client, socket := newMemorySocketPair()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		receiver.socket = socket
		receiver.completeHandshake()
		wire, _ := (v2.OpaqueRoute{RelaySessionID: id, Ciphertext: []byte("waiting")}).MarshalBinary()
		if err := server.forwardFrame(ctx, sender, wire); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- server.writeLoop(ctx, receiver) }()
		probe, _ := (v2.ConnectionProbe{Nonce: 1}).MarshalBinary()
		if err := server.forwardFrame(ctx, receiver, probe); err != nil {
			t.Fatal(err)
		}
		_, encoded, err := client.Read(ctx)
		ack, parseErr := v2.ParseConnectionProbeAck(encoded)
		if err != nil || parseErr != nil || ack.Nonce != 1 {
			t.Fatal("receive pressure hid connection probe", err, parseErr)
		}
		synctest.Wait()
		if receiver.forwardFrames != 1 {
			t.Fatal("heartbeat bypass released uncredited data")
		}
		cancel()
		<-done
	})
}

func receiveWindowFixture(t *testing.T) (*Server, *connection, *connection, v2.RelaySessionID) {
	t.Helper()
	registry, err := v2route.New(t.Context(), v2route.Config{
		MaxRoutes: 1, MaxSessions: 2, MaxSessionsPerShare: 2,
		Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newEndpointFixture(t)
	sender := newEndpointTestConnection("delivery-sender", nil, func() {})
	receiver := newEndpointTestConnection("delivery-receiver", nil, func() {})
	if err := registry.BeginRegistration(fixture.init, sender.ref); err != nil {
		t.Fatal(err)
	}
	if err := registry.Publish(fixture.init.ShareID, sender.ref, verifiedEndpointDescriptor(t, fixture)); err != nil {
		t.Fatal(err)
	}
	joined, err := registry.Join(fixture.init.ShareID, receiver.ref)
	if err != nil || joined.Status != v2route.JoinReady {
		t.Fatal("join", err)
	}
	id := joined.RelaySessionID
	sender.setRole(roleSender, fixture.init.ShareID)
	receiver.setRole(roleReceiver, fixture.init.ShareID)
	sender.addSession(id)
	receiver.addSession(id)
	return endpointTestServer(t, registry, sender, receiver), sender, receiver, id
}
