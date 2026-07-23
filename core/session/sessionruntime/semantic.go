package sessionruntime

import (
	"strings"
	"time"

	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

func newReceiverSenderControlValidator(
	peer protocolsession.SenderControlSemanticValidator,
) (protocolsession.SenderControlSemanticValidator, error) {
	validatePeer := protocolsession.SenderControlSemanticValidatorFunc(func(
		kind protocolsession.MessageKind,
		operationID protocolsession.OperationID,
		semantic []byte,
	) error {
		if peer == nil {
			// A relay-only receiver must fail closed if a sender unexpectedly
			// introduces peer signaling that no connectivity owner can decode.
			return protocolsession.ErrControlSemantic
		}
		return peer.ValidateSenderControl(kind, operationID, semantic)
	})
	return protocolsession.NewSenderControlSemanticRegistry(
		protocolsession.SenderControlSemanticRule{
			Kind: protocolsession.MessageCatalogResult,
			Validate: func(_ protocolsession.MessageKind, _ protocolsession.OperationID, semantic []byte) error {
				_, err := catalogflow.DecodeCatalogResult(semantic)
				return err
			},
		},
		protocolsession.SenderControlSemanticRule{
			Kind: protocolsession.MessageOpenResults,
			Validate: func(_ protocolsession.MessageKind, _ protocolsession.OperationID, semantic []byte) error {
				return contentflow.ValidateOpenResults(semantic)
			},
		},
		protocolsession.SenderControlSemanticRule{
			Kind: protocolsession.MessageOperationComplete,
			Validate: func(_ protocolsession.MessageKind, _ protocolsession.OperationID, semantic []byte) error {
				_, err := contentflow.DecodeOperationComplete(semantic)
				return err
			},
		},
		protocolsession.SenderControlSemanticRule{
			Kind: protocolsession.MessageLeaseResult,
			Validate: func(_ protocolsession.MessageKind, _ protocolsession.OperationID, semantic []byte) error {
				return contentflow.ValidateLeaseResult(semantic)
			},
		},
		protocolsession.SenderControlSemanticRule{
			Kind: protocolsession.MessageLaneAttach,
			Validate: func(_ protocolsession.MessageKind, operationID protocolsession.OperationID, semantic []byte) error {
				_, err := decodeLaneGrant(semantic, operationID)
				return err
			},
		},
		protocolsession.SenderControlSemanticRule{Kind: protocolsession.MessagePeerAnswer, Validate: validatePeer},
		protocolsession.SenderControlSemanticRule{Kind: protocolsession.MessagePeerCandidate, Validate: validatePeer},
	)
}

type RemoteOperationFailureSnapshot struct {
	scope      uint8
	code       uint16
	retryable  bool
	retryAfter time.Duration
	message    string
}

func (failure RemoteOperationFailureSnapshot) Scope() uint8              { return failure.scope }
func (failure RemoteOperationFailureSnapshot) Code() uint16              { return failure.code }
func (failure RemoteOperationFailureSnapshot) Retryable() bool           { return failure.retryable }
func (failure RemoteOperationFailureSnapshot) RetryAfter() time.Duration { return failure.retryAfter }
func (failure RemoteOperationFailureSnapshot) Message() string           { return failure.message }

type RemoteOperationError struct {
	failure RemoteOperationFailureSnapshot
}

func (RemoteOperationError) Error() string { return "sender rejected the operation" }

func (err RemoteOperationError) Failure() RemoteOperationFailureSnapshot { return err.failure }

// NewRemoteOperationError materializes an immutable diagnostic value from an
// already-owned snapshot. It carries no terminal authority; consumers obtain
// session consequences only from a sealed operation termination.
func NewRemoteOperationError(failure RemoteOperationFailureSnapshot) RemoteOperationError {
	return RemoteOperationError{failure: failure}
}

func decodeRemoteOperationFailure(
	message protocolsession.Message,
) (RemoteOperationFailureSnapshot, error) {
	body, err := protocolsession.SenderControlSemanticBody(message)
	if err != nil {
		return RemoteOperationFailureSnapshot{}, err
	}
	failure, err := protocolsession.DecodeOperationFailure(body)
	if err != nil {
		return RemoteOperationFailureSnapshot{}, err
	}
	return RemoteOperationFailureSnapshot{
		scope: failure.Scope, code: failure.Code, retryable: failure.Retryable,
		retryAfter: failure.RetryAfter, message: strings.Clone(failure.Message),
	}, nil
}

func remoteOperationError(message protocolsession.Message) error {
	failure, err := decodeRemoteOperationFailure(message)
	if err != nil {
		return err
	}
	return RemoteOperationError{failure: failure}
}

func remoteOperationErrorFor(message protocolsession.Message, expectedScope uint8) error {
	failure, err := decodeRemoteOperationFailure(message)
	if err != nil {
		return transfer.NewSessionFailure(err)
	}
	if failure.Scope() != expectedScope {
		return transfer.NewSessionFailure(protocolsession.ErrInvalidOperationFailure)
	}
	return NewRemoteOperationError(failure)
}
