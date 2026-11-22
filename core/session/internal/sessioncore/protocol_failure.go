package sessioncore

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func RevisionOperationCause(code uint16) (error, bool) {
	switch code {
	case contentflow.RevisionCodeStale:
		return content.ErrRevisionStale, true
	case contentflow.RevisionCodeNotFound:
		return content.ErrRevisionNotFound, true
	case contentflow.RevisionCodeUnreadable:
		return content.ErrRevisionUnreadable, true
	case contentflow.RevisionCodeUnsupportedStability:
		return content.ErrUnsupportedStability, true
	case contentflow.RevisionCodeQuota:
		return content.ErrQuotaExceeded, true
	case contentflow.RevisionCodeLeaseExpired:
		return content.ErrLeaseExpired, true
	case contentflow.RevisionCodeDrift:
		return content.ErrRevisionDrift, true
	case contentflow.RevisionCodeInvalidLease:
		return content.ErrInvalidLease, true
	default:
		return nil, false
	}
}

func RequestProvenNotDelivered(err error) bool {
	return protocolsession.IsNotSent(err)
}

func rpcDeliveryError(runtime *Runtime, notDelivered error, cause error) error {
	if runtime.ctx.Err() != nil {
		return errors.Join(notDelivered, cause, session.ErrRuntimeClosed, runtime.Err())
	}
	return errors.Join(notDelivered, cause)
}

func (runtime *Runtime) failRPCOperationAuthority() error {
	_ = runtime.operations.TerminateLocal()
	runtime.terminateRuntimeFailed(errRPCOperationAuthority)
	return errRPCOperationAuthority
}

func (runtime *Runtime) reconcileLocalCancel(id protocolsession.OperationID) (bool, error) {
	transitioned, err := runtime.operations.CancelOperation(id)
	if err == nil {
		return transitioned, nil
	}
	_ = runtime.operations.TerminateLocal()
	runtime.terminateRuntimeFailed(err)
	return false, err
}

func protocolOperationCause(err error) sessionobs.ProtocolOperationCause {
	switch wrapped := err.(type) { //nolint:errorlint // Inspect one node at a time so every joined fault is classified.
	case interface{ Unwrap() []error }:
		cause := sessionobs.ProtocolOperationCauseNone
		for _, component := range wrapped.Unwrap() {
			cause = cause.Merge(protocolOperationCause(component))
		}
		if cause != sessionobs.ProtocolOperationCauseNone {
			return cause
		}
	case interface{ Unwrap() error }:
		if inner := wrapped.Unwrap(); inner != nil {
			return protocolOperationCause(inner)
		}
	}
	switch {
	case err == nil:
		return sessionobs.ProtocolOperationCauseNone
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, contentflow.ErrFragmentInactivity),
		errors.Is(err, contentflow.ErrBlockResponseInactivity):
		return sessionobs.ProtocolOperationCauseDeadline
	case err == context.Canceled: //nolint:errorlint // Only the canonical leaf proves an ordinary canceled wait.
		return sessionobs.ProtocolOperationCauseCanceled
	case errors.Is(err, session.ErrRuntimeClosed):
		return sessionobs.ProtocolOperationCauseRuntimeClosed
	case errors.Is(err, ErrLaneUnavailable):
		return sessionobs.ProtocolOperationCauseLaneUnavailable
	case errors.Is(err, protocolsession.ErrWriterStopped):
		return sessionobs.ProtocolOperationCauseWriterStopped
	case err == session.ErrOperationMissing: //nolint:errorlint // An opaque Is match cannot prove that this operation ended.
		return sessionobs.ProtocolOperationCauseOperationClosed
	default:
		return sessionobs.ProtocolOperationCauseProtocolFailure
	}
}
