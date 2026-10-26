package receiversession

import (
	"errors"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"strings"
	"time"
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

func (failure RemoteOperationFailureSnapshot) Scope() uint8 { return failure.scope }

func (failure RemoteOperationFailureSnapshot) Code() uint16 { return failure.code }

func (failure RemoteOperationFailureSnapshot) Retryable() bool { return failure.retryable }

func (failure RemoteOperationFailureSnapshot) RetryAfter() time.Duration { return failure.retryAfter }

func (failure RemoteOperationFailureSnapshot) Message() string { return failure.message }

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

func remoteRevisionOperationError(message protocolsession.Message) error {
	failure, err := decodeRemoteOperationFailure(message)
	if err != nil {
		return sessionProtocolBoundaryError(err)
	}
	if failure.Scope() != protocolsession.OperationScopeRevision {
		return sessionProtocolBoundaryError(protocolsession.ErrInvalidOperationFailure)
	}
	return revisionOperationError(failure)
}

func remoteDirectoryOperationError(message protocolsession.Message) error {
	failure, err := decodeRemoteOperationFailure(message)
	if err != nil {
		return sessionProtocolBoundaryError(err)
	}
	if failure.Scope() != protocolsession.OperationScopeDirectory {
		return sessionProtocolBoundaryError(protocolsession.ErrInvalidOperationFailure)
	}
	return catalogDirectoryBoundaryError(NewRemoteOperationError(failure))
}

func blockRequestOperationError(message protocolsession.Message) error {
	failure, err := decodeRemoteOperationFailure(message)
	if err != nil {
		return sessionProtocolBoundaryError(err)
	}
	remote := NewRemoteOperationError(failure)
	switch failure.Scope() {
	case protocolsession.OperationScopeBlock:
		return isolatedBlockOperationError(remote)
	case protocolsession.OperationScopeRevision:
		return revisionOperationError(failure)
	default:
		return sessionProtocolBoundaryError(protocolsession.ErrInvalidOperationFailure)
	}
}

func isolatedBlockOperationError(remote RemoteOperationError) error {
	// FetchBlock reaches this branch only after the transport operation has
	// terminally rejected this block demand. The peer session remains valid;
	// bind the failure to the current file so sibling revisions can continue.
	return sourceBoundaryError(transferfault.SourceUnavailable, remote)
}

func revisionOperationError(failure RemoteOperationFailureSnapshot) error {
	return classifyRevisionFailure(NewRemoteOperationError(failure), failure.Code(), failure.Retryable())
}

func remoteRevisionFailureError(remote *RemoteRevisionError) error {
	if remote == nil {
		return sessionProtocolBoundaryError(protocolsession.ErrInvalidOperationFailure)
	}
	failure := remote.Failure()
	return classifyRevisionFailure(
		remote, failure.Code, failure.Retryable,
	)
}

func classifyRevisionFailure(diagnostic error, code uint16, retryable bool) error {
	cause, ok := sessioncore.RevisionOperationCause(code)
	if !ok {
		return sessionProtocolBoundaryError(errors.Join(protocolsession.ErrInvalidOperationFailure, diagnostic))
	}
	revisionFailure := errors.Join(diagnostic, cause)
	switch code {
	case contentflow.RevisionCodeDrift:
		return sourceBoundaryError(transferfault.SourceRevisionInvalidated, revisionFailure)
	case contentflow.RevisionCodeStale:
		// Retryability cannot turn an obsolete candidate into authority to retire
		// an already-opened revision's download progress.
		return sourceBoundaryError(transferfault.SourceRevisionChanged, revisionFailure)
	case contentflow.RevisionCodeNotFound,
		contentflow.RevisionCodeUnreadable,
		contentflow.RevisionCodeUnsupportedStability:
		if !retryable {
			return sourceBoundaryError(transferfault.SourcePermanent, revisionFailure)
		}
	}
	return sourceBoundaryError(transferfault.SourceUnavailable, revisionFailure)
}

func sessionProtocolBoundaryError(cause error) error {
	value, _ := transferfault.NewSession(
		transferfault.ScopeSessionTerminal, transferfault.SessionProtocol,
	)
	return transferfault.Wrap(value, cause)
}

func sessionTransportBoundaryError(cause error) error {
	value, _ := transferfault.NewSession(
		transferfault.ScopeSessionTerminal, transferfault.SessionTransport,
	)
	return transferfault.Wrap(value, cause)
}

func dependencyBoundaryError(cause error) error {
	return transferfault.Wrap(transferfault.DependencyContractFault(), cause)
}

func catalogDirectoryBoundaryError(cause error) error {
	value, _ := transferfault.NewCatalog(
		transferfault.ScopeDirectoryLocal, transferfault.CatalogUnavailable,
	)
	return transferfault.Wrap(value, cause)
}

func sourceBoundaryError(code transferfault.SourceCode, cause error) error {
	value, _ := transferfault.NewSource(transferfault.ScopeFileLocal, code)
	return transferfault.Wrap(value, cause)
}
