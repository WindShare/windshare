package nativepeer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

func TestTURNReservationsLeaveACompleteDirectOpportunity(t *testing.T) {
	gate, _, makeNative := newAdmissionFixture(t, 0)
	native := makeNative()
	credential, _ := iceconfig.Fixed("u", "p")
	relay, _ := iceconfig.NewRelay(iceconfig.Identity{Provider: "test", Account: "a", Profile: "p"}, []string{"turn:127.0.0.1:3478"}, credential)
	configuration, _ := iceconfig.New(nil, &relay, iceconfig.RelayCandidates)
	for id := byte(1); id < ProcessConcurrentAttempts; id++ {
		request := attemptFor(id)
		request.ICE = configuration
		prepared, err := native.PrepareAttempt(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(prepared.Close)
	}
	waiting, cancel := context.WithCancel(context.Background())
	defer cancel()
	pending := make(chan error, 1)
	go func() {
		request := attemptFor(20)
		request.ICE = configuration
		prepared, err := native.PrepareAttempt(waiting, request)
		if prepared != nil {
			prepared.Close()
		}
		pending <- err
	}()
	admissionEvent(t, native, AdmissionQueued, 20)
	direct, err := native.PrepareAttempt(context.Background(), attemptFor(21))
	if err != nil {
		t.Fatal(err)
	}
	direct.Close()
	cancel()
	if err := <-pending; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	gate.mu.Lock()
	active := gate.active
	gate.mu.Unlock()
	if active != ProcessConcurrentAttempts-1 {
		t.Fatal("TURN consumed direct capacity", active)
	}
}
