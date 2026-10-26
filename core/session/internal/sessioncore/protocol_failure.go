package sessioncore

import (
	"context"
	"errors"
	"fmt"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type rpcRequestSendError struct {
	outcome  protocolsession.SendOutcome
	admitted bool
	cause    error
}

func (failure *rpcRequestSendError) Error() string {
	return fmt.Sprintf("send operation request: %v", failure.cause)
}

func (failure *rpcRequestSendError) Unwrap() error { return failure.cause }

func RequestProvenNotDelivered(err error) bool {
	var failure *rpcRequestSendError
	return errors.As(err, &failure) && failure.outcome == protocolsession.SendOutcomeDropped && !failure.admitted
}

func newRPCRequestSendError(outcome protocolsession.SendOutcome, admitted bool, cause error) error {
	if cause == nil {
		cause = sessioncontract.ErrRuntimeClosed
	}
	return &rpcRequestSendError{outcome: outcome, admitted: admitted, cause: cause}
}

func rpcDeliveryError(runtime *Runtime, notDelivered error, cause error) error {
	if runtime.ctx.Err() != nil {
		return errors.Join(notDelivered, cause, sessioncontract.ErrRuntimeClosed, runtime.Err())
	}
	return errors.Join(notDelivered, cause)
}

func (runtime *Runtime) failRPCOperationAuthority() error {
	_ = runtime.router.TerminateLocal()
	runtime.terminateRuntimeFailed(errRPCOperationAuthority)
	return errRPCOperationAuthority
}

func (runtime *Runtime) reconcileLocalCancel(
	generation protocolsession.OperationGeneration,
) error {
	err := runtime.operations.CancelGeneration(generation)
	if err == nil {
		return nil
	}
	// Failure to retain a cancellation tombstone would leak active authority and
	// make a later ID collision ambiguous. Fail-closing atomically clears the
	// table instead of continuing a session whose at-most-once state is unknown.
	_ = runtime.router.TerminateLocal()
	runtime.terminateRuntimeFailed(err)
	return err
}

func protocolOperationCause(err error) sessionobs.ProtocolOperationCause {
	// Inspect joined components independently: errors.Is on the whole tree would
	// let a cancellation component erase an unrelated protocol or cleanup fault.
	// Only canonical lifecycle leaves prove a benign wakeup; an opaque Is match
	// cannot make a collaborator's own error harmless.
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
	case errors.Is(err, sessioncontract.ErrRuntimeClosed):
		return sessionobs.ProtocolOperationCauseRuntimeClosed
	case errors.Is(err, ErrLaneUnavailable):
		return sessionobs.ProtocolOperationCauseLaneUnavailable
	case errors.Is(err, protocolsession.ErrWriterStopped):
		return sessionobs.ProtocolOperationCauseWriterStopped
	case err == sessioncontract.ErrOperationMissing: //nolint:errorlint // An opaque Is match cannot prove that this operation ended.
		return sessionobs.ProtocolOperationCauseOperationClosed
	default:
		return sessionobs.ProtocolOperationCauseProtocolFailure
	}
}
