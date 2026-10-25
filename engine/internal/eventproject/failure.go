package eventproject

import (
	context "context"
	errors "errors"
	fmt "fmt"
	fs "io/fs"
	net "net"
	url "net/url"
	reflect "reflect"
	time "time"

	relayset "github.com/windshare/windshare/connectivity/relayset"
	catalog "github.com/windshare/windshare/core/catalog"
	transfer "github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	receive "github.com/windshare/windshare/engine/internal/receive"
	task "github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
	output "github.com/windshare/windshare/engine/output"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	relayv2 "github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

const (
	maximumErrorTreeDepth = 64
	maximumErrorTreeNodes = 1024
)

var (
	errStructuralProbe   = errors.New("workflow projection structural probe")
	trustedJoinType      = reflect.TypeOf(errors.Join(errStructuralProbe, errStructuralProbe))
	trustedWrapType      = reflect.TypeOf(fmt.Errorf("workflow projection wrapper: %w", errStructuralProbe))
	trustedMultiWrapType = reflect.TypeOf(fmt.Errorf("workflow projection wrappers: %w %w", errStructuralProbe, errStructuralProbe))
	trustedPathErrorType = reflect.TypeFor[*fs.PathError]()
	trustedURLErrorType  = reflect.TypeFor[*url.Error]()
	trustedNetErrorType  = reflect.TypeFor[*net.OpError]()
)

type errorTraversal struct{ remaining int }

func ClassifyError(cause error) (failure appobs.Failure, present bool) {
	if cause == nil {
		return appobs.Failure{}, false
	}
	// Diagnostic inputs are not authority. A hostile implementation must not be
	// able to panic the workflow while being reduced to a stable safe code.
	defer func() {
		if recover() != nil {
			failure = mustFailure(appobs.FailureUnexpected)
			present = true
		}
	}()
	result, ok := classifyErrorAtDepth(cause, 0, &errorTraversal{remaining: maximumErrorTreeNodes})
	if ok {
		return result, true
	}
	return mustFailure(appobs.FailureUnexpected), true
}

func classifyErrorAtDepth(cause error, depth int, traversal *errorTraversal) (appobs.Failure, bool) {
	if cause == nil || depth >= maximumErrorTreeDepth || traversal.remaining == 0 || typedNil(cause) {
		return appobs.Failure{}, false
	}
	traversal.remaining--
	// Preserve source semantics before considering native path errors, which
	// otherwise describe destination I/O. Selected paths are display facts, not
	// raw provider error text or filesystem authority.
	//nolint:errorlint
	if selected, ok := cause.(*catalog.SelectionError); ok {
		failure, known := classifyErrorAtDepth(selected.Cause, depth+1, traversal)
		if !known {
			return appobs.Failure{}, false
		}
		if located, err := appobs.NewSourcePathFailure(failure.Code(), appobs.NewDisplayPath(selected.Label)); err == nil {
			return located, true
		}
		return failure, true
	}
	//nolint:errorlint
	if source, ok := cause.(*catalog.SourceError); ok {
		return projectSourceFailure(source.Failure)
	}
	// Inspect this node directly: errors.As would bypass the traversal budget and
	// allow a diagnostic carrier's As method to replace the workflow's decision.
	//nolint:errorlint
	if reason, ok := cause.(receive.Failure); ok {
		if failure, known := projectReceiveFailure(reason); known {
			return failure, true
		}
		if failure, known := classifyErrorAtDepth(reason.Cause, depth+1, traversal); known {
			return failure, true
		}
		return mustFailure(appobs.FailureUnexpected), true
	}
	if failure, ok := classifyDirectError(cause); ok {
		return failure, true
	}
	switch reflect.TypeOf(cause) {
	case trustedPathErrorType:
		return mustFailure(appobs.FailureOutputStateIO), true
	case trustedURLErrorType, trustedNetErrorType:
		return mustFailure(appobs.FailureRelayTransport), true
	case trustedJoinType, trustedMultiWrapType, reflect.TypeFor[*relayset.ReceiverJoinFailure]():
		children, ok := cause.(interface{ Unwrap() []error })
		if !ok {
			return appobs.Failure{}, false
		}
		for _, child := range children.Unwrap() {
			if failure, found := classifyErrorAtDepth(child, depth+1, traversal); found {
				return failure, true
			}
		}
	case trustedWrapType:
		child, ok := cause.(interface{ Unwrap() error })
		if !ok {
			return appobs.Failure{}, false
		}
		return classifyErrorAtDepth(child.Unwrap(), depth+1, traversal)
	}
	return appobs.Failure{}, false
}

func classifyDirectError(cause error) (appobs.Failure, bool) {
	if diagnostic, ok := output.DiagnosticFor(cause); ok {
		return diagnostic.Failure, diagnostic.Failure.Valid()
	}
	// Engine stop causes preserve ownership, but all express deliberate local
	// cancellation rather than an unexpected provider failure.
	if exactError(cause, context.Canceled) || exactError(cause, task.ErrCancelled) ||
		exactError(cause, task.ErrApplicationClosed) || exactError(cause, task.ErrShareStopped) {
		return mustFailure(appobs.FailureCanceled), true
	}
	if exactError(cause, context.DeadlineExceeded) {
		return mustFailure(appobs.FailureDeadline), true
	}
	// Only a direct concrete provider fault is trusted here. errors.As could invoke
	// an untrusted As method while diagnostics are crossing the safety boundary.
	//nolint:errorlint
	if relayFailure, ok := cause.(*relayv2.RelayError); ok && relayFailure != nil {
		failure, mapped := ProjectRelayErrorCode(relayFailure.Code)
		if !mapped {
			return appobs.Failure{}, false
		}
		if (relayFailure.Code == v2.ErrorStarting || relayFailure.Code == v2.ErrorAdmission) &&
			relayFailure.RetryAfter > 0 {
			millis := uint64(relayFailure.RetryAfter / time.Millisecond)
			if retryable, err := appobs.NewRetryableFailure(failure.Code(), millis); err == nil {
				return retryable, true
			}
		}
		return failure, true
	}
	// The trusted concrete boundary must be direct for the same reason as relay
	// faults; traversal of wrappers is limited to the audited types above.
	//nolint:errorlint
	if boundary, ok := cause.(*transferfault.BoundaryError); ok && boundary != nil {
		return ProjectFault(boundary.Fault())
	}
	switch {
	case exactError(cause, relayset.ErrReceiverUnavailable):
		return mustFailure(appobs.FailureRelayNotFound), true
	case exactError(cause, relayset.ErrReceiverWaitExpired):
		return mustFailure(appobs.FailureDeadline), true
	case exactError(cause, relayset.ErrReceiverShareChanged), exactError(cause, relayset.ErrReceiverAuthentication):
		return mustFailure(appobs.FailureSessionProtocol), true
	case exactError(cause, relayv2.ErrFrameBounds), exactError(cause, relayv2.ErrProtocol):
		return mustFailure(appobs.FailureRelayProtocol), true
	case exactError(cause, relayv2.ErrIngressOverflow), exactError(cause, relayv2.ErrEgressOverflow):
		return mustFailure(appobs.FailureRelayOverflow), true
	case exactError(cause, relayv2.ErrClosed), exactError(cause, relayv2.ErrSessionRetired):
		return mustFailure(appobs.FailureRelayClosed), true
	case exactError(cause, wsrtc.ErrNilDataChannel), exactError(cause, wsrtc.ErrInvalidDataChannel),
		exactError(cause, wsrtc.ErrInvalidFlowControl):
		return mustFailure(appobs.FailurePeerConfiguration), true
	case exactError(cause, wsrtc.ErrChannelNotOpen), exactError(cause, wsrtc.ErrChannelClosed),
		exactError(cause, wsrtc.ErrRemoteClosed):
		return mustFailure(appobs.FailurePeerStopped), true
	case exactError(cause, wsrtc.ErrFrameBounds), exactError(cause, wsrtc.ErrPeerProtocol),
		exactError(cause, wsrtc.ErrTerminalNotAcknowledged):
		return mustFailure(appobs.FailurePeerProtocol), true
	case exactError(cause, wsrtc.ErrTransport):
		return mustFailure(appobs.FailurePeerNegotiation), true
	}
	return appobs.Failure{}, false
}

func projectSourceFailure(reason catalog.SourceFailure) (appobs.Failure, bool) {
	var code appobs.FailureCode
	switch reason {
	case catalog.SourceFailureMissing:
		code = appobs.FailureSourceMissing
	case catalog.SourceFailureAccessDenied:
		code = appobs.FailureSourceAccessDenied
	case catalog.SourceFailureUnsupported:
		code = appobs.FailureSourceUnsupported
	case catalog.SourceFailureStale:
		code = appobs.FailureSourceRevisionChanged
	case catalog.SourceFailureUnavailable:
		code = appobs.FailureSourceUnavailable
	default:
		return appobs.Failure{}, false
	}
	return mustFailure(code), true
}

func typedNil(value error) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func exactError(candidate, target error) bool {
	if candidate == nil || target == nil || reflect.TypeOf(candidate) != reflect.TypeOf(target) {
		return false
	}
	value := reflect.ValueOf(candidate)
	// Exact comparable identity is intentional. errors.Is would execute caller
	// hooks and would also broaden this predicate beyond its proof semantics.
	//nolint:errorlint
	return value.Comparable() && value.Interface() == target
}

func ProjectRelayErrorCode(code v2.ErrorCode) (appobs.Failure, bool) {
	var projected appobs.FailureCode
	switch code {
	case v2.ErrorMalformed:
		projected = appobs.FailureRelayMalformed
	case v2.ErrorUnsupportedMode:
		projected = appobs.FailureRelayUnsupportedMode
	case v2.ErrorShareIDCollision:
		projected = appobs.FailureRelayShareIDCollision
	case v2.ErrorAlreadyRegistered:
		projected = appobs.FailureRelayAlreadyRegistered
	case v2.ErrorChallengeExpired:
		projected = appobs.FailureRelayChallengeExpired
	case v2.ErrorInvalidProof:
		projected = appobs.FailureRelayInvalidProof
	case v2.ErrorDescriptorInvalid:
		projected = appobs.FailureRelayDescriptorInvalid
	case v2.ErrorNotFound:
		projected = appobs.FailureRelayNotFound
	case v2.ErrorStarting:
		projected = appobs.FailureRelayStarting
	case v2.ErrorAdmission:
		projected = appobs.FailureRelayAdmission
	case v2.ErrorStopped:
		projected = appobs.FailureRelayStopped
	default:
		return appobs.Failure{}, false
	}
	return mustFailure(projected), true
}

func ProjectTransferInterruption(value transfer.TransferInterruption) (appobs.Failure, bool) {
	switch value {
	case transfer.TransferInterruptionCanceled:
		return mustFailure(appobs.FailureCanceled), true
	case transfer.TransferInterruptionDeadline:
		return mustFailure(appobs.FailureDeadline), true
	default:
		return appobs.Failure{}, false
	}
}

func mustFailure(code appobs.FailureCode) appobs.Failure {
	failure, err := appobs.NewFailure(code)
	if err != nil {
		panic("eventproject: invalid internal failure registry")
	}
	return failure
}

// The workflow's typed reason owns precedence over incidental diagnostic causes.
func projectReceiveFailure(value receive.Failure) (appobs.Failure, bool) {
	if failure, ok := ProjectFault(value.Fault); ok {
		return failure, true
	}
	if failure, ok := ProjectTransferInterruption(value.Interruption); ok {
		return failure, true
	}
	local := map[receive.LocalFailure]appobs.FailureCode{
		receive.LocalSelectionMissing: appobs.FailureSelectionMissing, receive.LocalRevisionConflict: appobs.FailureCheckpointRevisionConflict,
		receive.LocalCheckpointInvalid: appobs.FailureCheckpointInvalid, receive.LocalOwnedObjectUnknown: appobs.FailureOwnedObjectUnknown, receive.LocalDestinationCollision: appobs.FailureDestinationCollision}
	if code, ok := local[value.Local]; ok {
		return mustFailure(code), true
	}
	if code, ok := ReceiveFailureCode(value.Code); ok {
		return mustFailure(code), true
	}
	return appobs.Failure{}, false
}

func ReceiveFailureCode(code receive.FailureCode) (appobs.FailureCode, bool) {
	switch code {
	case receive.FailureInvalidInput:
		return appobs.FailureInvalidInput, true
	case receive.FailureOutputContract:
		return appobs.FailureOutputContract, true
	case receive.FailureOutputFileAlreadyActive:
		return appobs.FailureOutputFileAlreadyActive, true
	case receive.FailureOutputNeedsAttention:
		return appobs.FailureOutputNeedsAttention, true
	case receive.FailureOutputOwnership:
		return appobs.FailureOutputOwnership, true
	case receive.FailureOutputRecoveryUnavailable:
		return appobs.FailureOutputRecoveryUnavailable, true
	case receive.FailurePeerConfiguration:
		return appobs.FailurePeerConfiguration, true
	case receive.FailurePeerNegotiation:
		return appobs.FailurePeerNegotiation, true
	case receive.FailurePeerProtocol:
		return appobs.FailurePeerProtocol, true
	case receive.FailurePeerSignaling:
		return appobs.FailurePeerSignaling, true
	case receive.FailurePeerStopped:
		return appobs.FailurePeerStopped, true
	default:
		return 0, false
	}
}

func ProjectFault(value transferfault.Fault) (appobs.Failure, bool) {
	return appobs.ProjectFault(value)
}
