package v2signal

import "github.com/windshare/windshare/core/session/protocolsession"

// ReceiverControlValidator is injected into sessionruntime by a Go receiver
// that enables peer connectivity. Signatures remain core-owned while this
// transport module owns the typed SDP/ICE schema, avoiding either import cycle.
type ReceiverControlValidator struct {
	MaximumCandidates int
}

func (validator ReceiverControlValidator) BeginOperationContinuation(
	requestKind protocolsession.MessageKind,
	canonicalRequestBody []byte,
) (protocolsession.OperationContinuationAuthority, bool, error) {
	return OperationContinuationClassifier(validator).BeginOperationContinuation(
		requestKind,
		canonicalRequestBody,
	)
}

func (validator ReceiverControlValidator) ClassifyUnboundOperationContinuation(
	kind protocolsession.MessageKind,
	canonicalBody []byte,
) (protocolsession.OperationContinuationScope, bool, error) {
	return OperationContinuationClassifier(validator).ClassifyUnboundOperationContinuation(kind, canonicalBody)
}

func (ReceiverControlValidator) ValidateSenderControl(
	kind protocolsession.MessageKind,
	operationID protocolsession.OperationID,
	semantic []byte,
) error {
	if operationID.IsZero() {
		return ErrInvalidSignal
	}
	switch kind {
	case protocolsession.MessagePeerAnswer:
		_, err := DecodeAnswer(semantic)
		return err
	case protocolsession.MessagePeerCandidate:
		_, err := DecodeCandidate(semantic)
		return err
	default:
		return ErrInvalidSignal
	}
}

var _ protocolsession.SenderControlSemanticValidator = ReceiverControlValidator{}
var _ protocolsession.OperationContinuationClassifier = ReceiverControlValidator{}
