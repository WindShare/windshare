package sessionruntime

import (
	"errors"
	"fmt"
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
