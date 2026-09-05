package peerset

import (
	"context"
	"net/netip"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/transport/webrtc/provider"
)

func TestProcessQueueUsesWaveTimeAndPreservesCompleteNativeCheckingOpportunity(t *testing.T) {
	clock := newClock()
	gate := nativepeer.NewProcessAdmission(nativepeer.AdmissionClock{Now: clock.Now})
	pool, _ := icepolicy.NewICEEndpointPool(nil)
	captured := make(chan provider.AttemptConfig, 8)
	native := nativepeer.New(nativepeer.Config{Admission: gate, Now: clock.Now, Pool: &pool, Monitor: networkstate.NewMonitor(fakeNetwork{}, time.Nanosecond),
		Reachability: reachability.New(reachability.Config{Now: clock.Now}), ObservationCapacity: 64,
		Connect: func(config pion.Configuration, request provider.AttemptConfig) (*provider.Connection, error) {
			captured <- request
			return provider.NewPeerConnection(config, request)
		}})
	defer native.Close(context.Background())
	binding := v2signal.Binding{PeerPathID: v2signal.PeerPathID{1}, AttemptID: v2signal.AttemptID{1}, AttemptSequence: 1}
	var blockers []*provider.Connection
	for i := byte(1); i <= nativepeer.ProcessConcurrentAttempts; i++ {
		pc, err := native.NewPeerConnection(context.Background(), nativepeer.AttemptRequest{ProtocolSessionID: [16]byte{i}, Binding: binding})
		if err != nil {
			t.Fatal(err)
		}
		blockers = append(blockers, pc)
		receive(t, captured)
	}
	owner, _ := New(Config{Clock: clock})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := &Path{owner: owner, ctx: ctx}
	var activeUsed time.Duration
	started := make(chan struct{})
	path.config.Prepare = func(ctx context.Context, binding v2signal.Binding) (PreparedStarter, error) {
		prepared, err := native.PrepareAttempt(ctx, nativepeer.AttemptRequest{ProtocolSessionID: [16]byte{9}, Binding: binding})
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
		opportunities <- path.prepareProvider(wave, binding, func(used time.Duration) { activeUsed = used }, func() {})
	}()
	nextTimer(t, clock, nativepeer.ProcessQueueBudget)
	for {
		event := receive(t, native.Observations())
		if event.Admission != nil && event.Admission.Kind == nativepeer.AdmissionQueued && event.Subject.ProtocolSessionID == [16]byte{9} {
			break
		}
	}
	clock.advance(50 * time.Second)
	_ = blockers[0].Close()
	opportunity := receive(t, opportunities)
	if opportunity.budget != 70*time.Second {
		t.Fatal("queue reset wave", opportunity.budget)
	}
	results := make(chan Result, 1)
	go func() { result, _ := path.executePrepared(opportunity); results <- result }()
	receive(t, started)
	actual := receive(t, captured)
	if actual.InitialCheckingTimeout < 40*time.Second || actual.SocketLease.Endpoints()[0].Addr() != netip.MustParseAddr("127.0.0.1") {
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
