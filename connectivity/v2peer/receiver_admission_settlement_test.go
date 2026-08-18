package v2peer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

var errReceiverLaneInstallation = errors.New("receiver lane installation failed after authenticated acceptance")

type acceptedInstallationFailureGate struct {
	accepted    chan struct{}
	contextDone chan struct{}
	release     chan struct{}
}

func newAcceptedInstallationFailureGate() *acceptedInstallationFailureGate {
	return &acceptedInstallationFailureGate{
		accepted: make(chan struct{}), contextDone: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (gate *acceptedInstallationFailureGate) attach(
	ctx context.Context,
	grant sessionruntime.LaneAttachmentGrant,
	channel protocolsession.FrameChannel,
) (sessionruntime.ReceiverLaneAdmissionResult, error) {
	close(gate.accepted)
	<-ctx.Done()
	close(gate.contextDone)
	<-gate.release
	_ = channel.Close()
	return sessionruntime.ReceiverLaneAdmissionResult{
		GrantOperationID: grant.OperationID,
		Lane: sessionruntime.LaneIdentity{
			ID: grant.LaneID, Epoch: grant.LaneEpoch,
		},
		Disposition:      sessionruntime.ReceiverLaneAdmissionAccepted,
		LaneInstallation: sessionruntime.ReceiverLaneInstallationFailed,
	}, errReceiverLaneInstallation
}

func TestReceiverAcceptedInstallationFailureWinsAdmissionTimeout(t *testing.T) {
	timers := newRecordingReceiverPhaseTimerSource()
	gate := newAcceptedInstallationFailureGate()
	harness := newReceiverHarness(t, func(config *ReceiverFactoryConfig, _ *receiverTestSignaling) {
		config.PhaseTimers = timers
	})
	harness.lanes.attach = gate.attach
	receiveTest(t, timers.created)
	harness.answer(t)
	harness.channel.open()
	receiveTest(t, harness.lanes.requested)
	admission := receiveTest(t, timers.created)
	receiveTest(t, gate.accepted)
	admission.timer.Fire()
	receiveTest(t, gate.contextDone)
	assertReceiverAdmissionStillJoining(t, harness.attempt.Done())
	close(gate.release)
	receiveTest(t, harness.attempt.Done())
	assertAcceptedInstallationFailureOutcome(t, harness)
}

func TestReceiverAcceptedInstallationFailureWinsLifecycleCancellation(t *testing.T) {
	gate := newAcceptedInstallationFailureGate()
	harness := newReceiverHarness(t, nil)
	harness.lanes.attach = gate.attach
	harness.answer(t)
	harness.channel.open()
	receiveTest(t, harness.lanes.requested)
	receiveTest(t, gate.accepted)
	closed := make(chan error, 1)
	go func() { closed <- harness.attempt.Close() }()
	receiveTest(t, gate.contextDone)
	assertReceiverAdmissionStillJoining(t, harness.attempt.Done())
	close(gate.release)
	if err := receiveTest(t, closed); !errors.Is(err, errReceiverLaneInstallation) ||
		errors.Is(err, context.Canceled) {
		t.Fatalf("lifecycle cancellation replaced installation failure: %v", err)
	}
	assertAcceptedInstallationFailureOutcome(t, harness)
}

func TestReceiverAttachmentSettlementCannotMintAuthorityFromInconsistentResult(t *testing.T) {
	grant := sessionruntime.LaneAttachmentGrant{
		LaneID: 7, LaneEpoch: 3, OperationID: testOperationID(90),
	}
	accepted := sessionruntime.ReceiverLaneAdmissionResult{
		GrantOperationID: grant.OperationID,
		Lane:             sessionruntime.LaneIdentity{ID: grant.LaneID, Epoch: grant.LaneEpoch},
		Disposition:      sessionruntime.ReceiverLaneAdmissionAccepted,
		LaneInstallation: sessionruntime.ReceiverLaneInstallationFailed,
	}
	for name, admission := range map[string]sessionruntime.ReceiverLaneAdmissionResult{
		"wrong grant operation": func() sessionruntime.ReceiverLaneAdmissionResult {
			value := accepted
			value.GrantOperationID = testOperationID(91)
			return value
		}(),
		"accepted with rejection": func() sessionruntime.ReceiverLaneAdmissionResult {
			value := accepted
			value.Rejection = protocolsession.LaneRejection{Code: protocolsession.LaneRejectStopping}
			return value
		}(),
		"claims installed with error": func() sessionruntime.ReceiverLaneAdmissionResult {
			value := accepted
			value.LaneInstallation = sessionruntime.ReceiverLaneInstalled
			return value
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			settlement, err := receiverAttachmentSettlement(
				grant, admission, errReceiverLaneInstallation,
			)
			if settlement != receiverAdmissionUnverified || !errors.Is(err, ErrProtocol) {
				t.Fatalf("inconsistent settlement = %v, %v", settlement, err)
			}
		})
	}
}

func assertReceiverAdmissionStillJoining(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("receiver completed before authenticated installation settlement joined")
	default:
	}
}

func assertAcceptedInstallationFailureOutcome(t *testing.T, harness *receiverHarness) {
	t.Helper()
	outcome := harness.attempt.Outcome()
	if !errors.Is(outcome.RetainedCause(), errReceiverLaneInstallation) ||
		!errors.Is(outcome.RetainedCause(), errChannelAdmission) ||
		errors.Is(outcome.RetainedCause(), ErrPeerAdmissionTimeout) ||
		errors.Is(outcome.RetainedCause(), context.Canceled) ||
		outcome.TransitionAuthority() != ReceiverTerminalLocal ||
		outcome.TransitionProvenance() != ReceiverProvenanceLocalOperationContract ||
		outcome.LocallyCanceled() ||
		!outcome.HasRetainedCauseClass(ReceiverCauseChannelAdmission) {
		t.Fatalf("accepted installation outcome = %+v", outcome)
	}
	if _, admitted := harness.attempt.Lane(); admitted {
		t.Fatal("failed installation published an admitted lane")
	}
	if harness.peer.closeCalls.Load() != 1 || harness.channel.closeCalls.Load() != 1 {
		t.Fatalf(
			"failed installation teardown peer=%d channel=%d",
			harness.peer.closeCalls.Load(),
			harness.channel.closeCalls.Load(),
		)
	}
	_ = harness.attempt.Close()
	if harness.peer.closeCalls.Load() != 1 || harness.channel.closeCalls.Load() != 1 {
		t.Fatal("idempotent Close repeated authenticated installation teardown")
	}
}
