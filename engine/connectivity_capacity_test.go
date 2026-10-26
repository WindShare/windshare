package engine

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/turncapacity"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2peer/peerset"
	"github.com/windshare/windshare/connectivity/v2signal"

	sessioncontract "github.com/windshare/windshare/core/session"
)

const capacityTestTimeout = 5 * time.Second
const capacityQueueProbe = 50 * time.Millisecond

type capacityNetwork struct{}

func (capacityNetwork) Snapshot(context.Context) (networkstate.State, error) {
	return networkstate.State{Addresses: []networkstate.Address{
		{IP: netip.MustParseAddr("127.0.0.1"), InterfaceIndex: 1},
	}}, nil
}

func capacityNative(t *testing.T, application *Engine) *nativepeer.NativePeerConnectivity {
	t.Helper()
	pool, err := icepolicy.NewICEEndpointPool(nil)
	if err != nil {
		t.Fatal(err)
	}
	native, err := application.connectivity.NewNative(nativepeer.Config{
		Now:                 application.config.Now,
		Pool:                &pool,
		Monitor:             networkstate.NewMonitor(capacityNetwork{}, time.Nanosecond),
		Reachability:        reachability.New(reachability.Config{Now: application.config.Now}),
		ObservationCapacity: nativepeer.DefaultObservationCapacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), capacityTestTimeout)
		defer cancel()
		if err := native.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return native
}

func capacityRequest() nativepeer.AttemptRequest {
	return nativepeer.AttemptRequest{
		ProtocolSessionID: [16]byte{1},
		Binding:           v2signal.Binding{PeerPathID: v2signal.PeerPathID{1}, AttemptID: v2signal.AttemptID{1}, AttemptSequence: 1},
	}
}

func TestEngineAdmissionSurvivesOwnerReplacementAndIsolatesOtherEngines(t *testing.T) {
	now := func() time.Time { return time.Unix(100, 0) }
	first := newTestEngine(t, Config{Now: now})
	second := newTestEngine(t, Config{Now: now})
	ctx, cancel := context.WithTimeout(context.Background(), capacityTestTimeout)
	defer cancel()

	for range nativepeer.StartsPerWindow {
		native := capacityNative(t, first)
		prepared, err := native.PrepareAttempt(ctx, capacityRequest())
		if err != nil {
			t.Fatal(err)
		}
		prepared.Close()
		if err := native.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}

	native := capacityNative(t, first)
	wait, stop := context.WithCancel(ctx)
	defer stop()
	pending := make(chan error, 1)
	go func() {
		prepared, err := native.PrepareAttempt(wait, capacityRequest())
		prepared.Close()
		pending <- err
	}()
	for {
		select {
		case event := <-native.Observations():
			if event.Admission == nil || event.Admission.Kind != nativepeer.AdmissionQueued {
				continue
			}
			if event.Admission.StartsRemaining != 0 {
				t.Fatalf("replacement minted allowance: %+v", event.Admission)
			}
		case <-ctx.Done():
			t.Fatal("exhausted admission did not queue")
		}
		break
	}

	independent := capacityNative(t, second)
	prepared, err := independent.PrepareAttempt(ctx, capacityRequest())
	if err != nil {
		t.Fatalf("another Engine inherited exhausted allowance: %v", err)
	}
	prepared.Close()
	stop()
	if err := <-pending; !errors.Is(err, context.Canceled) {
		t.Fatalf("exhausted owner escaped its queue: %v", err)
	}
}

func TestEngineTURNCapacitySharesAcrossOwnersAndIsolatesOtherEngines(t *testing.T) {
	var seconds atomic.Int64
	now := func() time.Time { return time.Unix(100+seconds.Load(), 0) }
	first := newTestEngine(t, Config{Now: now})
	second := newTestEngine(t, Config{Now: now})
	ctx, cancel := context.WithTimeout(context.Background(), capacityTestTimeout)
	defer cancel()
	credential, err := iceconfig.Fixed("user", "password")
	if err != nil {
		t.Fatal(err)
	}
	relay, err := iceconfig.NewRelay(iceconfig.Identity{Provider: "test", Account: "account", Profile: "profile"},
		[]string{"turn:127.0.0.1:3478"}, credential)
	if err != nil {
		t.Fatal(err)
	}
	request := capacityRequest()
	request.ICE, err = iceconfig.New(nil, &relay, iceconfig.RelayCandidates)
	if err != nil {
		t.Fatal(err)
	}

	var held []*nativepeer.PreparedAttempt
	for range turncapacity.MaximumConnections {
		native := capacityNative(t, first)
		prepared, err := native.PrepareAttempt(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(prepared.Close)
		held = append(held, prepared)
		// An admitted path keeps TURN ownership after returning its attempt slot.
		native.SetAdmitted(request.ProtocolSessionID, request.Binding.PeerPathID, false)
		seconds.Add(int64(nativepeer.AdmissionWindow / time.Second))
	}
	blocked := capacityNative(t, first)
	wait, stop := context.WithTimeout(ctx, capacityQueueProbe)
	prepared, err := blocked.PrepareAttempt(wait, request)
	stop()
	prepared.Close()
	if !errors.Is(err, turncapacity.ErrCapacity) {
		t.Fatalf("another native owner bypassed the Engine TURN limit: %v", err)
	}
	independent := capacityNative(t, second)
	prepared, err = independent.PrepareAttempt(ctx, request)
	if err != nil {
		t.Fatalf("another Engine inherited full TURN capacity: %v", err)
	}
	prepared.Close()

	held[0].Close()
	prepared, err = blocked.PrepareAttempt(ctx, request)
	if err != nil {
		t.Fatalf("released TURN capacity was not reusable: %v", err)
	}
	prepared.Close()
}

type capacityAttempt struct {
	done chan struct{}
	once sync.Once
}

func (*capacityAttempt) Ready() <-chan struct{}  { return nil }
func (a *capacityAttempt) Done() <-chan struct{} { return a.done }
func (*capacityAttempt) Lane() (sessioncontract.LaneIdentity, bool) {
	return sessioncontract.LaneIdentity{}, false
}
func (*capacityAttempt) Outcome() v2peer.ReceiverAttemptOutcome {
	return v2peer.ReceiverAttemptOutcome{}
}
func (*capacityAttempt) Err() error     { return nil }
func (a *capacityAttempt) Close() error { a.once.Do(func() { close(a.done) }); return nil }

func TestEngineAttemptCapacitySharesAcrossReceivesAndIsolatesOtherEngines(t *testing.T) {
	first := newTestEngine(t, Config{})
	second := newTestEngine(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), capacityTestTimeout)
	defer cancel()
	open := func(parent context.Context, application *Engine) (*peerset.Path, <-chan struct{}) {
		t.Helper()
		owner, err := peerset.New(peerset.Config{Capacity: application.connectivity.Attempts()})
		if err != nil {
			t.Fatal(err)
		}
		started := make(chan struct{}, 1)
		path, err := owner.Open(parent, peerset.PathConfig{
			Demand: peerset.ContentDemand,
			Start: func(context.Context, v2signal.Binding) (peerset.Attempt, error) {
				started <- struct{}{}
				return &capacityAttempt{done: make(chan struct{})}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = path.Close() })
		return path, started
	}
	awaitStart := func(started <-chan struct{}) {
		t.Helper()
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("available attempt capacity did not start")
		}
	}
	var held []*peerset.Path
	for range peerset.DefaultAttemptCapacity {
		path, started := open(ctx, first)
		awaitStart(started)
		held = append(held, path)
	}
	wait, stop := context.WithTimeout(ctx, capacityQueueProbe)
	defer stop()
	blocked, started := open(wait, first)
	select {
	case <-blocked.Done():
	case <-ctx.Done():
		t.Fatal("queued attempt did not cancel")
	}
	select {
	case <-started:
		t.Fatal("another receive bypassed the Engine attempt limit")
	default:
	}
	_, independent := open(ctx, second)
	awaitStart(independent)

	_ = held[0].Close()
	_, released := open(ctx, first)
	awaitStart(released)
}
