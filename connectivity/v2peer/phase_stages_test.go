package v2peer

import (
	"context"
	"errors"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
)

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
	if checking.phase != PeerAttemptPhaseChecking || checking.duration != 40*time.Second {
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
