package eventproject

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"reflect"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
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
	if selected, ok := cause.(*osfs.SelectedPathError); ok {
		failure, known := classifyErrorAtDepth(selected.Cause, depth+1, traversal)
		if !known {
			return appobs.Failure{}, false
		}
		if located, err := appobs.NewSourcePathFailure(failure.Code(), appobs.NewDisplayPath(selected.Path)); err == nil {
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
	if diagnostic, ok := receive.OutputDiagnostic(cause); ok {
		return ProjectNormalizedFault(diagnostic.FaultDomain, diagnostic.NormalizedScope, diagnostic.NormalizedCode)
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
	// The concrete type is intentionally private. The filesystem adapter seals a
	// validated value at its trust boundary; arbitrary diagnostic carriers remain
	// unable to grant themselves presentation authority.
	//nolint:errorlint
	if filesystemFailure, ok := cause.(*filesystemOutputFailure); ok && filesystemFailure != nil {
		diagnostic := filesystemFailure.diagnostic
		if !diagnostic.Valid() {
			return appobs.Failure{}, false
		}
		return ProjectNormalizedFault(
			diagnostic.FaultDomain,
			diagnostic.NormalizedScope,
			diagnostic.NormalizedCode,
		)
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

func ProjectFault(value transferfault.Fault) (appobs.Failure, bool) {
	if !value.Valid() {
		return appobs.Failure{}, false
	}
	domain, ok := projectFaultDomain(value.Domain())
	if !ok {
		return appobs.Failure{}, false
	}
	scope, ok := projectFaultScope(value.Scope())
	if !ok {
		return appobs.Failure{}, false
	}
	code, ok := projectFaultCode(value)
	if !ok {
		return appobs.Failure{}, false
	}
	context, err := appobs.NewFaultContext(domain, scope, value.Code())
	if err != nil {
		return appobs.Failure{}, false
	}
	failure, err := appobs.NewFaultFailure(code, context)
	return failure, err == nil
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

func ProjectNormalizedFault(domain uint8, scope uint8, code uint16) (appobs.Failure, bool) {
	projectedDomain, ok := projectFaultDomain(transferfault.Domain(domain))
	if !ok {
		return appobs.Failure{}, false
	}
	projectedScope, ok := projectFaultScope(transferfault.Scope(scope))
	if !ok {
		return appobs.Failure{}, false
	}
	projectedCode, ok := projectNumericFaultCode(transferfault.Domain(domain), code)
	if !ok {
		return appobs.Failure{}, false
	}
	context, err := appobs.NewFaultContext(projectedDomain, projectedScope, code)
	if err != nil {
		return appobs.Failure{}, false
	}
	failure, err := appobs.NewFaultFailure(projectedCode, context)
	return failure, err == nil
}

func projectNumericFaultCode(domain transferfault.Domain, code uint16) (appobs.FailureCode, bool) {
	switch domain {
	case transferfault.DomainSource:
		switch transferfault.SourceCode(code) {
		case transferfault.SourceUnavailable:
			return appobs.FailureSourceUnavailable, true
		case transferfault.SourceRevisionChanged:
			return appobs.FailureSourceRevisionChanged, true
		case transferfault.SourceRevisionInvalidated:
			return appobs.FailureSourceRevisionInvalidated, true
		case transferfault.SourcePermanent:
			return appobs.FailureSourcePermanent, true
		}
	case transferfault.DomainCatalog:
		switch transferfault.CatalogCode(code) {
		case transferfault.CatalogUnavailable:
			return appobs.FailureCatalogUnavailable, true
		case transferfault.CatalogDirectoryStale:
			return appobs.FailureCatalogDirectoryStale, true
		case transferfault.CatalogInvalidGeneration:
			return appobs.FailureCatalogInvalidGeneration, true
		}
	case transferfault.DomainSession:
		switch transferfault.SessionCode(code) {
		case transferfault.SessionTransport:
			return appobs.FailureSessionTransport, true
		case transferfault.SessionProtocol:
			return appobs.FailureSessionProtocol, true
		case transferfault.SessionResourceBudget:
			return appobs.FailureSessionResourceBudget, true
		case transferfault.SessionDependencyContract:
			return appobs.FailureSessionDependencyContract, true
		}
	case transferfault.DomainOutput:
		switch transferfault.OutputCode(code) {
		case transferfault.OutputStateIO:
			return appobs.FailureOutputStateIO, true
		case transferfault.OutputOwnership:
			return appobs.FailureOutputOwnership, true
		case transferfault.OutputNamespaceUnsafe:
			return appobs.FailureOutputNamespaceUnsafe, true
		case transferfault.OutputUnsupportedFilesystem:
			return appobs.FailureOutputUnsupportedFilesystem, true
		case transferfault.OutputDirectoryBinding:
			return appobs.FailureOutputDirectoryBinding, true
		case transferfault.OutputDirectoryMetadata:
			return appobs.FailureOutputDirectoryMetadata, true
		case transferfault.OutputFileAlreadyActive:
			return appobs.FailureOutputFileAlreadyActive, true
		case transferfault.OutputResourceBudget:
			return appobs.FailureOutputResourceBudget, true
		case transferfault.OutputMutationAmbiguous:
			return appobs.FailureOutputMutationAmbiguous, true
		case transferfault.OutputContract:
			return appobs.FailureOutputContract, true
		}
	case transferfault.DomainCheckpoint:
		switch transferfault.CheckpointCode(code) {
		case transferfault.CheckpointBusy:
			return appobs.FailureCheckpointBusy, true
		case transferfault.CheckpointCorruptRecord:
			return appobs.FailureCheckpointCorruptRecord, true
		case transferfault.CheckpointUnsafeInstall:
			return appobs.FailureCheckpointUnsafeInstall, true
		case transferfault.CheckpointOwnershipMismatch:
			return appobs.FailureCheckpointOwnershipMismatch, true
		case transferfault.CheckpointStateIO:
			return appobs.FailureCheckpointStateIO, true
		}
	}
	return 0, false
}

func projectFaultDomain(value transferfault.Domain) (appobs.FaultDomain, bool) {
	switch value {
	case transferfault.DomainSource:
		return appobs.FaultSource, true
	case transferfault.DomainCatalog:
		return appobs.FaultCatalog, true
	case transferfault.DomainSession:
		return appobs.FaultSession, true
	case transferfault.DomainOutput:
		return appobs.FaultOutput, true
	case transferfault.DomainCheckpoint:
		return appobs.FaultCheckpoint, true
	default:
		return 0, false
	}
}

func projectFaultScope(value transferfault.Scope) (appobs.FaultScope, bool) {
	switch value {
	case transferfault.ScopeFileLocal:
		return appobs.FaultFileLocal, true
	case transferfault.ScopeDirectoryLocal:
		return appobs.FaultDirectoryLocal, true
	case transferfault.ScopeOutputPause:
		return appobs.FaultOutputPause, true
	case transferfault.ScopeSessionTerminal:
		return appobs.FaultSessionTerminal, true
	default:
		return 0, false
	}
}

func projectFaultCode(value transferfault.Fault) (appobs.FailureCode, bool) {
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

func projectSourceFaultCode(code transferfault.SourceCode) (appobs.FailureCode, bool) {
	switch code {
	case transferfault.SourceUnavailable:
		return appobs.FailureSourceUnavailable, true
	case transferfault.SourceRevisionChanged:
		return appobs.FailureSourceRevisionChanged, true
	case transferfault.SourceRevisionInvalidated:
		return appobs.FailureSourceRevisionInvalidated, true
	case transferfault.SourcePermanent:
		return appobs.FailureSourcePermanent, true
	default:
		return 0, false
	}
}

func projectCatalogFaultCode(code transferfault.CatalogCode) (appobs.FailureCode, bool) {
	switch code {
	case transferfault.CatalogUnavailable:
		return appobs.FailureCatalogUnavailable, true
	case transferfault.CatalogDirectoryStale:
		return appobs.FailureCatalogDirectoryStale, true
	case transferfault.CatalogInvalidGeneration:
		return appobs.FailureCatalogInvalidGeneration, true
	default:
		return 0, false
	}
}

func projectSessionFaultCode(code transferfault.SessionCode) (appobs.FailureCode, bool) {
	switch code {
	case transferfault.SessionTransport:
		return appobs.FailureSessionTransport, true
	case transferfault.SessionProtocol:
		return appobs.FailureSessionProtocol, true
	case transferfault.SessionResourceBudget:
		return appobs.FailureSessionResourceBudget, true
	case transferfault.SessionDependencyContract:
		return appobs.FailureSessionDependencyContract, true
	default:
		return 0, false
	}
}

func projectOutputFaultCode(code transferfault.OutputCode) (appobs.FailureCode, bool) {
	switch code {
	case transferfault.OutputStateIO:
		return appobs.FailureOutputStateIO, true
	case transferfault.OutputOwnership:
		return appobs.FailureOutputOwnership, true
	case transferfault.OutputNamespaceUnsafe:
		return appobs.FailureOutputNamespaceUnsafe, true
	case transferfault.OutputUnsupportedFilesystem:
		return appobs.FailureOutputUnsupportedFilesystem, true
	case transferfault.OutputDirectoryBinding:
		return appobs.FailureOutputDirectoryBinding, true
	case transferfault.OutputDirectoryMetadata:
		return appobs.FailureOutputDirectoryMetadata, true
	case transferfault.OutputFileAlreadyActive:
		return appobs.FailureOutputFileAlreadyActive, true
	case transferfault.OutputResourceBudget:
		return appobs.FailureOutputResourceBudget, true
	case transferfault.OutputMutationAmbiguous:
		return appobs.FailureOutputMutationAmbiguous, true
	case transferfault.OutputContract:
		return appobs.FailureOutputContract, true
	default:
		return 0, false
	}
}

func projectCheckpointFaultCode(code transferfault.CheckpointCode) (appobs.FailureCode, bool) {
	switch code {
	case transferfault.CheckpointBusy:
		return appobs.FailureCheckpointBusy, true
	case transferfault.CheckpointCorruptRecord:
		return appobs.FailureCheckpointCorruptRecord, true
	case transferfault.CheckpointUnsafeInstall:
		return appobs.FailureCheckpointUnsafeInstall, true
	case transferfault.CheckpointOwnershipMismatch:
		return appobs.FailureCheckpointOwnershipMismatch, true
	case transferfault.CheckpointStateIO:
		return appobs.FailureCheckpointStateIO, true
	default:
		return 0, false
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
