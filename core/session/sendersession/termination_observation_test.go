package sendersession

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/diagnosticerror"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestSenderTerminalAdapterPreservesCauseAndFailureEvidence(t *testing.T) {
	for _, test := range []struct {
		cause      sessioncore.TerminationCause
		trigger    sessionobs.SenderSessionTerminalTrigger
		provenance sessionobs.SenderSessionTerminalProvenance
	}{
		{sessioncore.TerminationGracefulStop, sessionobs.SenderSessionTerminalTriggerGracefulStop, sessionobs.SenderSessionTerminalProvenanceNormalStop},
		{sessioncore.TerminationForcedClose, sessionobs.SenderSessionTerminalTriggerForcedClose, sessionobs.SenderSessionTerminalProvenanceCallerStop},
		{sessioncore.TerminationPeerTerminal, sessionobs.SenderSessionTerminalTriggerPeerTerminal, sessionobs.SenderSessionTerminalProvenanceRemoteClose},
		{sessioncore.TerminationPathsExhausted, sessionobs.SenderSessionTerminalTriggerPathsExhausted, sessionobs.SenderSessionTerminalProvenanceLaneRetirement},
		{sessioncore.TerminationFailed, sessionobs.SenderSessionTerminalTriggerRuntimeFailed, sessionobs.SenderSessionTerminalProvenanceLocalFault},
	} {
		t.Run(string(test.trigger), func(t *testing.T) {
			observed := make(chan sessionobs.SenderSessionTerminated, 1)
			adapter := newSenderTerminalObservers(nil, sessionobs.SenderSessionTerminalObserverFunc(func(event sessionobs.SenderSessionTerminated) {
				observed <- event
			}))
			event := sessioncore.Termination{
				ProtocolSessionID: id16[protocolsession.ProtocolSessionID](31), Cause: test.cause,
			}
			if test.cause == sessioncore.TerminationFailed {
				event.Failure = diagnosticerror.Capture(errors.New("original component failure"), "component")
			}
			adapter.sessionObserver().ObserveTermination(event)
			got := awaitSenderSessionTermination(t, observed)
			assertSenderSessionTermination(t, got, event.ProtocolSessionID, test.trigger, test.provenance)
			if got.Failure.Source() != event.Failure.Source() || len(got.Failure.Nodes()) != len(event.Failure.Nodes()) {
				t.Fatal("sender adapter lost the original failure snapshot")
			}
			adapter.sessionObserver().ObserveTermination(sessioncore.Termination{})
			assertNoSenderSessionTermination(t, observed)
		})
	}
	onlySend := newSenderTerminalObservers(sessionobs.SenderTerminalSendObserverFunc(func(sessionobs.SenderTerminalSendObserved) {}), nil)
	if onlySend.sessionObserver() != nil {
		t.Fatal("disabled session diagnostics installed an error-inspecting observer")
	}
}

func TestTerminalSendObserverPanicCannotEscape(t *testing.T) {
	var observations int
	observer := sessionobs.SenderTerminalSendObserverFunc(func(event sessionobs.SenderTerminalSendObserved) {
		observations++
		if event.Decision != sessionobs.SenderTerminalSendDecisionFailed || !event.Settled {
			t.Errorf("terminal consequence=%+v", event)
		}
		panic("diagnostic consumer failed")
	})
	observeSenderTerminalSend(observer, sessioncore.TerminalSendObservation{
		ProtocolSessionID: id16[protocolsession.ProtocolSessionID](32),
		Lane:              session.LaneIdentity{ID: 1},
		Settled:           true, Outcome: protocolsession.SendOutcomeDropped, TransportDisposition: framechannel.SendAccepted,
	})
	if observations != 1 {
		t.Fatalf("observations=%d", observations)
	}
}
