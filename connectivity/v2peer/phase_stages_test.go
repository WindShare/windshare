package v2peer

import (
	"context"
	"errors"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
)

func TestRecoveryReservationUsesEffectiveProviderPhases(t *testing.T) {
	injected := ReceiverPeerConnectionFactoryFunc(func(config pion.Configuration) (ReceiverPeerConnection, error) {
		return pion.NewPeerConnection(config)
	})
	for _, tc := range []struct {
		name   string
		config ReceiverFactoryConfig
		want   time.Duration
	}{
		{"native default", ReceiverFactoryConfig{}, 77 * time.Second},
		{"native staged", ReceiverFactoryConfig{NegotiationBudget: time.Second, AdmissionBudget: MaximumPeerAdmissionBudget}, 82 * time.Second},
		{"injected negotiation", ReceiverFactoryConfig{PeerConnections: injected, NegotiationBudget: 60 * time.Second, AdmissionBudget: 24 * time.Second}, 84 * time.Second},
		{"outer limit", ReceiverFactoryConfig{PeerConnections: injected, NegotiationBudget: time.Duration(1<<63 - 1)}, 85 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory, err := newTestReceiverFactory(t, tc.config)
			if err != nil {
				t.Fatal(err)
			}
			if got := factory.AttemptBudget(85 * time.Second); got != tc.want {
				t.Fatalf("attempt budget = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestNativeObservedStagesPreserveCheckingWindowAndCancelOriginalIO(t *testing.T) {
	timers := newRecordingReceiverPhaseTimerSource()
	lifecycle := newPeerPhaseLifecycle(timers, DefaultPeerNegotiationBudget, DefaultPeerAdmissionBudget)
	lifecycle.staged = true
	ctx, err := lifecycle.beginNegotiation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.terminate(context.Canceled)
	preparation := receiveTest(t, timers.created)
	if preparation.phase != PeerAttemptPhasePreparation || preparation.duration != 10*time.Second {
		t.Fatal(preparation)
	}
	lifecycle.observeICE(pion.ICEConnectionStateChecking)
	checking := receiveTest(t, timers.created)
	if checking.phase != PeerAttemptPhaseChecking || checking.duration != 32*time.Second {
		t.Fatal(checking)
	}
	preparation.timer.Fire()
	if ctx.Err() != nil {
		t.Fatal("retired preparation deadline canceled ICE")
	}
	lifecycle.observeICE(pion.ICEConnectionStateChecking)
	select {
	case extra := <-timers.created:
		t.Fatal("duplicate checking reset deadline", extra)
	default:
	}
	checking.timer.Fire()
	receiveTest(t, ctx.Done())
	if !errors.Is(context.Cause(ctx), ErrPeerNegotiationTimeout) {
		t.Fatal(context.Cause(ctx))
	}
	lifecycle.observeICE(pion.ICEConnectionStateConnected)
	select {
	case extra := <-timers.created:
		t.Fatal("late state resurrected expired attempt", extra)
	default:
	}
}
func TestNativeEstablishedPairAdvancesToDataChannelThenAuthenticatedAdmission(t *testing.T) {
	timers := newRecordingReceiverPhaseTimerSource()
	lifecycle := newPeerPhaseLifecycle(timers, DefaultPeerNegotiationBudget, DefaultPeerAdmissionBudget)
	lifecycle.staged = true
	ctx, err := lifecycle.beginNegotiation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.terminate(context.Canceled)
	receiveTest(t, timers.created)
	lifecycle.observeICE(pion.ICEConnectionStateChecking)
	checking := receiveTest(t, timers.created)
	lifecycle.observeICE(pion.ICEConnectionStateConnected)
	establishment := receiveTest(t, timers.created)
	if establishment.phase != PeerAttemptPhaseEstablishing || establishment.duration != 15*time.Second {
		t.Fatal(establishment)
	}
	checking.timer.Fire()
	if ctx.Err() != nil {
		t.Fatal("successful checks retained timeout")
	}
	lifecycle.observeICE(pion.ICEConnectionStateCompleted)
	select {
	case extra := <-timers.created:
		t.Fatal("ICE completed reset establishment", extra)
	default:
	}
	admission, started, err := lifecycle.beginAdmission(context.Background())
	if err != nil || !started {
		t.Fatal(err)
	}
	phase := receiveTest(t, timers.created)
	if phase.duration != 20*time.Second || phase.phase != PeerAttemptPhaseAdmission {
		t.Fatal(phase)
	}
	phase.timer.Fire()
	receiveTest(t, admission.Done())
	if !errors.Is(context.Cause(admission), ErrPeerAdmissionTimeout) {
		t.Fatal(context.Cause(admission))
	}
}
