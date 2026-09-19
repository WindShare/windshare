package v2peer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/socketauthority"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestSenderResourceWaitExpiresBeforeRemotePreparation(t *testing.T) {
	for _, constraint := range []string{"process", "sockets"} {
		t.Run(constraint, func(t *testing.T) {
			var sockets *socketauthority.Authority
			if constraint == "sockets" {
				sockets = socketauthority.New(socketauthority.Config{Capacity: nativepeer.ProcessConcurrentAttempts})
			}
			h := newResourceQueuedHandshake(t, sockets)
			code, typed, reason := protocolsession.PeerOperationCodeTimeout, TypedPeerErrorTimeout, "resource_preparation_timeout"
			if sockets != nil {
				// Admitted lanes release attempt permits while retaining physical
				// sockets, isolating socket pressure from the process-start limit.
				for i := byte(1); i <= nativepeer.ProcessConcurrentAttempts; i++ {
					h.native.SetDirect([16]byte{i}, testBinding(i).PeerPathID)
				}
				for {
					event := receiveTest(t, h.native.Observations())
					if event.Admission != nil && event.Admission.SocketCapacity != nil {
						break
					}
				}
				code, typed, reason = protocolsession.PeerOperationCodeCapacity, TypedPeerErrorBusy, "socket_capacity"
			}
			h.clock.Advance(PeerSignalingPreparationBudget - PeerAnswerPreparationReserve)
			receiveTest(t, h.sender.done)
			failure := receiveTest(t, h.session.failures)
			if failure.code != code {
				t.Fatalf("resource deadline lost its typed failure: %+v", failure)
			}
			if h.receiver.phaseContext.Err() != nil {
				t.Fatal("resource wait consumed the receiver's failure delivery margin")
			}
			if h.started.Load() != nativepeer.ProcessConcurrentAttempts {
				t.Fatal("resource timeout started a provider")
			}
			expirationObserved := false
			for {
				event := receiveTest(t, h.sender.config.factory.SenderAttemptObservations())
				if event.Stage == SenderAttemptNegotiationDeadlineExpired {
					expirationObserved = event.DeadlineMillis == durationMilliseconds(PeerSignalingPreparationBudget-PeerAnswerPreparationReserve)
				}
				if event.Failure == nil {
					continue
				}
				if event.Failure.Scope != AttemptFailureScopeAttempt || event.Failure.TypedPeerErrorCode != typed || event.Failure.Termination.Cause != reason {
					t.Fatalf("resource wait was reported as runtime termination: %+v", event.Failure)
				}
				if !expirationObserved || !event.Failure.DeadlineExpired {
					t.Fatal("resource deadline was absent from failure diagnostics")
				}
				break
			}
		})
	}
}

func TestResourcePreparationKeepsOriginalSignalingDeadline(t *testing.T) {
	clock := &handshakeClock{now: time.Unix(1, 0)}
	timers := handshakeTimers{clock: clock, created: make(chan recordedReceiverPhaseTimer, 4)}
	lifecycle := newPeerPhaseLifecycle(timers, DefaultPeerNegotiationBudget, DefaultPeerAdmissionBudget)
	lifecycle.staged = true
	ctx, err := lifecycle.beginNegotiation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.terminate(nil)
	preparation := receiveTest(t, timers.created)
	var resources recordedReceiverPhaseTimer
	err = lifecycle.prepareResources(ctx, func(context.Context) error {
		resources = receiveTest(t, timers.created)
		clock.Advance(resources.duration - time.Second)
		return nil
	})
	if err != nil || !resources.timer.stopped.Load() || preparation.timer.stopped.Load() {
		t.Fatal("successful resource admission changed preparation ownership", err)
	}
	clock.Advance(PeerAnswerPreparationReserve + time.Second)
	receiveTest(t, ctx.Done())
	if !errors.Is(context.Cause(ctx), ErrPeerNegotiationTimeout) {
		t.Fatal("resource admission extended the signaling deadline", context.Cause(ctx))
	}
}

func TestResourcePreparationSettlesDeadlineBeforeSuccessfulCallbackReturn(t *testing.T) {
	timers := newRecordingReceiverPhaseTimerSource()
	lifecycle := newPeerPhaseLifecycle(timers, DefaultPeerNegotiationBudget, DefaultPeerAdmissionBudget)
	lifecycle.staged = true
	ctx, err := lifecycle.beginNegotiation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.terminate(nil)
	receiveTest(t, timers.created)
	err = lifecycle.prepareResources(ctx, func(resourceContext context.Context) error {
		resources := receiveTest(t, timers.created)
		resources.timer.Fire()
		receiveTest(t, resourceContext.Done())
		// Native preparation can return an allocation as its caller's deadline
		// wins. The lifecycle must reject that late result so the sender closes it.
		return nil
	})
	if !errors.Is(err, ErrPeerResourcePreparationTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("expired preparation returned success", err)
	}
	if ctx.Err() != nil {
		t.Fatal("resource deadline cancelled the remaining signaling window")
	}
}

func TestResourcePreparationPreservesDeadlineCauseFromContextError(t *testing.T) {
	timers := newRecordingReceiverPhaseTimerSource()
	lifecycle := newPeerPhaseLifecycle(timers, DefaultPeerNegotiationBudget, DefaultPeerAdmissionBudget)
	lifecycle.staged = true
	ctx, err := lifecycle.beginNegotiation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.terminate(nil)
	receiveTest(t, timers.created)
	err = lifecycle.prepareResources(ctx, func(resourceContext context.Context) error {
		resources := receiveTest(t, timers.created)
		resources.timer.Fire()
		receiveTest(t, resourceContext.Done())
		return errors.Join(errors.New("network inspection interrupted"), resourceContext.Err())
	})
	if !errors.Is(err, ErrPeerResourcePreparationTimeout) || errors.Is(err, context.Canceled) {
		t.Fatal("deadline interruption became owner cancellation", err)
	}
}

func TestResourcePreparationStopsWithItsOwner(t *testing.T) {
	for _, stop := range []string{"operation", "lifecycle", "parent"} {
		t.Run(stop, func(t *testing.T) {
			timers := newRecordingReceiverPhaseTimerSource()
			lifecycle := newPeerPhaseLifecycle(timers, DefaultPeerNegotiationBudget, DefaultPeerAdmissionBudget)
			lifecycle.staged = true
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, err := lifecycle.beginNegotiation(parent)
			if err != nil {
				t.Fatal(err)
			}
			defer lifecycle.terminate(nil)
			receiveTest(t, timers.created)
			done := make(chan error, 1)
			go func() {
				done <- lifecycle.prepareResources(ctx, func(resourceContext context.Context) error {
					<-resourceContext.Done()
					return resourceContext.Err()
				})
			}()
			resources := receiveTest(t, timers.created)
			switch stop {
			case "operation":
				lifecycle.cancelBeforeAdmission(context.Canceled)
			case "lifecycle":
				lifecycle.terminate(context.Canceled)
			case "parent":
				cancel()
			}
			if err := receiveTest(t, done); !errors.Is(err, context.Canceled) || errors.Is(err, ErrPeerResourcePreparationTimeout) {
				t.Fatal("owner cancellation became a resource timeout", err)
			}
			if !resources.timer.stopped.Load() {
				t.Fatal("resource timer survived its owner")
			}
		})
	}
}
