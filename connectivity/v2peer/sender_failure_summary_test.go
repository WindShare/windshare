package v2peer

import (
	"context"
	"errors"
	"testing"
	"time"

	transportwebrtc "github.com/windshare/windshare/transport/webrtc"
)

func TestSenderFailureSummaryPreservesAdmissionWaitAndRemoteClose(t *testing.T) {
	now := time.Unix(1, 0)
	collector := &senderObservationCollector{}
	factory := mustTestFactoryWithSenderCollector(t, collector, Config{Now: func() time.Time { return now }})
	binding := testBinding(123)
	recorder := newSenderAttemptRecorder(factory, newTestPeerSession(122).sessionID, binding)
	recorder.begin()
	recorder.negotiationDeadlineArmed()
	recorder.complete(SenderAttemptAnswerCreated, SenderCandidateCounts{}, nil)
	recorder.complete(SenderAttemptAnswerSent, SenderCandidateCounts{}, nil)
	recorder.dataChannelOpened(SenderCandidateCounts{})
	now = now.Add(18716 * time.Millisecond)
	primary := errors.Join(errChannelAdmission, transportwebrtc.ErrRemoteClosed)
	recorder.fail(attemptFailure(errors.Join(primary, context.Canceled), primary, false))
	events := collector.forAttempt(binding.AttemptID)
	failure := events[len(events)-1].Failure
	if failure == nil || failure.LastCompletedStage != SenderAttemptAdmissionDeadlineArmed ||
		failure.FailedAtStage != SenderAttemptLaneHelloAuthenticated || failure.StageElapsedMillis != 18716 ||
		failure.DeadlineExpired || failure.Termination.Initiator != "remote" || failure.Termination.Cause != "remote_closed" {
		t.Fatalf("failure summary=%+v", failure)
	}
}

func TestSenderFailureSummaryDistinguishesOwnDeadline(t *testing.T) {
	now := time.Unix(1, 0)
	collector := &senderObservationCollector{}
	factory := mustTestFactoryWithSenderCollector(t, collector, Config{Now: func() time.Time { return now }})
	binding := testBinding(126)
	recorder := newSenderAttemptRecorder(factory, newTestPeerSession(125).sessionID, binding)
	recorder.begin()
	recorder.negotiationDeadlineArmed()
	now = now.Add(factory.negotiationBudget)
	recorder.phaseDeadlineExpired(PeerAttemptPhaseNegotiation)
	recorder.fail(attemptFailure(ErrPeerNegotiationTimeout, ErrPeerNegotiationTimeout, false))
	events := collector.forAttempt(binding.AttemptID)
	failure := events[len(events)-1].Failure
	if !failure.DeadlineExpired || failure.Termination.Cause != "negotiation_timeout" ||
		failure.LastCompletedStage != SenderAttemptOfferReceived || failure.StageElapsedMillis != uint64(factory.negotiationBudget.Milliseconds()) {
		t.Fatalf("deadline summary=%+v", failure)
	}
}
