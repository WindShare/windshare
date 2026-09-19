package nativepeer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/socketauthority"
)

func awaitSocketWait(t *testing.T, n *NativePeerConnectivity, session byte) AdmissionFacts {
	t.Helper()
	for {
		facts := admissionEvent(t, n, AdmissionQueued, session)
		if facts.SocketCapacity != nil {
			return facts
		}
	}
}

func TestSocketAdmissionWaitsWithoutSpendingAttemptsAndWakesOnPhysicalRelease(t *testing.T) {
	gate, _, makeNative := newAdmissionFixture(t, 0)
	n, independent := makeNative(), makeNative()
	n.config.Sockets = socketauthority.New(socketauthority.Config{Capacity: 1})
	first := startActual(t, n, 1)
	defer first.Close()
	n.SetDirect(attemptFor(1).ProtocolSessionID, attemptFor(1).Binding.PeerPathID)
	pending := queuedActual(t, n, 2, t.Context())
	facts := awaitSocketWait(t, n, 2)
	if facts.SocketCapacity.Used != 1 || facts.SocketCapacity.Requested != 1 || facts.Active != 0 || facts.StartsRemaining != ProcessStartsPerWindow-1 {
		t.Fatal("socket waiting spent a process attempt", facts)
	}
	facts.SocketCapacity.Limit = 99
	gate.mu.Lock()
	capacity := gate.queue[0].capacity.Limit
	gate.mu.Unlock()
	if capacity != 1 {
		t.Fatal("observer changed admission state", capacity)
	}
	// A full sender must not stall a different native owner's usable sockets.
	other := startActual(t, independent, 3)
	defer other.Close()
	select {
	case <-pending:
		t.Fatal("full socket pool started a provider")
	default:
	}
	n.CloseSession(attemptFor(1).ProtocolSessionID)
	second := actualResult(t, pending)
	if second.err != nil {
		t.Fatal(second.err)
	}
	defer second.peer.Close()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.starts != ProcessStartsPerWindow-3 {
		t.Fatal("wait consumed starts", gate.starts)
	}
}

func TestSocketWaitCancellationDeadlineAndRetirementDoNotLeakReservations(t *testing.T) {
	for _, finish := range []string{"cancel", "deadline", "close", "network"} {
		t.Run(finish, func(t *testing.T) {
			gate, _, makeNative := newAdmissionFixture(t, 0)
			n := makeNative()
			n.config.Sockets = socketauthority.New(socketauthority.Config{Capacity: 1})
			first := startActual(t, n, 1)
			defer first.Close()
			n.SetDirect(attemptFor(1).ProtocolSessionID, attemptFor(1).Binding.PeerPathID)
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(context.Canceled)
			pending := queuedActual(t, n, 2, ctx)
			awaitSocketWait(t, n, 2)
			switch finish {
			case "cancel":
				cancel(context.Canceled)
			case "deadline":
				cancel(context.DeadlineExceeded)
			case "close":
				n.CloseSession(attemptFor(2).ProtocolSessionID)
			case "network":
				n.config.Monitor.(*testMonitor).state.ResumeSequence++
				n.Maintain(t.Context())
			}
			result := actualResult(t, pending)
			if result.err == nil {
				t.Fatal("canceled wait allocated a provider")
			}
			if finish == "deadline" && (!errors.Is(result.err, socketauthority.ErrCapacity) || !errors.Is(result.err, context.DeadlineExceeded)) {
				t.Fatal(result.err)
			}
			gate.mu.Lock()
			defer gate.mu.Unlock()
			if len(gate.queue) != 0 || gate.starts != ProcessStartsPerWindow-1 {
				t.Fatal("canceled wait charged a start")
			}
		})
	}
}

func TestAbandonedSocketPreparationReturnsReservation(t *testing.T) {
	_, clock, makeNative := newAdmissionFixture(t, 0)
	n := makeNative()
	n.config.Sockets = socketauthority.New(socketauthority.Config{Capacity: 1})
	prepared, err := n.PrepareAttempt(t.Context(), attemptFor(1))
	if err != nil {
		t.Fatal(err)
	}
	pending := queuedActual(t, n, 2, t.Context())
	if facts := awaitSocketWait(t, n, 2); facts.SocketCapacity.Reserved != 1 || facts.SocketCapacity.Used != 0 {
		t.Fatal(facts)
	}
	clock.advance(time.Second)
	prepared.Close()
	result := actualResult(t, pending)
	if result.err != nil {
		t.Fatal(result.err)
	}
	_ = result.peer.Close()
}

func TestNetworkReplacementReleasesOldPathDemandBeforeReservingNewSockets(t *testing.T) {
	_, _, makeNative := newAdmissionFixture(t, 0)
	n := makeNative()
	n.config.Sockets = socketauthority.New(socketauthority.Config{Capacity: 1})
	first := startActual(t, n, 1)
	_ = first.Close()
	n.config.Monitor.(*testMonitor).state.ResumeSequence++
	n.Maintain(t.Context())
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	next := attemptFor(1)
	next.Binding.AttemptSequence++
	second, err := n.NewPeerConnection(ctx, next)
	if err != nil {
		t.Fatal("old demand blocked its own replacement", err)
	}
	_ = second.Close()
}
