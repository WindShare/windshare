package nativepeer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/pionpeer"
	"github.com/windshare/windshare/core/session/protocolsession"
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

func TestDelayedPollCannotRetireCurrentGenerationLease(t *testing.T) {
	_, _, makeNative := newAdmissionFixture(t, 0)
	n := makeNative()
	monitor := n.config.Monitor.(*testMonitor)
	stale := &testMonitor{state: monitor.state}
	n.Maintain(t.Context())
	monitor.state.ResumeSequence++
	_ = startActual(t, n, 1).Close()
	generation := n.NetworkGeneration()
	if generation != 2 {
		t.Fatal("missing replacement generation", generation)
	}
	n.config.Monitor = stale
	request := attemptFor(1)
	request.Binding.AttemptSequence++
	prepared, err := n.PrepareAttempt(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if prepared.snapshot.GenerationID() != generation || prepared.path.lease == nil || prepared.path.lease.GenerationID() != generation {
		t.Fatal("delayed first-generation sample displaced the current lease")
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

func TestCloseReleasesAdmissionSynchronouslyWhilePreservingProviderClosedDrain(t *testing.T) {
	for _, mode := range []string{"active-observation", "concurrent-close"} {
		t.Run(mode, func(t *testing.T) {
			gate, _, makeNative := newAdmissionFixture(t, 0)
			n := makeNative()
			targetReq := attemptFor(ConcurrentAttempts)
			entered, release := make(chan struct{}), make(chan struct{})
			var enterOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)

			n.config.Connect = func(cfg pion.Configuration, req pionpeer.AttemptConfig) (*pionpeer.Connection, error) {
				if req.ProtocolSessionID == targetReq.ProtocolSessionID {
					origObserve := req.Observe
					req.Observe = func(event pionpeer.Event) {
						shouldBlock := (mode == "active-observation" && event.Candidate != nil) ||
							(mode == "concurrent-close" && event.Milestone == pionpeer.ProviderClosed)
						if shouldBlock {
							enterOnce.Do(func() {
								close(entered)
								<-release
							})
						}
						origObserve(event)
					}
				}
				return pionpeer.NewPeerConnection(cfg, req)
			}

			peers := make([]*pionpeer.Connection, 0, ConcurrentAttempts)
			for i := byte(1); i <= ConcurrentAttempts; i++ {
				peers = append(peers, startActual(t, n, i))
			}
			t.Cleanup(func() {
				unblock()
				for _, pc := range peers {
					_ = pc.Close()
				}
			})
			target := peers[ConcurrentAttempts-1]

			var firstCloseDone chan error
			if mode == "active-observation" {
				if _, err := target.CreateDataChannel("close-quiescence", nil); err != nil {
					t.Fatal(err)
				}
				offer, err := target.CreateOffer(nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := target.SetLocalDescription(offer); err != nil {
					t.Fatal(err)
				}
				select {
				case <-entered:
				case <-time.After(2 * time.Second):
					t.Fatal("candidate observation did not start")
				}
				if err := target.Close(); err != nil {
					t.Fatalf("target.Close err = %v", err)
				}
			} else {
				firstCloseDone = make(chan error, 1)
				go func() { firstCloseDone <- target.Close() }()
				select {
				case <-entered:
				case <-time.After(2 * time.Second):
					t.Fatal("provider_closed observation did not start")
				}
				if err := target.Close(); err != nil {
					t.Fatalf("concurrent target.Close err = %v", err)
				}
			}

			gate.mu.Lock()
			active := gate.active
			gate.mu.Unlock()
			if active != ConcurrentAttempts-1 {
				t.Fatalf("active after Close = %d, want %d", active, ConcurrentAttempts-1)
			}
			immediate, err := n.PrepareAttemptWithin(t.Context(), attemptFor(ConcurrentAttempts+1), 0)
			if err != nil {
				t.Fatalf("immediate retry after Close failed: %v", err)
			}
			immediate.Close()

			// ClosePath must still wait for the in-flight ProviderClosed observation
			// so CompleteObservations cannot truncate the terminal provider event.
			closePathDone := make(chan struct{})
			go func() {
				n.ClosePath(targetReq.ProtocolSessionID, targetReq.Binding.PeerPathID)
				close(closePathDone)
			}()
			sawPathClosed := false
			for !sawPathClosed {
				select {
				case event := <-n.Observations():
					if event.Subject.ProtocolSessionID == targetReq.ProtocolSessionID &&
						event.Lifecycle != nil && event.Lifecycle.Kind == PathClosed {
						sawPathClosed = true
					}
					if event.Subject.ProtocolSessionID == targetReq.ProtocolSessionID &&
						event.Provider != nil && event.Provider.Milestone == pionpeer.ProviderClosed {
						t.Fatal("provider_closed observed before blocked observer was released")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("missing PathClosed observation")
				}
			}
			select {
			case <-closePathDone:
				t.Fatal("ClosePath returned before ProviderClosed observation finished")
			default:
			}

			unblock()
			select {
			case <-closePathDone:
			case <-time.After(2 * time.Second):
				t.Fatal("ClosePath did not finish after observer release")
			}
			if firstCloseDone != nil {
				if err := <-firstCloseDone; err != nil {
					t.Fatalf("winner target.Close err = %v", err)
				}
			}
			for _, pc := range peers[:ConcurrentAttempts-1] {
				if err := pc.Close(); err != nil {
					t.Fatalf("sibling Close err = %v", err)
				}
			}
			n.CompleteObservations()
			sawProviderClosed := false
			for event := range n.Observations() {
				if event.Subject.ProtocolSessionID == targetReq.ProtocolSessionID &&
					event.Provider != nil && event.Provider.Milestone == pionpeer.ProviderClosed {
					sawProviderClosed = true
				}
			}
			if !sawProviderClosed {
				t.Fatal("CompleteObservations after ClosePath missed target provider_closed event")
			}
		})
	}
}
