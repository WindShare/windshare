package receiversession

import sessionobs "github.com/windshare/windshare/core/session/observation"

// Local ownership alone cannot make a joined fault benign. The operation's
// immutable terminal diagnostics and the RPC evidence must both agree before a
// canceled wait can be observed as an ordinary end.
func receiverPeerProtocolOutcome(termination PeerTermination, cause sessionobs.ProtocolOperationCause) (sessionobs.ProtocolOperationStage, sessionobs.ProtocolOperationCause) {
	localStop := termination.Authority() == PeerTerminalAuthorityLocal &&
		termination.Severity() == PeerTerminalOperationOnly &&
		(termination.ConsequenceProvenance() == PeerProvenanceLocalExplicitStop ||
			termination.ConsequenceProvenance() == PeerProvenanceLocalContextEnded)
	if !localStop {
		failure := sessionobs.ProtocolOperationCauseProtocolFailure
		if termination.Severity() == PeerTerminalSessionUnavailable {
			failure = sessionobs.ProtocolOperationCauseRuntimeClosed
		}
		cause = cause.Merge(failure)
	}
	for _, diagnostic := range termination.diagnostics.components[:termination.diagnostics.count] {
		component := sessionobs.ProtocolOperationCauseProtocolFailure
		switch diagnostic.Code() {
		case PeerDiagnosticContextCanceled:
			component = sessionobs.ProtocolOperationCauseCanceled
		case PeerDiagnosticOperationMissing:
			component = sessionobs.ProtocolOperationCauseOperationClosed
		case PeerDiagnosticRuntimeClosed:
			component = sessionobs.ProtocolOperationCauseRuntimeClosed
		}
		cause = cause.Merge(component)
	}
	if localStop && cause.IsLifecycle() {
		return sessionobs.ProtocolOperationReceiverEnded, cause
	}
	return sessionobs.ProtocolOperationReceiverFailed, cause
}
