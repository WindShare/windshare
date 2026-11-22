package protocolsession

import "errors"

var ErrControlSemantic = errors.New("session control semantic body is invalid")

// SenderMessageValidator runs inside the lane pump, after authenticated
// decryption and before operation admission. A valid envelope can still carry
// an invalid operation result, which must retire the correct operation.
func SenderMessageValidator(semantic SenderControlSemanticValidator) InboundMessageValidatorFunc {
	return func(message Message) (InboundValidationResult, error) {
		if message.IsData() {
			return InboundValidationResult{}, nil
		}
		if !isSenderControl(message.kind) {
			return InboundValidationResult{}, ErrInvalidDirection
		}
		operationID, _ := message.OperationID()
		if err := validateSenderControlSemantic(semantic, message.kind, operationID, message.body); err != nil {
			switch message.kind {
			case MessageOperationError:
				return authenticatedOperationViolationResult(AuthenticatedOperationViolationMalformedFailure), nil
			case MessagePeerAnswer, MessagePeerCandidate:
				return authenticatedOperationViolationResult(AuthenticatedOperationViolationMalformedPeerControl), nil
			}
			return InboundValidationResult{}, err
		}
		return InboundValidationResult{}, nil
	}
}

func ValidateReceiverMessage(message Message) (InboundValidationResult, error) {
	if message.kind.isSessionControl() {
		return InboundValidationResult{}, ValidateSessionControlBody(message.kind, message.body)
	}
	// The operation table enforces request direction and the selected request
	// handler owns decoding the request body.
	return InboundValidationResult{}, nil
}

func isSenderControl(kind MessageKind) bool {
	switch kind {
	case MessageCatalogResult, MessageOpenResults, MessageOperationError, MessageSessionTerminal,
		MessageLaneAttach, MessageScanProgress, MessageOperationComplete, MessageLeaseResult,
		MessagePeerAnswer, MessagePeerCandidate, MessagePeerPathControl:
		return true
	default:
		return false
	}
}
