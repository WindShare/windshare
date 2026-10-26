package sendersession

import (
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	sessionobs "github.com/windshare/windshare/core/session/observation"
)

type senderTerminalObservers struct {
	send    sessionobs.SenderTerminalSendObserver
	session sessionobs.SenderSessionTerminalObserver
}

func newSenderTerminalObservers(
	send sessionobs.SenderTerminalSendObserver,
	session sessionobs.SenderSessionTerminalObserver,
) *senderTerminalObservers {
	if send == nil && session == nil {
		return nil
	}
	return &senderTerminalObservers{send: send, session: session}
}

func (observers *senderTerminalObservers) sendObserver() sessionobs.SenderTerminalSendObserver {
	if observers == nil {
		return nil
	}
	return observers.send
}

func (observers *senderTerminalObservers) sessionObserver() sessioncore.TerminationObserver {
	if observers == nil || observers.session == nil {
		return nil
	}
	return observers
}

func (observers *senderTerminalObservers) ObserveTermination(event sessioncore.Termination) {
	trigger, provenance, valid := senderTerminalPair(event.Cause)
	if !valid {
		return
	}
	observers.session.ObserveSenderSessionTerminated(sessionobs.SenderSessionTerminated{
		ProtocolSessionID: event.ProtocolSessionID, Trigger: trigger, Provenance: provenance, Failure: event.Failure,
	})
}

func senderTerminalPair(cause sessioncore.TerminationCause) (
	sessionobs.SenderSessionTerminalTrigger,
	sessionobs.SenderSessionTerminalProvenance,
	bool,
) {
	switch cause {
	case sessioncore.TerminationGracefulStop:
		return sessionobs.SenderSessionTerminalTriggerGracefulStop, sessionobs.SenderSessionTerminalProvenanceNormalStop, true
	case sessioncore.TerminationForcedClose:
		return sessionobs.SenderSessionTerminalTriggerForcedClose, sessionobs.SenderSessionTerminalProvenanceCallerStop, true
	case sessioncore.TerminationPeerTerminal:
		return sessionobs.SenderSessionTerminalTriggerPeerTerminal, sessionobs.SenderSessionTerminalProvenanceRemoteClose, true
	case sessioncore.TerminationPathsExhausted:
		return sessionobs.SenderSessionTerminalTriggerPathsExhausted, sessionobs.SenderSessionTerminalProvenanceLaneRetirement, true
	case sessioncore.TerminationFailed:
		return sessionobs.SenderSessionTerminalTriggerRuntimeFailed, sessionobs.SenderSessionTerminalProvenanceLocalFault, true
	default:
		return "", "", false
	}
}
