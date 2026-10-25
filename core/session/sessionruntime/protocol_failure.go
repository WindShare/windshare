package sessionruntime

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	diag "github.com/windshare/windshare/core/diagnostics"

	"github.com/windshare/windshare/core/session/contentflow"

	"github.com/windshare/windshare/core/session/protocolsession"
)

var ErrProtocolErrorContent = errors.New("session runtime protocol error content is invalid")

type rpcRequestSendError struct {
	outcome  protocolsession.SendOutcome
	admitted bool
	cause    error
}

func (failure *rpcRequestSendError) Error() string {
	return fmt.Sprintf("send operation request: %v", failure.cause)
}

func (failure *rpcRequestSendError) Unwrap() error { return failure.cause }

func requestProvenNotDelivered(err error) bool {
	var failure *rpcRequestSendError
	return errors.As(err, &failure) && failure.outcome == protocolsession.SendOutcomeDropped && !failure.admitted
}

func newRPCRequestSendError(outcome protocolsession.SendOutcome, admitted bool, cause error) error {
	if cause == nil {
		cause = ErrRuntimeClosed
	}
	return &rpcRequestSendError{outcome: outcome, admitted: admitted, cause: cause}
}

func rpcDeliveryError(runtime *runtimeCore, notDelivered error, cause error) error {
	if runtime.ctx.Err() != nil {
		return errors.Join(notDelivered, cause, ErrRuntimeClosed, runtime.Err())
	}
	return errors.Join(notDelivered, cause)
}

func (runtime *runtimeCore) failRPCOperationAuthority() error {
	_ = runtime.router.TerminateLocal()
	runtime.terminateRuntimeFailed(errRPCOperationAuthority)
	return errRPCOperationAuthority
}

func (runtime *runtimeCore) reconcileLocalCancel(
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

type ProtocolErrorScope uint8

const (
	ProtocolErrorDirectory = ProtocolErrorScope(protocolsession.OperationScopeDirectory)
	ProtocolErrorRevision  = ProtocolErrorScope(protocolsession.OperationScopeRevision)
	ProtocolErrorBlock     = ProtocolErrorScope(protocolsession.OperationScopeBlock)
	ProtocolErrorPeer      = ProtocolErrorScope(protocolsession.OperationScopePeer)
)

type ProtocolErrorContentSpec struct {
	WireScope        ProtocolErrorScope
	WireCode         uint16
	Retryable        bool
	RetryAfterMillis uint32
	HasRetryAfter    bool
}

// ProtocolErrorContent contains wire meaning only. The observing boundary owns
// identity, lane, and send evidence, so migration cannot rewrite error context.
type ProtocolErrorContent struct {
	scope            ProtocolErrorScope
	code             uint16
	retryable        bool
	retryAfterMillis uint32
	hasRetryAfter    bool
}

func NewProtocolErrorContent(spec ProtocolErrorContentSpec) (ProtocolErrorContent, error) {
	if !validProtocolErrorScope(spec.WireScope) ||
		spec.Retryable != spec.HasRetryAfter ||
		(!spec.HasRetryAfter && spec.RetryAfterMillis != 0) ||
		(spec.HasRetryAfter && (spec.RetryAfterMillis < uint32(protocolsession.MinOperationFailureRetryAfter.Milliseconds()) ||
			spec.RetryAfterMillis > uint32(protocolsession.MaxOperationFailureRetryAfter.Milliseconds()))) {
		return ProtocolErrorContent{}, ErrProtocolErrorContent
	}
	return ProtocolErrorContent{scope: spec.WireScope, code: spec.WireCode, retryable: spec.Retryable, retryAfterMillis: spec.RetryAfterMillis, hasRetryAfter: spec.HasRetryAfter}, nil
}
func (content ProtocolErrorContent) IsZero() bool                  { return content.scope == 0 }
func (content ProtocolErrorContent) WireScope() ProtocolErrorScope { return content.scope }
func (content ProtocolErrorContent) WireCode() uint16              { return content.code }
func (content ProtocolErrorContent) Retryable() bool               { return content.retryable }
func (content ProtocolErrorContent) RetryAfterMillis() (uint32, bool) {
	return content.retryAfterMillis, content.hasRetryAfter
}

func validProtocolErrorScope(scope ProtocolErrorScope) bool {
	switch scope {
	case ProtocolErrorDirectory, ProtocolErrorRevision, ProtocolErrorBlock, ProtocolErrorPeer:
		return true
	default:
		return false
	}
}
func protocolErrorContent(wire protocolsession.OperationFailure) (ProtocolErrorContent, bool) {
	spec := ProtocolErrorContentSpec{WireScope: ProtocolErrorScope(wire.Scope), WireCode: wire.Code, Retryable: wire.Retryable, HasRetryAfter: wire.Retryable}
	if wire.Retryable {
		spec.RetryAfterMillis = uint32(wire.RetryAfter.Milliseconds())
	}
	value, err := NewProtocolErrorContent(spec)
	return value, err == nil
}
func protocolErrorForResponse(kind protocolsession.MessageKind, body []byte) ProtocolErrorContent {
	if kind != protocolsession.MessageOperationError {
		return ProtocolErrorContent{}
	}
	wire, err := protocolsession.DecodeOperationFailure(body)
	if err != nil {
		return ProtocolErrorContent{}
	}
	value, _ := protocolErrorContent(wire)
	return value
}
func protocolErrorForAuthenticatedReceive(message protocolsession.Message) (ProtocolErrorContent, bool) {
	if message.Kind() != protocolsession.MessageOperationError {
		return ProtocolErrorContent{}, false
	}
	semantic, err := protocolsession.SenderControlSemanticBody(message)
	if err != nil {
		return ProtocolErrorContent{}, false
	}
	wire, err := protocolsession.DecodeOperationFailure(semantic)
	if err != nil {
		return ProtocolErrorContent{}, false
	}
	return protocolErrorContent(wire)
}

// DiagnosticName belongs to the source vocabulary; recorders accept new values.
func (value ProtocolErrorScope) DiagnosticName() string {
	switch value {
	case ProtocolErrorDirectory:
		return "directory"
	case ProtocolErrorRevision:
		return "revision"
	case ProtocolErrorBlock:
		return "block"
	case ProtocolErrorPeer:
		return "peer"
	default:
		return "unknown_" + strconv.FormatUint(uint64(value), 10)
	}
}

func protocolOperationLifecycleCause(cause ProtocolOperationCause) bool {
	return cause == ProtocolOperationCauseNone || cause == ProtocolOperationCauseCanceled ||
		cause == ProtocolOperationCauseOperationClosed || cause == ProtocolOperationCauseSuperseded
}

func mergeProtocolOperationCause(current, next ProtocolOperationCause) ProtocolOperationCause {
	// A wakeup can arrive before an authenticated fault or failed cleanup.
	// Preserve the first actual failure instead of letting cancellation mask it.
	if current == ProtocolOperationCauseNone ||
		(protocolOperationLifecycleCause(current) && !protocolOperationLifecycleCause(next)) {
		return next
	}
	return current
}

func protocolOperationCause(err error) ProtocolOperationCause {
	// Inspect joined components independently: errors.Is on the whole tree would
	// let a cancellation component erase an unrelated protocol or cleanup fault.
	// Only canonical lifecycle leaves prove a benign wakeup; an opaque Is match
	// cannot make a collaborator's own error harmless.
	switch wrapped := err.(type) { //nolint:errorlint // Inspect one node at a time so every joined fault is classified.
	case interface{ Unwrap() []error }:
		cause := ProtocolOperationCauseNone
		for _, component := range wrapped.Unwrap() {
			cause = mergeProtocolOperationCause(cause, protocolOperationCause(component))
		}
		if cause != ProtocolOperationCauseNone {
			return cause
		}
	case interface{ Unwrap() error }:
		if inner := wrapped.Unwrap(); inner != nil {
			return protocolOperationCause(inner)
		}
	}
	switch {
	case err == nil:
		return ProtocolOperationCauseNone
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, contentflow.ErrFragmentInactivity),
		errors.Is(err, contentflow.ErrBlockResponseInactivity):
		return ProtocolOperationCauseDeadline
	case err == context.Canceled: //nolint:errorlint // Only the canonical leaf proves an ordinary canceled wait.
		return ProtocolOperationCauseCanceled
	case errors.Is(err, ErrRuntimeClosed):
		return ProtocolOperationCauseRuntimeClosed
	case errors.Is(err, ErrLaneUnavailable):
		return ProtocolOperationCauseLaneUnavailable
	case errors.Is(err, protocolsession.ErrWriterStopped):
		return ProtocolOperationCauseWriterStopped
	case err == ErrOperationMissing: //nolint:errorlint // An opaque Is match cannot prove that this operation ended.
		return ProtocolOperationCauseOperationClosed
	default:
		return ProtocolOperationCauseProtocolFailure
	}
}

func (value ProtocolErrorContent) DiagnosticValue() diag.Value {
	if value.IsZero() {
		return diag.Value{}
	}
	fields := diag.Fields{"scope": diag.Text(value.WireScope().DiagnosticName()), "code": diag.Number(float64(value.WireCode())), "retryable": diag.Bool(value.Retryable())}
	if retry, ok := value.RetryAfterMillis(); ok {
		fields["retry_after_ms"] = diag.Number(float64(retry))
	}
	return diag.Object(fields)
}
