package v2peer

import (
	"fmt"

	"github.com/windshare/windshare/connectivity/v2signal"
)

func (execution *receiverExecution) acceptRemoteCandidate(body []byte) receiverWorkflowResult {
	candidate, err := v2signal.DecodeCandidate(body)
	if err != nil {
		return receiverWorkflowDiagnostic(err)
	}
	if err := execution.attempt.binding.RequireSame(candidate.Binding); err != nil {
		return receiverWorkflowUnsafe(
			err,
			ReceiverProvenanceAuthenticatedCandidateBindingMismatch,
		)
	}
	if execution.remoteCandidates >= execution.attempt.factory.maxCandidates {
		return receiverWorkflowDiagnostic(errCandidateLimit)
	}
	execution.remoteCandidates++
	if !execution.answerSeen {
		execution.queuedCandidates = append(execution.queuedCandidates, candidate)
		return receiverWorkflowResult{}
	}
	if err := execution.attempt.peer.AddICECandidate(candidateInit(candidate)); err != nil {
		return receiverWorkflowDiagnostic(fmt.Errorf("add remote ICE candidate: %w", err))
	}
	return receiverWorkflowResult{}
}
