package peerset

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/pionpeer"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
)

func TestNativeMappingOpportunityAtWaveBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		left time.Duration
		busy bool
	}{
		{"three seconds to queue", 45 * time.Second, false},
		{"immediate grant", 42 * time.Second, false},
		{"immediate refusal", 42 * time.Second, true},
		{"too late", 42*time.Second - time.Nanosecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newClock()
			owner, _ := newTestPeerSet(t, Config{Clock: clock})
			native := newTestNative(t, nativepeer.Config{Now: clock.Now,
				Monitor:      networkstate.NewMonitor(fakeNetwork{}, time.Nanosecond),
				Reachability: reachability.New(reachability.Config{Now: clock.Now}),
			})
			factory, err := v2peer.NewReceiverFactory(v2peer.ReceiverFactoryConfig{Native: native})
			if err != nil {
				t.Fatal(err)
			}
			lanes := &fakeReceiverLanes{sessionID: [16]byte{9}}
			receiver := ReceiverConfig{Factory: factory, Lanes: lanes, Signaling: blockingReceiverSignaling{}}
			p := &Path{owner: owner, ctx: t.Context(), slot: testPeerSlot(t), key: pathKey{lanes.sessionID, v2signal.PeerPathID{1}},
				demand: ContentDemand, config: PathConfig{Native: native, StopAfterWave: true},
			}
			wait := make(chan time.Duration, 1)
			p.config.Prepare = func(ctx context.Context, binding v2signal.Binding, queueWait time.Duration) (PreparedStarter, error) {
				wait <- queueWait
				return receiver.prepare(ctx, binding, queueWait)
			}
			wave := recoveryWave{started: clock.Now(), attempts: AttemptsPerWave - 1, opportunities: AttemptsPerWave - 1}
			p.deferred = p.budget().reserveDeferred(clock.Now(), DefaultAttemptBudget)
			for range wave.attempts {
				binding, err := p.freshBinding()
				if err != nil {
					t.Fatal(err)
				}
				previous, err := factory.PrepareBinding(p.ctx, lanes, binding, nativepeer.QueueBudget)
				if err != nil {
					t.Fatal(err)
				}
				previous.Close()
			}
			if tc.busy {
				for i := range nativepeer.ConcurrentAttempts {
					binding := v2signal.Binding{PeerPathID: p.key.path, AttemptID: v2signal.AttemptID{1}, AttemptSequence: 1}
					held, err := native.PrepareAttempt(p.ctx, nativepeer.AttemptRequest{ProtocolSessionID: [16]byte{byte(i + 1)}, Binding: binding})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(held.Close)
				}
			}
			clock.advance(WaveBudget - tc.left)
			p.handleNativeChange(p.ctx, nativepeer.Change{MappingReady: true, Remote: true})
			opportunity, stop := p.prepareOpportunity(&wave)
			if tc.left >= MinimumAttemptOpportunity {
				if got := receive(t, wait); got != tc.left-MinimumAttemptOpportunity {
					t.Fatalf("queue allowance %s", got)
				}
			} else if len(wait) != 0 {
				t.Fatal("started preparation after the checking opportunity elapsed")
			}
			if tc.busy || tc.left < MinimumAttemptOpportunity {
				if stop == nil {
					stop = p.nextWave(&wave)
				}
				if opportunity != nil || !errors.Is(stop.Cause, ErrWaveExhausted) ||
					p.budget().active != float64(ActiveTimePerWindow) || p.budget().attempts != AttemptsPerWindow {
					t.Fatalf("unstarted mapping retry consumed its reservation: %+v, %+v", opportunity, stop)
				}
				return
			}
			if stop != nil || opportunity == nil || opportunity.budget != tc.left || opportunity.binding.Purpose != v2signal.Recovery {
				t.Fatalf("expected mapped retry capped at %s, got %+v, %+v", tc.left, opportunity, stop)
			}
			defer opportunity.close()
			attempt, err := opportunity.start(p.ctx, opportunity.binding)
			if err != nil {
				t.Fatal(err)
			}
			_ = attempt.Close()
			opportunity.refund(0)
		})
	}
}

func TestProcessQueueUsesWaveTimeAndPreservesCompleteNativeCheckingOpportunity(t *testing.T) {
	clock := newClock()
	gate := nativepeer.NewAdmission(nativepeer.AdmissionClock{Now: clock.Now})
	pool, _ := icepolicy.NewICEEndpointPool(nil)
	captured := make(chan pionpeer.AttemptConfig, 8)
	native := newTestNative(t, nativepeer.Config{Admission: gate, Now: clock.Now, Pool: func() icepolicy.ICEEndpointPool { return pool }, Monitor: networkstate.NewMonitor(fakeNetwork{}, time.Nanosecond),
		Reachability: reachability.New(reachability.Config{Now: clock.Now}), ObservationCapacity: 64,
		Connect: func(config pion.Configuration, request pionpeer.AttemptConfig) (*pionpeer.Connection, error) {
			captured <- request
			return pionpeer.NewPeerConnection(config, request)
		}})
	// The test context is canceled before cleanup joins outstanding preparations.
	t.Cleanup(func() { _ = native.Close(context.Background()) })
	binding := v2signal.Binding{PeerPathID: v2signal.PeerPathID{1}, AttemptID: v2signal.AttemptID{1}, AttemptSequence: 1}
	var blockers []*pionpeer.Connection
	for i := byte(1); i <= nativepeer.ConcurrentAttempts; i++ {
		pc, err := native.NewPeerConnection(context.Background(), nativepeer.AttemptRequest{ProtocolSessionID: [16]byte{i}, Binding: binding})
		if err != nil {
			t.Fatal(err)
		}
		blockers = append(blockers, pc)
		receive(t, captured)
	}
	owner, _ := newTestPeerSet(t, Config{Clock: clock})
	ctx := t.Context()
	path := &Path{owner: owner, ctx: ctx}
	var activeUsed time.Duration
	started := make(chan struct{})
	path.config.Prepare = func(ctx context.Context, binding v2signal.Binding, queueWait time.Duration) (PreparedStarter, error) {
		prepared, err := native.PrepareAttemptWithin(ctx, nativepeer.AttemptRequest{ProtocolSessionID: [16]byte{9}, Binding: binding}, queueWait)
		if err != nil {
			return PreparedStarter{}, err
		}
		return PreparedStarter{Close: prepared.Close, Start: func(ctx context.Context, _ v2signal.Binding) (Attempt, error) {
			pc, err := prepared.Start(ctx)
			if err != nil {
				return nil, err
			}
			a := newAttempt()
			go func() { <-ctx.Done(); _ = pc.Close(); _ = a.Close() }()
			close(started)
			return a, nil
		}}, nil
	}
	wave := &recoveryWave{started: clock.Now()}
	opportunities := make(chan *attemptOpportunity, 1)
	go func() {
		opportunity, err := path.prepareProvider(wave, binding, DefaultAttemptBudget, func(used time.Duration) { activeUsed = used })
		if err != nil {
			t.Error(err)
		}
		opportunities <- opportunity
	}()
	for {
		event := receive(t, native.Observations())
		if event.Admission != nil && event.Admission.Kind == nativepeer.AdmissionQueued && event.Subject.ProtocolSessionID == [16]byte{9} {
			break
		}
	}
	clock.advance(50 * time.Second)
	_ = blockers[0].Close()
	opportunity := receive(t, opportunities)
	if opportunity == nil {
		t.Fatal("native queue did not return a preparation")
	}
	if opportunity.budget != 70*time.Second {
		t.Fatal("queue reset wave", opportunity.budget)
	}
	results := make(chan Result, 1)
	go func() { result, _ := path.executePrepared(opportunity); results <- result }()
	receive(t, started)
	actual := receive(t, captured)
	if actual.SocketLease.Endpoints()[0].Addr() != netip.MustParseAddr("192.0.2.1") {
		t.Fatal("native ICE opportunity lost", actual)
	}
	cap := nextTimer(t, clock, 70*time.Second)
	clock.advance(70 * time.Second)
	cap.fire(clock.Now())
	receive(t, results)
	if activeUsed != 70*time.Second || clock.Now().Sub(wave.started) != WaveBudget {
		t.Fatal("queue was charged as active setup or wave extended", activeUsed)
	}
	for _, pc := range blockers {
		_ = pc.Close()
	}
}
