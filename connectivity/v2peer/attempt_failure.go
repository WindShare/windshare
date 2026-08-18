package v2peer

import (
	"context"
	"errors"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func (attempt *peerAttempt) deliverFailure(
	ctx context.Context,
	result error,
	operationCanceled bool,
) error {
	if result == nil || operationCanceled || attempt.recorder.admitted() ||
		attempt.authenticatedSettlementBegan() || errors.Is(result, context.Canceled) {
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
	case errors.Is(err, ErrPeerNegotiationTimeout), errors.Is(err, ErrPeerAdmissionTimeout):
		return protocolsession.PeerOperationCodeTimeout, peerTimeoutFailureMessage
	case errors.Is(err, errCandidateLimit):
		return protocolsession.PeerOperationCodeCandidates, peerCandidateLimitMessage
	case errors.Is(err, errChannelAdmission):
		return protocolsession.PeerOperationCodeAdmission, peerAdmissionFailureMessage
	default:
		return protocolsession.PeerOperationCodeNegotiation, peerNegotiationFailureMessage
	}
}

func senderOperationAttemptFailure(code uint16, message string, cause error) SenderAttemptFailure {
	operation := &PeerOperationFailure{Code: code, Message: message}
	typedCode := typedPeerErrorForOperationCode(code)
	if isSenderSignalingContractFailure(cause) {
		typedCode = TypedPeerErrorSignaling
	}
	return SenderAttemptFailure{
		Scope: AttemptFailureScopeAttempt, TypedPeerErrorCode: typedCode,
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
		return senderOperationAttemptFailure(code, message, primary)
	default:
		return SenderAttemptFailure{
			Scope: AttemptFailureScopeAttempt, TypedPeerErrorCode: TypedPeerErrorUnexpected,
			Message: peerUnexpectedFailureMessage,
		}
	}
}

func senderAttemptCancelledFailure() SenderAttemptFailure {
	return SenderAttemptFailure{
		Scope: AttemptFailureScopeAttempt, TypedPeerErrorCode: TypedPeerErrorCancelled,
		Message: peerAttemptCancelledMessage,
	}
}

func senderRuntimeStoppedFailure() SenderAttemptFailure {
	return SenderAttemptFailure{
		Scope: AttemptFailureScopeSession, TypedPeerErrorCode: TypedPeerErrorStopped,
		Message: peerRuntimeStoppedMessage,
	}
}

func senderEvidenceCapacityFailure() SenderAttemptFailure {
	return SenderAttemptFailure{
		Scope: AttemptFailureScopeSession, TypedPeerErrorCode: TypedPeerErrorStopped,
		Message: peerEvidenceCapacityFailureMessage,
	}
}

func rejectionForEvent(event handlerEvent, cause error) *peerOperationRejection {
	if event.rejection != nil {
		return event.rejection
	}
	if event.kind == handlerCandidate {
		return &peerOperationRejection{
			code: protocolsession.PeerOperationCodeCandidates, message: peerCandidateFailureMessage, cause: cause,
		}
	}
	return &peerOperationRejection{
		code: protocolsession.PeerOperationCodeNegotiation, message: peerNegotiationFailureMessage, cause: cause,
	}
}

func offerBindingForEvent(event handlerEvent) *v2signal.Binding {
	if event.kind == handlerOffer && event.offer.Binding.Validate() == nil {
		binding := event.offer.Binding
		return &binding
	}
	if event.kind == handlerReject && event.offerBinding != nil && event.offerBinding.Validate() == nil {
		binding := *event.offerBinding
		return &binding
	}
	return nil
}

func (handler *senderHandler) rejectOperation(
	ctx context.Context,
	operation peerOperation,
	rejection *peerOperationRejection,
	binding *v2signal.Binding,
) error {
	if rejection == nil {
		rejection = &peerOperationRejection{
			code:    protocolsession.PeerOperationCodeNegotiation,
			message: peerNegotiationFailureMessage,
			cause:   ErrNegotiation,
		}
	}
	handler.mu.Lock()
	attempt := handler.attempts[operation]
	handler.mu.Unlock()
	if attempt != nil {
		attempt.stop(rejection)
	}
	claim := evidenceClaim{}
	if binding != nil {
		claim = handler.claimRejectedOffer(operation, *binding)
		if claim.acquired {
			failure := senderOperationAttemptFailure(rejection.code, rejection.message, rejection)
			if claim.sessionTerminal {
				failure = senderEvidenceCapacityFailure()
			}
			handler.emitRejectedOfferTerminal(*binding, failure)
		}
	}
	if claim.sessionTerminal || handler.evidenceSessionTerminal() {
		handler.factory.reportDiagnostic(PeerDiagnosticSenderAttempt, PeerDiagnosticEvidenceCapacity)
		return ErrEvidenceIdentityCapacity
	}
	failureContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureDeliveryTimeout)
	err := handler.session.FailPeerOperation(
		failureContext, operation.id, rejection.code, rejection.message,
	)
	cancel()
	if err != nil {
		handler.factory.reportDiagnostic(PeerDiagnosticSenderAttempt, PeerDiagnosticCleanupResidue)
	}
	return nil
}

func isSenderSignalingContractFailure(err error) bool {
	return errors.Is(err, ErrProtocol) || errors.Is(err, v2signal.ErrInvalidSignal) ||
		errors.Is(err, v2signal.ErrNonCanonicalSignal) || errors.Is(err, v2signal.ErrSignalBinding)
}

func (handler *senderHandler) terminalizeUnstartedOffer(
	event handlerEvent,
	failure SenderAttemptFailure,
) error {
	binding := offerBindingForEvent(event)
	if binding == nil {
		return nil
	}
	claim := handler.claimRejectedOffer(event.operation, *binding)
	if claim.acquired {
		if claim.sessionTerminal {
			failure = senderEvidenceCapacityFailure()
		}
		handler.emitRejectedOfferTerminal(*binding, failure)
	}
	if claim.sessionTerminal {
		handler.factory.reportDiagnostic(PeerDiagnosticSenderAttempt, PeerDiagnosticEvidenceCapacity)
		return ErrEvidenceIdentityCapacity
	}
	return nil
}

func (handler *senderHandler) emitUnstartedOfferTerminal(
	event handlerEvent,
	failure SenderAttemptFailure,
) {
	binding := offerBindingForEvent(event)
	if binding != nil {
		handler.emitRejectedOfferTerminal(*binding, failure)
	}
}

func (handler *senderHandler) emitRejectedOfferTerminal(
	binding v2signal.Binding,
	failure SenderAttemptFailure,
) {
	recorder := newSenderAttemptRecorder(handler.factory, handler.session.ProtocolSessionID(), binding)
	recorder.begin()
	recorder.fail(failure)
}

func (handler *senderHandler) unstartedOfferFailure() SenderAttemptFailure {
	handler.mu.Lock()
	runtimeStopped := handler.stopping ||
		(handler.runtimeContext != nil && handler.runtimeContext.Err() != nil)
	handler.mu.Unlock()
	if runtimeStopped {
		return senderRuntimeStoppedFailure()
	}
	return senderAttemptCancelledFailure()
}

func (handler *senderHandler) evidenceSessionTerminal() bool {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.evidenceAuthority.terminal
}
