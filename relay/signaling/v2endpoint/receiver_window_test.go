package v2endpoint

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
)

func TestReceiverReservationsPreserveConfiguredAdmissionAndMemory(t *testing.T) {
	for _, count := range []int{64, 128, receiverCreditCapacity} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			destination := newEndpointTestConnection("destination", nil, func() {})
			destination.receiverCredits.windowFrames = min(receiverWindowFrames, receiverCreditCapacity/count)
			for index := range count {
				id := receiverCreditTestID(index + 1)
				source := receiverCreditTestSource(id)
				destination.addSession(id)
				if !destination.reserveReceiverCredit(id, source) {
					t.Fatal("reservation failed")
				}
				credit, _, ok := source.takeForwardCredit()
				if !ok || credit.Frames == 0 || credit.Bytes != v2.SenderWindowBytes {
					t.Fatalf("admitted session %d has no starter credit: %+v", index, credit)
				}
			}
			pool := &destination.receiverCredits
			if pool.reserved > receiverCreditCapacity ||
				(pool.reserved+1)*MaximumV2WebSocketMessageSize > MaximumForwardQueueBytes {
				t.Fatal("reservations and sole writer exceed memory bound", pool.reserved)
			}
		})
	}
}

func TestReceiverCreditCompletionAndRetirementAreExact(t *testing.T) {
	destination := newEndpointTestConnection("destination", nil, func() {})
	id := receiverCreditTestID(1)
	source := receiverCreditTestSource(id)
	destination.addSession(id)
	destination.reserveReceiverCredit(id, source)
	destination.reserveReceiverCredit(id, source)
	credit, _, _ := source.takeForwardCredit()
	if credit.Frames != receiverWindowFrames || credit.Bytes != v2.SenderWindowBytes {
		t.Fatal(credit)
	}
	frame := []byte("encoded route")
	if _, allowed := source.consumeForwardCredit(id, len(frame)); !allowed {
		t.Fatal("initial grant unusable")
	}
	destination.receiverCredits.complete(id, len(frame))
	refund, _, ok := source.takeForwardCredit()
	if !ok || refund.Frames != 1 || refund.Bytes != uint32(len(frame)) {
		t.Fatal(refund)
	}
	destination.removeSession(id)
	source.removeSession(id)
	if destination.receiverCredits.reserved != 0 {
		t.Fatal("retirement leaked reservation")
	}
	nextID := receiverCreditTestID(2)
	replacement := receiverCreditTestSource(nextID)
	destination.addSession(nextID)
	destination.reserveReceiverCredit(nextID, replacement)
	replacement.takeForwardCredit()
	destination.removeSession(id)
	destination.receiverCredits.complete(id, len(frame))
	if _, _, ok := replacement.takeForwardCredit(); ok {
		t.Fatal("old completion credited replacement")
	}
	if destination.receiverCredits.reserved != receiverWindowFrames {
		t.Fatal("old retirement changed replacement reservations")
	}
	source.closed.Store(true)
	if destination.reserveReceiverCredit(id, source) {
		t.Fatal("removed destination session was recreated")
	}
}

func TestReceiverPoolRotatesFreedSlotsAndSkipsClosedSources(t *testing.T) {
	destination := newEndpointTestConnection("destination", nil, func() {})
	destination.receiverCredits.windowFrames = 1
	var firstID v2.RelaySessionID
	var last *connection
	// Artificial saturation also exercises the round-robin fallback after a
	// session retires; production reserves join headroom from configured capacity.
	for index := range receiverCreditCapacity + 1 {
		id := receiverCreditTestID(index + 1)
		source := receiverCreditTestSource(id)
		if index == 0 {
			firstID = id
		}
		destination.addSession(id)
		destination.reserveReceiverCredit(id, source)
		source.takeForwardCredit()
		last = source
	}
	if _, _, ok := last.takeForwardCredit(); ok {
		t.Fatal("saturated pool granted excess credit")
	}
	destination.removeSession(firstID)
	if credit, _, ok := last.takeForwardCredit(); !ok || credit.Frames == 0 || credit.Bytes != 0 {
		t.Fatal("freed capacity did not reach waiting session", credit)
	}
	last.closed.Store(true)
	destination.receiverCredits.grantLockedForTest()
}

func (pool *receiverCreditPool) grantLockedForTest() {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.grantLocked()
}

func TestReceiverCreditRejectsUnreservedDataWithoutBlockingReader(t *testing.T) {
	source := receiverCreditTestSource(receiverCreditTestID(1))
	destination := newEndpointTestConnection("destination", nil, func() {})
	id := receiverCreditTestID(1)
	destination.addSession(id)
	server := &Server{}
	if err := server.forwardToDestination(source, destination, id, []byte("data")); !errors.Is(err, ErrProtocol) {
		t.Fatal("unreserved receiver data entered destination", err)
	}
	destination.reserveReceiverCredit(id, source)
	source.takeForwardCredit()
	if err := server.forwardToDestination(source, destination, id, []byte("data")); err != nil {
		t.Fatal(err)
	}
	destination.requestClose()
	if err := server.forwardToDestination(source, destination, id, []byte("data")); !errors.Is(err, ErrConnection) {
		t.Fatal(err)
	}
	if err := server.forwardToDestination(source, nil, id, []byte("data")); !errors.Is(err, ErrConnection) {
		t.Fatal(err)
	}
}

func TestReceiverProductivePressureNeverHidesConnectionProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, socket := newMemorySocketPair()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		source := newEndpointTestConnection("receiver", socket, cancel)
		source.completeHandshake()
		destination := newEndpointTestConnection("sender", nil, func() {})
		id := receiverCreditTestID(1)
		source.addSession(id)
		destination.addSession(id)
		destination.reserveReceiverCredit(id, source)
		source.takeForwardCredit()
		registry, err := v2route.New(t.Context(), v2route.Config{MaxRoutes: 1, MaxSessions: 1, MaxSessionsPerShare: 1, Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{}})
		if err != nil {
			t.Fatal(err)
		}
		server := &Server{registry: registry, writeTimeout: 15 * time.Second}
		writerDone := make(chan error, 1)
		go func() { writerDone <- server.writeLoop(ctx, source) }()
		// Fill every reserved destination slot. Further application sends stay at
		// the client credit gate, while a probe immediately follows this FIFO burst.
		for range receiverWindowFrames {
			if err := server.forwardToDestination(source, destination, id, []byte("frame")); err != nil {
				t.Fatal(err)
			}
		}
		for round := uint64(1); round <= 6; round++ {
			time.Sleep(10 * time.Second)
			destination.takeForward()
			destination.receiverCredits.complete(id, len("frame"))
			source.takeForwardCredit()
			if err := server.forwardToDestination(source, destination, id, []byte("frame")); err != nil {
				t.Fatal(err)
			}
			probe, _ := (v2.ConnectionProbe{Nonce: round}).MarshalBinary()
			started := time.Now()
			if err := server.forwardFrame(ctx, source, probe); err != nil {
				t.Fatal(err)
			}
			_, encoded, err := client.Read(ctx)
			for len(encoded) >= 4 && string(encoded[:4]) == v2.SessionCreditMagic {
				_, encoded, err = client.Read(ctx)
			}
			ack, parseErr := v2.ParseConnectionProbeAck(encoded)
			if err != nil || parseErr != nil || ack.Nonce != round || time.Since(started) != 0 {
				t.Fatalf("productive pressure delayed probe: ack=%+v read=%v parse=%v", ack, err, parseErr)
			}
		}
		cancel()
		<-writerDone
	})
}

func TestReceiverReservationConfigurationRejectsImpossibleConcurrency(t *testing.T) {
	registry, err := v2route.New(t.Context(), v2route.Config{
		MaxRoutes: 1, MaxSessions: receiverCreditCapacity + 1, MaxSessionsPerShare: receiverCreditCapacity + 1,
		Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{Capacity: 1, Random: &sequenceReader{next: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var identity v2.RelayIdentity
	identity[0] = 1
	if _, err := New(Config{Registry: registry, Challenges: ledger, RelayIdentity: identity}); !errors.Is(err, ErrConfig) {
		t.Fatal("configuration admitted more sessions than bounded starter capacity", err)
	}
}

func receiverCreditTestID(value int) v2.RelaySessionID {
	var id v2.RelaySessionID
	binary.BigEndian.PutUint64(id[:], uint64(value))
	return id
}

func receiverCreditTestSource(id v2.RelaySessionID) *connection {
	source := newEndpointTestConnection(v2route.ConnectionID(fmt.Sprintf("receiver-%x", id)), nil, func() {})
	source.addSession(id)
	return source
}
