package commandprojection

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"reflect"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2peer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

const (
	maximumErrorTreeDepth = 64
	maximumErrorTreeNodes = 1024
)

var (
	errStructuralProbe   = errors.New("command projection structural probe")
	trustedJoinType      = reflect.TypeOf(errors.Join(errStructuralProbe, errStructuralProbe))
	trustedWrapType      = reflect.TypeOf(fmt.Errorf("command projection wrapper: %w", errStructuralProbe))
	trustedMultiWrapType = reflect.TypeOf(fmt.Errorf("command projection wrappers: %w %w", errStructuralProbe, errStructuralProbe))
	trustedPathErrorType = reflect.TypeFor[*fs.PathError]()
	trustedURLErrorType  = reflect.TypeFor[*url.Error]()
	trustedNetErrorType  = reflect.TypeFor[*net.OpError]()
)

type errorTraversal struct{ remaining int }

func ClassifyError(cause error) (failure clievent.Failure, present bool) {
	if cause == nil {
		return clievent.Failure{}, false
	}
	// Diagnostic inputs are not authority. A hostile implementation must not be
	// able to panic the command while being reduced to a stable safe code.
	defer func() {
		if recover() != nil {
			failure = mustFailure(clievent.FailureUnexpected)
			present = true
		}
	}()
	result, ok := classifyErrorAtDepth(cause, 0, &errorTraversal{remaining: maximumErrorTreeNodes})
	if ok {
		return result, true
	}
	return mustFailure(clievent.FailureUnexpected), true
}

func classifyErrorAtDepth(cause error, depth int, traversal *errorTraversal) (clievent.Failure, bool) {
	if cause == nil || depth >= maximumErrorTreeDepth || traversal.remaining == 0 || typedNil(cause) {
		return clievent.Failure{}, false
	}
	traversal.remaining--
	if failure, ok := classifyDirectError(cause); ok {
		return failure, true
	}
	switch reflect.TypeOf(cause) {
	case trustedPathErrorType:
		return mustFailure(clievent.FailureOutputStateIO), true
	case trustedURLErrorType, trustedNetErrorType:
		return mustFailure(clievent.FailureRelayTransport), true
	case trustedJoinType, trustedMultiWrapType:
		children, ok := cause.(interface{ Unwrap() []error })
		if !ok {
			return clievent.Failure{}, false
		}
		for _, child := range children.Unwrap() {
			if failure, found := classifyErrorAtDepth(child, depth+1, traversal); found {
				return failure, true
			}
		}
	case trustedWrapType:
		child, ok := cause.(interface{ Unwrap() error })
		if !ok {
			return clievent.Failure{}, false
		}
		return classifyErrorAtDepth(child.Unwrap(), depth+1, traversal)
	}
	return clievent.Failure{}, false
}

func classifyDirectError(cause error) (clievent.Failure, bool) {
	if exactError(cause, context.Canceled) {
		return mustFailure(clievent.FailureCanceled), true
	}
	if exactError(cause, context.DeadlineExceeded) {
		return mustFailure(clievent.FailureDeadline), true
	}
	// Only a direct concrete provider fault is trusted here. errors.As could invoke
	// an untrusted As method while diagnostics are crossing the safety boundary.
	//nolint:errorlint
	if relayFailure, ok := cause.(*relayv2.RelayError); ok && relayFailure != nil {
		failure, mapped := ProjectRelayErrorCode(relayFailure.Code)
		if !mapped {
			return clievent.Failure{}, false
		}
		if (relayFailure.Code == v2.ErrorStarting || relayFailure.Code == v2.ErrorAdmission) &&
			relayFailure.RetryAfter > 0 {
			millis := uint64(relayFailure.RetryAfter / time.Millisecond)
			if retryable, err := clievent.NewRetryableFailure(failure.Code(), millis); err == nil {
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
	case exactError(cause, relayv2.ErrFrameBounds), exactError(cause, relayv2.ErrProtocol):
		return mustFailure(clievent.FailureRelayProtocol), true
	case exactError(cause, relayv2.ErrIngressOverflow), exactError(cause, relayv2.ErrEgressOverflow):
		return mustFailure(clievent.FailureRelayOverflow), true
	case exactError(cause, relayv2.ErrClosed), exactError(cause, relayv2.ErrSessionRetired):
		return mustFailure(clievent.FailureRelayClosed), true
	case exactError(cause, wsrtc.ErrNilDataChannel), exactError(cause, wsrtc.ErrInvalidDataChannel),
		exactError(cause, wsrtc.ErrInvalidFlowControl):
		return mustFailure(clievent.FailurePeerConfiguration), true
	case exactError(cause, wsrtc.ErrChannelNotOpen), exactError(cause, wsrtc.ErrChannelClosed),
		exactError(cause, wsrtc.ErrRemoteClosed):
		return mustFailure(clievent.FailurePeerStopped), true
	case exactError(cause, wsrtc.ErrFrameBounds), exactError(cause, wsrtc.ErrPeerProtocol),
		exactError(cause, wsrtc.ErrTerminalNotAcknowledged):
		return mustFailure(clievent.FailurePeerProtocol), true
	case exactError(cause, wsrtc.ErrTransport):
		return mustFailure(clievent.FailurePeerNegotiation), true
	}
	return clievent.Failure{}, false
}

func containsExactError(cause, target error) (found bool) {
	defer func() {
		if recover() != nil {
			found = false
		}
	}()
	return containsExactErrorAtDepth(
		cause, target, 0, &errorTraversal{remaining: maximumErrorTreeNodes},
	)
}

func containsExactErrorAtDepth(
	cause, target error,
	depth int,
	traversal *errorTraversal,
) bool {
	if cause == nil || depth >= maximumErrorTreeDepth || traversal.remaining == 0 || typedNil(cause) {
		return false
	}
	traversal.remaining--
	if exactError(cause, target) {
		return true
	}
	switch reflect.TypeOf(cause) {
	case trustedJoinType, trustedMultiWrapType:
		children, ok := cause.(interface{ Unwrap() []error })
		if !ok {
			return false
		}
		for _, child := range children.Unwrap() {
			if containsExactErrorAtDepth(child, target, depth+1, traversal) {
				return true
			}
		}
	case trustedWrapType:
		child, ok := cause.(interface{ Unwrap() error })
		return ok && containsExactErrorAtDepth(child.Unwrap(), target, depth+1, traversal)
	}
	return false
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

func ProjectRelayErrorCode(code v2.ErrorCode) (clievent.Failure, bool) {
	var projected clievent.FailureCode
	switch code {
	case v2.ErrorMalformed:
		projected = clievent.FailureRelayMalformed
	case v2.ErrorUnsupportedMode:
		projected = clievent.FailureRelayUnsupportedMode
	case v2.ErrorShareIDCollision:
		projected = clievent.FailureRelayShareIDCollision
	case v2.ErrorAlreadyRegistered:
		projected = clievent.FailureRelayAlreadyRegistered
	case v2.ErrorChallengeExpired:
		projected = clievent.FailureRelayChallengeExpired
	case v2.ErrorInvalidProof:
		projected = clievent.FailureRelayInvalidProof
	case v2.ErrorDescriptorInvalid:
		projected = clievent.FailureRelayDescriptorInvalid
	case v2.ErrorNotFound:
		projected = clievent.FailureRelayNotFound
	case v2.ErrorStarting:
		projected = clievent.FailureRelayStarting
	case v2.ErrorAdmission:
		projected = clievent.FailureRelayAdmission
	case v2.ErrorStopped:
		projected = clievent.FailureRelayStopped
	default:
		return clievent.Failure{}, false
	}
	return mustFailure(projected), true
}

func ProjectFault(value transferfault.Fault) (clievent.Failure, bool) {
	if !value.Valid() {
		return clievent.Failure{}, false
	}
	domain, ok := projectFaultDomain(value.Domain())
	if !ok {
		return clievent.Failure{}, false
	}
	scope, ok := projectFaultScope(value.Scope())
	if !ok {
		return clievent.Failure{}, false
	}
	code, ok := projectFaultCode(value)
	if !ok {
		return clievent.Failure{}, false
	}
	context, err := clievent.NewFaultContext(domain, scope, value.Code())
	if err != nil {
		return clievent.Failure{}, false
	}
	failure, err := clievent.NewFaultFailure(code, context)
	return failure, err == nil
}

func ProjectNormalizedFault(domain uint8, scope uint8, code uint16) (clievent.Failure, bool) {
	projectedDomain, ok := projectFaultDomain(transferfault.Domain(domain))
	if !ok {
		return clievent.Failure{}, false
	}
	projectedScope, ok := projectFaultScope(transferfault.Scope(scope))
	if !ok {
		return clievent.Failure{}, false
	}
	projectedCode, ok := projectNumericFaultCode(transferfault.Domain(domain), code)
	if !ok {
		return clievent.Failure{}, false
	}
	context, err := clievent.NewFaultContext(projectedDomain, projectedScope, code)
	if err != nil {
		return clievent.Failure{}, false
	}
	failure, err := clievent.NewFaultFailure(projectedCode, context)
	return failure, err == nil
}

func projectNumericFaultCode(domain transferfault.Domain, code uint16) (clievent.FailureCode, bool) {
	switch domain {
	case transferfault.DomainSource:
		switch transferfault.SourceCode(code) {
		case transferfault.SourceUnavailable:
			return clievent.FailureSourceUnavailable, true
		case transferfault.SourceRevisionChanged:
			return clievent.FailureSourceRevisionChanged, true
		case transferfault.SourceRevisionInvalidated:
			return clievent.FailureSourceRevisionInvalidated, true
		case transferfault.SourcePermanent:
			return clievent.FailureSourcePermanent, true
		}
	case transferfault.DomainCatalog:
		switch transferfault.CatalogCode(code) {
		case transferfault.CatalogUnavailable:
			return clievent.FailureCatalogUnavailable, true
		case transferfault.CatalogDirectoryStale:
			return clievent.FailureCatalogDirectoryStale, true
		case transferfault.CatalogInvalidGeneration:
			return clievent.FailureCatalogInvalidGeneration, true
		}
	case transferfault.DomainSession:
		switch transferfault.SessionCode(code) {
		case transferfault.SessionTransport:
			return clievent.FailureSessionTransport, true
		case transferfault.SessionProtocol:
			return clievent.FailureSessionProtocol, true
		case transferfault.SessionResourceBudget:
			return clievent.FailureSessionResourceBudget, true
		case transferfault.SessionDependencyContract:
			return clievent.FailureSessionDependencyContract, true
		}
	case transferfault.DomainOutput:
		switch transferfault.OutputCode(code) {
		case transferfault.OutputStateIO:
			return clievent.FailureOutputStateIO, true
		case transferfault.OutputOwnership:
			return clievent.FailureOutputOwnership, true
		case transferfault.OutputNamespaceUnsafe:
			return clievent.FailureOutputNamespaceUnsafe, true
		case transferfault.OutputUnsupportedFilesystem:
			return clievent.FailureOutputUnsupportedFilesystem, true
		case transferfault.OutputDirectoryBinding:
			return clievent.FailureOutputDirectoryBinding, true
		case transferfault.OutputDirectoryMetadata:
			return clievent.FailureOutputDirectoryMetadata, true
		case transferfault.OutputFileAlreadyActive:
			return clievent.FailureOutputFileAlreadyActive, true
		case transferfault.OutputResourceBudget:
			return clievent.FailureOutputResourceBudget, true
		case transferfault.OutputMutationAmbiguous:
			return clievent.FailureOutputMutationAmbiguous, true
		case transferfault.OutputContract:
			return clievent.FailureOutputContract, true
		}
	case transferfault.DomainCheckpoint:
		switch transferfault.CheckpointCode(code) {
		case transferfault.CheckpointBusy:
			return clievent.FailureCheckpointBusy, true
		case transferfault.CheckpointCorruptRecord:
			return clievent.FailureCheckpointCorruptRecord, true
		case transferfault.CheckpointUnsafeInstall:
			return clievent.FailureCheckpointUnsafeInstall, true
		case transferfault.CheckpointOwnershipMismatch:
			return clievent.FailureCheckpointOwnershipMismatch, true
		case transferfault.CheckpointStateIO:
			return clievent.FailureCheckpointStateIO, true
		}
	}
	return 0, false
}

func projectFaultDomain(value transferfault.Domain) (clievent.FaultDomain, bool) {
	switch value {
	case transferfault.DomainSource:
		return clievent.FaultSource, true
	case transferfault.DomainCatalog:
		return clievent.FaultCatalog, true
	case transferfault.DomainSession:
		return clievent.FaultSession, true
	case transferfault.DomainOutput:
		return clievent.FaultOutput, true
	case transferfault.DomainCheckpoint:
		return clievent.FaultCheckpoint, true
	default:
		return 0, false
	}
}

func projectFaultScope(value transferfault.Scope) (clievent.FaultScope, bool) {
	switch value {
	case transferfault.ScopeFileLocal:
		return clievent.FaultFileLocal, true
	case transferfault.ScopeDirectoryLocal:
		return clievent.FaultDirectoryLocal, true
	case transferfault.ScopeOutputPause:
		return clievent.FaultOutputPause, true
	case transferfault.ScopeSessionTerminal:
		return clievent.FaultSessionTerminal, true
	default:
		return 0, false
	}
}

func projectFaultCode(value transferfault.Fault) (clievent.FailureCode, bool) {
	switch value.Domain() {
	case transferfault.DomainSource:
		code, ok := value.SourceCode()
		if !ok {
			return 0, false
		}
		return projectSourceFaultCode(code)
	case transferfault.DomainCatalog:
		code, ok := value.CatalogCode()
		if !ok {
			return 0, false
		}
		return projectCatalogFaultCode(code)
	case transferfault.DomainSession:
		code, ok := value.SessionCode()
		if !ok {
			return 0, false
		}
		return projectSessionFaultCode(code)
	case transferfault.DomainOutput:
		code, ok := value.OutputCode()
		if !ok {
			return 0, false
		}
		return projectOutputFaultCode(code)
	case transferfault.DomainCheckpoint:
		code, ok := value.CheckpointCode()
		if !ok {
			return 0, false
		}
		return projectCheckpointFaultCode(code)
	}
	return 0, false
}

func projectSourceFaultCode(code transferfault.SourceCode) (clievent.FailureCode, bool) {
	switch code {
	case transferfault.SourceUnavailable:
		return clievent.FailureSourceUnavailable, true
	case transferfault.SourceRevisionChanged:
		return clievent.FailureSourceRevisionChanged, true
	case transferfault.SourceRevisionInvalidated:
		return clievent.FailureSourceRevisionInvalidated, true
	case transferfault.SourcePermanent:
		return clievent.FailureSourcePermanent, true
	default:
		return 0, false
	}
}

func projectCatalogFaultCode(code transferfault.CatalogCode) (clievent.FailureCode, bool) {
	switch code {
	case transferfault.CatalogUnavailable:
		return clievent.FailureCatalogUnavailable, true
	case transferfault.CatalogDirectoryStale:
		return clievent.FailureCatalogDirectoryStale, true
	case transferfault.CatalogInvalidGeneration:
		return clievent.FailureCatalogInvalidGeneration, true
	default:
		return 0, false
	}
}

func projectSessionFaultCode(code transferfault.SessionCode) (clievent.FailureCode, bool) {
	switch code {
	case transferfault.SessionTransport:
		return clievent.FailureSessionTransport, true
	case transferfault.SessionProtocol:
		return clievent.FailureSessionProtocol, true
	case transferfault.SessionResourceBudget:
		return clievent.FailureSessionResourceBudget, true
	case transferfault.SessionDependencyContract:
		return clievent.FailureSessionDependencyContract, true
	default:
		return 0, false
	}
}

func projectOutputFaultCode(code transferfault.OutputCode) (clievent.FailureCode, bool) {
	switch code {
	case transferfault.OutputStateIO:
		return clievent.FailureOutputStateIO, true
	case transferfault.OutputOwnership:
		return clievent.FailureOutputOwnership, true
	case transferfault.OutputNamespaceUnsafe:
		return clievent.FailureOutputNamespaceUnsafe, true
	case transferfault.OutputUnsupportedFilesystem:
		return clievent.FailureOutputUnsupportedFilesystem, true
	case transferfault.OutputDirectoryBinding:
		return clievent.FailureOutputDirectoryBinding, true
	case transferfault.OutputDirectoryMetadata:
		return clievent.FailureOutputDirectoryMetadata, true
	case transferfault.OutputFileAlreadyActive:
		return clievent.FailureOutputFileAlreadyActive, true
	case transferfault.OutputResourceBudget:
		return clievent.FailureOutputResourceBudget, true
	case transferfault.OutputMutationAmbiguous:
		return clievent.FailureOutputMutationAmbiguous, true
	case transferfault.OutputContract:
		return clievent.FailureOutputContract, true
	default:
		return 0, false
	}
}

func projectCheckpointFaultCode(code transferfault.CheckpointCode) (clievent.FailureCode, bool) {
	switch code {
	case transferfault.CheckpointBusy:
		return clievent.FailureCheckpointBusy, true
	case transferfault.CheckpointCorruptRecord:
		return clievent.FailureCheckpointCorruptRecord, true
	case transferfault.CheckpointUnsafeInstall:
		return clievent.FailureCheckpointUnsafeInstall, true
	case transferfault.CheckpointOwnershipMismatch:
		return clievent.FailureCheckpointOwnershipMismatch, true
	case transferfault.CheckpointStateIO:
		return clievent.FailureCheckpointStateIO, true
	default:
		return 0, false
	}
}

func ProjectPeerErrorCode(value v2peer.TypedPeerErrorCode) (clievent.Failure, bool) {
	var code clievent.FailureCode
	switch value {
	case v2peer.TypedPeerErrorNegotiation:
		code = clievent.FailurePeerNegotiation
	case v2peer.TypedPeerErrorTimeout:
		code = clievent.FailurePeerTimeout
	case v2peer.TypedPeerErrorCandidates:
		code = clievent.FailurePeerCandidates
	case v2peer.TypedPeerErrorAdmission:
		code = clievent.FailurePeerAdmission
	case v2peer.TypedPeerErrorSignaling:
		code = clievent.FailurePeerSignaling
	case v2peer.TypedPeerErrorCancelled:
		code = clievent.FailurePeerCanceled
	case v2peer.TypedPeerErrorStopped:
		code = clievent.FailurePeerStopped
	case v2peer.TypedPeerErrorUnexpected:
		code = clievent.FailureUnexpected
	default:
		return clievent.Failure{}, false
	}
	return mustFailure(code), true
}

func ProjectReceiverCauseClass(value v2peer.ReceiverCauseClass) (clievent.Failure, bool) {
	var code clievent.FailureCode
	switch value {
	case v2peer.ReceiverCauseRuntimeClosed:
		code = clievent.FailurePeerStopped
	case v2peer.ReceiverCauseConfiguration:
		code = clievent.FailurePeerConfiguration
	case v2peer.ReceiverCauseOperationMissing:
		code = clievent.FailurePeerOperationMissing
	case v2peer.ReceiverCauseAttemptTimeout:
		code = clievent.FailurePeerTimeout
	case v2peer.ReceiverCauseCandidateLimit:
		code = clievent.FailurePeerCandidates
	case v2peer.ReceiverCauseChannelAdmission:
		code = clievent.FailurePeerAdmission
	case v2peer.ReceiverCauseEventCapacity:
		code = clievent.FailurePeerEventCapacity
	case v2peer.ReceiverCauseNegotiation:
		code = clievent.FailurePeerNegotiation
	case v2peer.ReceiverCauseProtocol:
		code = clievent.FailurePeerProtocol
	case v2peer.ReceiverCauseDeadline:
		code = clievent.FailureDeadline
	case v2peer.ReceiverCausePeerShutdown:
		code = clievent.FailurePeerShutdown
	case v2peer.ReceiverCauseChannelDrain:
		code = clievent.FailurePeerChannelDrain
	case v2peer.ReceiverCauseUnknown:
		code = clievent.FailureUnexpected
	default:
		return clievent.Failure{}, false
	}
	return mustFailure(code), true
}

func ProjectRelayLifecycleCause(value relayv2.LifecycleCause) (clievent.Failure, bool) {
	switch value {
	case relayv2.LifecycleCauseNone:
		return clievent.Failure{}, false
	case relayv2.LifecycleCauseCanceled:
		return mustFailure(clievent.FailureCanceled), true
	case relayv2.LifecycleCauseDeadline:
		return mustFailure(clievent.FailureDeadline), true
	case relayv2.LifecycleCauseFrameBounds, relayv2.LifecycleCauseProtocol:
		return mustFailure(clievent.FailureRelayProtocol), true
	case relayv2.LifecycleCauseEgressOverflow, relayv2.LifecycleCauseIngressOverflow:
		return mustFailure(clievent.FailureRelayOverflow), true
	case relayv2.LifecycleCauseSessionRetired, relayv2.LifecycleCauseClosed:
		return mustFailure(clievent.FailureRelayClosed), true
	case relayv2.LifecycleCauseTransport:
		return mustFailure(clievent.FailureRelayTransport), true
	default:
		return clievent.Failure{}, false
	}
}

func ProjectWebRTCLifecycleCause(value wsrtc.LifecycleCause) (clievent.Failure, bool) {
	switch value {
	case wsrtc.LifecycleCauseNone:
		return clievent.Failure{}, false
	case wsrtc.LifecycleCauseCanceled:
		return mustFailure(clievent.FailureCanceled), true
	case wsrtc.LifecycleCauseDeadline:
		return mustFailure(clievent.FailureDeadline), true
	case wsrtc.LifecycleCauseNotOpen, wsrtc.LifecycleCauseNaturalRetirement, wsrtc.LifecycleCauseRemoteClosed:
		return mustFailure(clievent.FailurePeerStopped), true
	case wsrtc.LifecycleCauseTerminalUnacknowledged, wsrtc.LifecycleCausePeerProtocol:
		return mustFailure(clievent.FailurePeerProtocol), true
	case wsrtc.LifecycleCauseTransport:
		return mustFailure(clievent.FailurePeerNegotiation), true
	case wsrtc.LifecycleCauseOther:
		return mustFailure(clievent.FailureUnexpected), true
	default:
		return clievent.Failure{}, false
	}
}

func mustFailure(code clievent.FailureCode) clievent.Failure {
	failure, err := clievent.NewFailure(code)
	if err != nil {
		panic("commandprojection: invalid internal failure registry")
	}
	return failure
}
