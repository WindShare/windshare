package v2peer

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func (attempt *peerAttempt) deliverFailure(
	ctx context.Context,
	result error,
	operationCanceled bool,
) error {
	if result == nil || operationCanceled || attempt.recorder.admitted() || errors.Is(result, context.Canceled) {
		return result
	}
	code, message := peerFailure(result)
	failureContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureDeliveryTimeout)
	defer cancel()
	return errors.Join(result, attempt.config.session.FailPeerOperation(
		failureContext,
		attempt.config.operation,
		code,
		message,
	))
}

func peerFailure(err error) (uint16, string) {
	if rejected, ok := errors.AsType[*peerOperationRejection](err); ok {
		return rejected.code, rejected.message
	}
	switch {
	case errors.Is(err, errAttemptTimeout):
		return protocolsession.PeerOperationCodeTimeout, peerTimeoutFailureMessage
	case errors.Is(err, errCandidateLimit):
		return protocolsession.PeerOperationCodeCandidates, peerCandidateLimitMessage
	case errors.Is(err, errChannelAdmission):
		return protocolsession.PeerOperationCodeAdmission, peerAdmissionFailureMessage
	default:
		return protocolsession.PeerOperationCodeNegotiation, peerNegotiationFailureMessage
	}
}

func senderOperationAttemptFailure(code uint16, message string) SenderAttemptFailure {
	operation := &PeerOperationFailure{Code: code, Message: message}
	return SenderAttemptFailure{
		Scope: AttemptFailureScopeAttempt, TypedCode: typedPeerErrorForOperationCode(code),
		Message: message, Operation: operation,
	}
}

func attemptFailure(result, primary error, operationCanceled bool) SenderAttemptFailure {
	switch {
	case operationCanceled || errors.Is(primary, errAnswerDropped) || errors.Is(primary, errCandidateDropped):
		return senderAttemptCancelledFailure()
	case errors.Is(primary, context.Canceled) || errors.Is(primary, context.DeadlineExceeded):
		return senderRuntimeStoppedFailure()
	case result != nil:
		code, message := peerFailure(result)
		return senderOperationAttemptFailure(code, message)
	default:
		return SenderAttemptFailure{
			Scope: AttemptFailureScopeAttempt, TypedCode: TypedPeerErrorUnexpected,
			Message: peerUnexpectedFailureMessage,
		}
	}
}

func senderAttemptCancelledFailure() SenderAttemptFailure {
	return SenderAttemptFailure{
		Scope: AttemptFailureScopeAttempt, TypedCode: TypedPeerErrorCancelled,
		Message: peerAttemptCancelledMessage,
	}
}

func senderRuntimeStoppedFailure() SenderAttemptFailure {
	return SenderAttemptFailure{
		Scope: AttemptFailureScopeSession, TypedCode: TypedPeerErrorStopped,
		Message: peerRuntimeStoppedMessage,
	}
}

func senderEvidenceCapacityFailure() SenderAttemptFailure {
	return SenderAttemptFailure{
		Scope: AttemptFailureScopeSession, TypedCode: TypedPeerErrorStopped,
		Message: peerEvidenceCapacityFailureMessage,
	}
}
