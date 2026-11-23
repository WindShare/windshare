package nativepeer

import (
	"context"
	"errors"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
	"time"
)

type expiringSnapshot struct{ expire func() }

func (m expiringSnapshot) Poll(ctx context.Context, _ time.Time) (networkstate.Snapshot, bool, error) {
	m.expire()
	return networkstate.Snapshot{}, false, ctx.Err()
}

func TestImmediateAdmissionDeclinesColdSnapshotWithoutPolling(t *testing.T) {
	_, _, makeNative := newAdmissionFixture(t, 0)
	n := makeNative()
	n.config.Monitor = expiringSnapshot{expire: func() { t.Fatal("cold snapshot polled without queue time") }}
	if _, err := n.PrepareAttemptWithin(t.Context(), attemptFor(1), 0); !errors.Is(err, ErrAdmission) {
		t.Fatalf("cold immediate admission = %v", err)
	}
}

func TestQueueDeadlineInterruptsSnapshotBeforeReservingCapacity(t *testing.T) {
	gate, clock, makeNative := newAdmissionFixture(t, 0)
	n := makeNative()
	const wait = 3 * time.Second
	n.config.Monitor = expiringSnapshot{expire: func() { clock.advance(wait) }}
	if _, err := n.PrepareAttemptWithin(t.Context(), attemptFor(1), wait); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("snapshot deadline cause = %v", err)
	}
	if gate.active != 0 || gate.starts != StartsPerWindow || gate.activeTime != float64(ActiveTimePerWindow) {
		t.Fatal("expired snapshot acquired a resource permit")
	}
}

func TestProcessProviderQueueHasFiniteCapacityAndManagerCloseDetachesWaiters(t *testing.T) {
	gate, _, makeNative := newAdmissionFixture(t, 0)
	active, waiting, overflow := makeNative(), makeNative(), makeNative()
	for i := byte(1); i <= ConcurrentAttempts; i++ {
		startActual(t, active, i)
	}
	var results []<-chan admissionResult
	for i := byte(1); i <= MaximumQueuedAttempts; i++ {
		results = append(results, queuedActual(t, waiting, i, context.Background()))
	}
	if _, err := overflow.NewPeerConnection(context.Background(), attemptFor(1)); !errors.Is(err, ErrAdmission) {
		t.Fatal("queue overflow allocated resources", err)
	}
	if err := waiting.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, done := range results {
		if actualResult(t, done).err == nil {
			t.Fatal("manager close started queued provider")
		}
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.queue) != 0 || gate.active != ConcurrentAttempts || gate.starts != StartsPerWindow-ConcurrentAttempts {
		t.Fatal("canceled queue consumed sibling allowance")
	}
}
func TestProcessAdmissionDoesNotMintTimeWhenClockMovesBackwards(t *testing.T) {
	gate, clock, makeNative := newAdmissionFixture(t, 0)
	n := makeNative()
	pc := startActual(t, n, 1)
	clock.advance(-time.Second)
	_ = pc.Close()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.starts != StartsPerWindow-1 || gate.activeTime != float64(ActiveTimePerWindow) {
		t.Fatal("backwards clock minted allowance")
	}
}
func TestUnstartedPreparationExpiresAndCannotBeReused(t *testing.T) {
	gate, clock, makeNative := newAdmissionFixture(t, 0)
	n := makeNative()
	prepared, err := n.PrepareAttempt(context.Background(), attemptFor(1))
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Matches(attemptFor(1).ProtocolSessionID, attemptFor(1).Binding) || prepared.Matches(attemptFor(2).ProtocolSessionID, attemptFor(1).Binding) {
		t.Fatal("preparation lost immutable identity")
	}
	var missingContext context.Context // Exercise rejection before a preparation can acquire resources.
	if _, err := prepared.Start(missingContext); !errors.Is(err, ErrAdmission) {
		t.Fatal(err)
	}
	clock.advance(PermitLifetime)
	if _, err := prepared.Start(context.Background()); !errors.Is(err, ErrAdmission) {
		t.Fatal("expired preparation revived", err)
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.active != 0 {
		t.Fatal("abandoned preparation retained active capacity")
	}
}

func TestRetiredPathOrNetworkCancelsQueuedAdmissionWithoutChargingStart(t *testing.T) {
	for _, kind := range []string{"revoke", "network"} {
		t.Run(kind, func(t *testing.T) {
			gate, _, makeNative := newAdmissionFixture(t, 0)
			blockers, waiting := makeNative(), makeNative()
			for i := byte(1); i <= ConcurrentAttempts; i++ {
				startActual(t, blockers, i)
			}
			result := queuedActual(t, waiting, 9, context.Background())
			if kind == "revoke" {
				_, applied := waiting.ApplyControl(attemptFor(9).ProtocolSessionID, controlBytes(t, 1, protocolsession.PeerPathRevoke, 0))
				if !applied {
					t.Fatal("revoke ignored")
				}
			} else {
				waiting.config.Monitor.(*testMonitor).state.ResumeSequence++
				waiting.Maintain(context.Background())
			}
			if actualResult(t, result).err == nil {
				t.Fatal("retired preparation created provider")
			}
			gate.mu.Lock()
			defer gate.mu.Unlock()
			if gate.starts != StartsPerWindow-ConcurrentAttempts || len(gate.queue) != 0 {
				t.Fatal("retirement charged a new start")
			}
		})
	}
}
