package engine

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const capacityTestTimeout = 5 * time.Second

type capacityNetwork struct{}

func (capacityNetwork) Snapshot(context.Context) (networkstate.State, error) {
	return networkstate.State{Addresses: []networkstate.Address{
		{IP: netip.MustParseAddr("192.0.2.1"), InterfaceIndex: 1},
	}}, nil
}

func capacityNative(t *testing.T, application *Engine) *nativepeer.NativePeerConnectivity {
	t.Helper()
	pool, err := icepolicy.NewICEEndpointPool(nil)
	if err != nil {
		t.Fatal(err)
	}
	native, err := application.connectivity.NewNative(nativepeer.Config{
		Now:                 application.config.Clock.Now,
		Pool:                func() icepolicy.ICEEndpointPool { return pool },
		Monitor:             networkstate.NewMonitor(capacityNetwork{}, time.Nanosecond),
		Reachability:        reachability.New(reachability.Config{Now: application.config.Clock.Now}),
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
	first := newTestEngine(t, Config{Clock: engineTestClock{now: now}})
	second := newTestEngine(t, Config{Clock: engineTestClock{now: now}})
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

func TestEngineIsolatesSenderAndReceiverTURNPermitsAcrossDirectionsAndEngines(t *testing.T) {
	first := newTestEngine(t, Config{})
	second := newTestEngine(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), capacityTestTimeout)
	defer cancel()

	var receiverReleases []func()
	for range protocolsession.MaxGrantsPerMessage {
		release, err := first.connectivity.ReceiverTURN().Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		receiverReleases = append(receiverReleases, release)
	}
	defer func() {
		for _, release := range receiverReleases {
			release()
		}
	}()

	var senderReleases []func()
	for range protocolsession.MaxGrantsPerMessage {
		release, err := first.connectivity.SenderTURN().Acquire(ctx)
		if err != nil {
			t.Fatalf("same-engine sender TURN acquire blocked by full receiver pool: %v", err)
		}
		senderReleases = append(senderReleases, release)
	}
	defer func() {
		for _, release := range senderReleases {
			release()
		}
	}()

	otherReceiver, err := second.connectivity.ReceiverTURN().Acquire(ctx)
	if err != nil {
		t.Fatalf("independent Engine inherited exhausted receiver TURN pool: %v", err)
	}
	otherReceiver()
	otherSender, err := second.connectivity.SenderTURN().Acquire(ctx)
	if err != nil {
		t.Fatalf("independent Engine inherited exhausted sender TURN pool: %v", err)
	}
	otherSender()
}
