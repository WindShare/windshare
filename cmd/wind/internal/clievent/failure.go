package clievent

import "errors"

var ErrInvalidFailure = errors.New("CLI event failure is invalid")

type SafeMessageKey uint8

const (
	MessageUnexpected SafeMessageKey = iota + 1
	MessageInterrupted
	MessageTimedOut
	MessageInvalidRequest
	MessageCapabilityInvalid
	MessageSelectionMissing
	MessageRelayRejected
	MessageRelayUnavailable
	MessageDirectUnavailable
	MessageSourceUnavailable
	MessageSourceChanged
	MessageCatalogUnavailable
	MessageSessionFailed
	MessageOutputFailed
	MessageCheckpointFailed
	MessagePublicationFailed
	MessageTraceIncomplete
	MessageTraceExists
	MessageOutputNeedsAttention
)

func (value SafeMessageKey) Name() (string, bool) {
	switch value {
	case MessageUnexpected:
		return "unexpected", true
	case MessageInterrupted:
		return "interrupted", true
	case MessageTimedOut:
		return "timed_out", true
	case MessageInvalidRequest:
		return "invalid_request", true
	case MessageCapabilityInvalid:
		return "capability_invalid", true
	case MessageSelectionMissing:
		return "selection_missing", true
	case MessageRelayRejected:
		return "relay_rejected", true
	case MessageRelayUnavailable:
		return "relay_unavailable", true
	case MessageDirectUnavailable:
		return "direct_unavailable", true
	case MessageSourceUnavailable:
		return "source_unavailable", true
	case MessageSourceChanged:
		return "source_changed", true
	case MessageCatalogUnavailable:
		return "catalog_unavailable", true
	case MessageSessionFailed:
		return "session_failed", true
	case MessageOutputFailed:
		return "output_failed", true
	case MessageCheckpointFailed:
		return "checkpoint_failed", true
	case MessagePublicationFailed:
		return "publication_failed", true
	case MessageTraceIncomplete:
		return "trace_incomplete", true
	case MessageTraceExists:
		return "trace_exists", true
	case MessageOutputNeedsAttention:
		return "output_needs_attention", true
	default:
		return "", false
	}
}

type FailureCode uint16

const (
	FailureUnexpected FailureCode = iota + 1
	FailureCanceled
	FailureDeadline
	FailureInvalidInput
	FailureCapabilityInvalid
	FailureSelectionMissing
	FailurePublication
	FailureTraceOpen
	FailureTraceExists
	FailureTraceWrite

	FailureRelayMalformed
	FailureRelayUnsupportedMode
	FailureRelayShareIDCollision
	FailureRelayAlreadyRegistered
	FailureRelayChallengeExpired
	FailureRelayInvalidProof
	FailureRelayDescriptorInvalid
	FailureRelayNotFound
	FailureRelayStarting
	FailureRelayAdmission
	FailureRelayStopped
	FailureRelayProtocol
	FailureRelayClosed
	FailureRelayOverflow
	FailureRelayTransport

	FailurePeerNegotiation
	FailurePeerTimeout
	FailurePeerCandidates
	FailurePeerAdmission
	FailurePeerSignaling
	FailurePeerCanceled
	FailurePeerStopped
	FailurePeerConfiguration
	FailurePeerOperationMissing
	FailurePeerEventCapacity
	FailurePeerShutdown
	FailurePeerChannelDrain
	FailurePeerProtocol

	FailureSourceUnavailable
	FailureSourceRevisionChanged
	FailureSourceRevisionInvalidated
	FailureSourcePermanent
	FailureCatalogUnavailable
	FailureCatalogDirectoryStale
	FailureCatalogInvalidGeneration
	FailureSessionTransport
	FailureSessionProtocol
	FailureSessionResourceBudget
	FailureSessionDependencyContract
	FailureOutputStateIO
	FailureOutputOwnership
	FailureOutputNamespaceUnsafe
	FailureOutputUnsupportedFilesystem
	FailureOutputDirectoryBinding
	FailureOutputDirectoryMetadata
	FailureOutputFileAlreadyActive
	FailureOutputResourceBudget
	FailureOutputMutationAmbiguous
	FailureOutputContract
	FailureOutputNeedsAttention
	FailureCheckpointBusy
	FailureCheckpointCorruptRecord
	FailureCheckpointUnsafeInstall
	FailureCheckpointOwnershipMismatch
	FailureCheckpointStateIO
)

type failureDefinition struct {
	name string
	key  SafeMessageKey
}

func (code FailureCode) definition() (failureDefinition, bool) {
	switch {
	case code >= FailureUnexpected && code <= FailureTraceWrite:
		return commandFailureDefinition(code)
	case code >= FailureRelayMalformed && code <= FailureRelayTransport:
		return relayFailureDefinition(code)
	case code >= FailurePeerNegotiation && code <= FailurePeerProtocol:
		return peerFailureDefinition(code)
	case code >= FailureSourceUnavailable && code <= FailureSessionDependencyContract:
		return coreFailureDefinition(code)
	case code >= FailureOutputStateIO && code <= FailureCheckpointStateIO:
		return outputFailureDefinition(code)
	default:
		return failureDefinition{}, false
	}
}

func commandFailureDefinition(code FailureCode) (failureDefinition, bool) {
	switch code {
	case FailureUnexpected:
		return failureDefinition{"unexpected", MessageUnexpected}, true
	case FailureCanceled:
		return failureDefinition{"canceled", MessageInterrupted}, true
	case FailureDeadline:
		return failureDefinition{"deadline", MessageTimedOut}, true
	case FailureInvalidInput:
		return failureDefinition{"invalid_input", MessageInvalidRequest}, true
	case FailureCapabilityInvalid:
		return failureDefinition{"capability_invalid", MessageCapabilityInvalid}, true
	case FailureSelectionMissing:
		return failureDefinition{"selection_missing", MessageSelectionMissing}, true
	case FailurePublication:
		return failureDefinition{"capability_publication", MessagePublicationFailed}, true
	case FailureTraceOpen:
		return failureDefinition{"trace_open", MessageTraceIncomplete}, true
	case FailureTraceExists:
		return failureDefinition{"trace_exists", MessageTraceExists}, true
	case FailureTraceWrite:
		return failureDefinition{"trace_write", MessageTraceIncomplete}, true
	default:
		return failureDefinition{}, false
	}
}

func relayFailureDefinition(code FailureCode) (failureDefinition, bool) {
	switch code {
	case FailureRelayMalformed:
		return failureDefinition{"relay_malformed", MessageRelayRejected}, true
	case FailureRelayUnsupportedMode:
		return failureDefinition{"relay_unsupported_mode", MessageRelayRejected}, true
	case FailureRelayShareIDCollision:
		return failureDefinition{"relay_share_id_collision", MessageRelayRejected}, true
	case FailureRelayAlreadyRegistered:
		return failureDefinition{"relay_already_registered", MessageRelayRejected}, true
	case FailureRelayChallengeExpired:
		return failureDefinition{"relay_challenge_expired", MessageRelayRejected}, true
	case FailureRelayInvalidProof:
		return failureDefinition{"relay_invalid_proof", MessageRelayRejected}, true
	case FailureRelayDescriptorInvalid:
		return failureDefinition{"relay_descriptor_invalid", MessageRelayRejected}, true
	case FailureRelayNotFound:
		return failureDefinition{"relay_not_found", MessageRelayRejected}, true
	case FailureRelayStarting:
		return failureDefinition{"relay_starting", MessageRelayUnavailable}, true
	case FailureRelayAdmission:
		return failureDefinition{"relay_admission", MessageRelayUnavailable}, true
	case FailureRelayStopped:
		return failureDefinition{"relay_stopped", MessageRelayUnavailable}, true
	case FailureRelayProtocol:
		return failureDefinition{"relay_protocol", MessageRelayUnavailable}, true
	case FailureRelayClosed:
		return failureDefinition{"relay_closed", MessageRelayUnavailable}, true
	case FailureRelayOverflow:
		return failureDefinition{"relay_overflow", MessageRelayUnavailable}, true
	case FailureRelayTransport:
		return failureDefinition{"relay_transport", MessageRelayUnavailable}, true
	default:
		return failureDefinition{}, false
	}
}

func peerFailureDefinition(code FailureCode) (failureDefinition, bool) {
	switch code {
	case FailurePeerNegotiation:
		return failureDefinition{"peer_negotiation", MessageDirectUnavailable}, true
	case FailurePeerTimeout:
		return failureDefinition{"peer_timeout", MessageDirectUnavailable}, true
	case FailurePeerCandidates:
		return failureDefinition{"peer_candidates", MessageDirectUnavailable}, true
	case FailurePeerAdmission:
		return failureDefinition{"peer_admission", MessageDirectUnavailable}, true
	case FailurePeerSignaling:
		return failureDefinition{"peer_signaling", MessageDirectUnavailable}, true
	case FailurePeerCanceled:
		return failureDefinition{"peer_canceled", MessageInterrupted}, true
	case FailurePeerStopped:
		return failureDefinition{"peer_stopped", MessageDirectUnavailable}, true
	case FailurePeerConfiguration:
		return failureDefinition{"peer_configuration", MessageDirectUnavailable}, true
	case FailurePeerOperationMissing:
		return failureDefinition{"peer_operation_missing", MessageDirectUnavailable}, true
	case FailurePeerEventCapacity:
		return failureDefinition{"peer_event_capacity", MessageDirectUnavailable}, true
	case FailurePeerShutdown:
		return failureDefinition{"peer_shutdown", MessageDirectUnavailable}, true
	case FailurePeerChannelDrain:
		return failureDefinition{"peer_channel_drain", MessageDirectUnavailable}, true
	case FailurePeerProtocol:
		return failureDefinition{"peer_protocol", MessageDirectUnavailable}, true
	default:
		return failureDefinition{}, false
	}
}

func coreFailureDefinition(code FailureCode) (failureDefinition, bool) {
	switch code {
	case FailureSourceUnavailable:
		return failureDefinition{"source_unavailable", MessageSourceUnavailable}, true
	case FailureSourceRevisionChanged:
		return failureDefinition{"source_revision_changed", MessageSourceChanged}, true
	case FailureSourceRevisionInvalidated:
		return failureDefinition{"source_revision_invalidated", MessageSourceChanged}, true
	case FailureSourcePermanent:
		return failureDefinition{"source_permanent", MessageSourceUnavailable}, true
	case FailureCatalogUnavailable:
		return failureDefinition{"catalog_unavailable", MessageCatalogUnavailable}, true
	case FailureCatalogDirectoryStale:
		return failureDefinition{"catalog_directory_stale", MessageSourceChanged}, true
	case FailureCatalogInvalidGeneration:
		return failureDefinition{"catalog_invalid_generation", MessageCatalogUnavailable}, true
	case FailureSessionTransport:
		return failureDefinition{"session_transport", MessageSessionFailed}, true
	case FailureSessionProtocol:
		return failureDefinition{"session_protocol", MessageSessionFailed}, true
	case FailureSessionResourceBudget:
		return failureDefinition{"session_resource_budget", MessageSessionFailed}, true
	case FailureSessionDependencyContract:
		return failureDefinition{"session_dependency_contract", MessageSessionFailed}, true
	default:
		return failureDefinition{}, false
	}
}

func outputFailureDefinition(code FailureCode) (failureDefinition, bool) {
	switch code {
	case FailureOutputStateIO:
		return failureDefinition{"output_state_io", MessageOutputFailed}, true
	case FailureOutputOwnership:
		return failureDefinition{"output_ownership", MessageOutputFailed}, true
	case FailureOutputNamespaceUnsafe:
		return failureDefinition{"output_namespace_unsafe", MessageOutputFailed}, true
	case FailureOutputUnsupportedFilesystem:
		return failureDefinition{"output_unsupported_filesystem", MessageOutputFailed}, true
	case FailureOutputDirectoryBinding:
		return failureDefinition{"output_directory_binding", MessageOutputFailed}, true
	case FailureOutputDirectoryMetadata:
		return failureDefinition{"output_directory_metadata", MessageOutputFailed}, true
	case FailureOutputFileAlreadyActive:
		return failureDefinition{"output_file_already_active", MessageOutputFailed}, true
	case FailureOutputResourceBudget:
		return failureDefinition{"output_resource_budget", MessageOutputFailed}, true
	case FailureOutputMutationAmbiguous:
		return failureDefinition{"output_mutation_ambiguous", MessageOutputFailed}, true
	case FailureOutputContract:
		return failureDefinition{"output_contract", MessageOutputFailed}, true
	case FailureOutputNeedsAttention:
		return failureDefinition{"output_needs_attention", MessageOutputNeedsAttention}, true
	case FailureCheckpointBusy:
		return failureDefinition{"checkpoint_busy", MessageCheckpointFailed}, true
	case FailureCheckpointCorruptRecord:
		return failureDefinition{"checkpoint_corrupt_record", MessageCheckpointFailed}, true
	case FailureCheckpointUnsafeInstall:
		return failureDefinition{"checkpoint_unsafe_install", MessageCheckpointFailed}, true
	case FailureCheckpointOwnershipMismatch:
		return failureDefinition{"checkpoint_ownership_mismatch", MessageCheckpointFailed}, true
	case FailureCheckpointStateIO:
		return failureDefinition{"checkpoint_state_io", MessageCheckpointFailed}, true
	default:
		return failureDefinition{}, false
	}
}

func (code FailureCode) Name() (string, bool) {
	definition, ok := code.definition()
	return definition.name, ok
}

func (code FailureCode) MessageKey() (SafeMessageKey, bool) {
	definition, ok := code.definition()
	return definition.key, ok
}

type FaultDomain uint8

const (
	FaultSource FaultDomain = iota + 1
	FaultCatalog
	FaultSession
	FaultOutput
	FaultCheckpoint
)

func (value FaultDomain) Name() (string, bool) {
	switch value {
	case FaultSource:
		return "source", true
	case FaultCatalog:
		return "catalog", true
	case FaultSession:
		return "session", true
	case FaultOutput:
		return "output", true
	case FaultCheckpoint:
		return "checkpoint", true
	default:
		return "", false
	}
}

type FaultScope uint8

const (
	FaultFileLocal FaultScope = iota + 1
	FaultDirectoryLocal
	FaultOutputPause
	FaultSessionTerminal
)

func (value FaultScope) Name() (string, bool) {
	switch value {
	case FaultFileLocal:
		return "file_local", true
	case FaultDirectoryLocal:
		return "directory_local", true
	case FaultOutputPause:
		return "output_pause", true
	case FaultSessionTerminal:
		return "session_terminal", true
	default:
		return "", false
	}
}

type FaultContext struct {
	domain FaultDomain
	scope  FaultScope
	code   uint16
}

func NewFaultContext(domain FaultDomain, scope FaultScope, code uint16) (FaultContext, error) {
	if _, ok := domain.Name(); !ok || code == 0 {
		return FaultContext{}, ErrInvalidFailure
	}
	if _, ok := scope.Name(); !ok {
		return FaultContext{}, ErrInvalidFailure
	}
	return FaultContext{domain: domain, scope: scope, code: code}, nil
}

func (context FaultContext) Domain() FaultDomain { return context.domain }
func (context FaultContext) Scope() FaultScope   { return context.scope }
func (context FaultContext) Code() uint16        { return context.code }
func (context FaultContext) Valid() bool {
	_, domainOK := context.domain.Name()
	_, scopeOK := context.scope.Name()
	return domainOK && scopeOK && context.code != 0
}

type Failure struct {
	code             FailureCode
	fault            FaultContext
	hasFault         bool
	retryAfterMillis uint64
	hasRetryAfter    bool
}

func NewFailure(code FailureCode) (Failure, error) {
	if _, ok := code.definition(); !ok {
		return Failure{}, ErrInvalidFailure
	}
	return Failure{code: code}, nil
}

func NewFaultFailure(code FailureCode, context FaultContext) (Failure, error) {
	failure, err := NewFailure(code)
	wantDomain, wantCode, isFault := failureFaultDefinition(code)
	if err != nil || !context.Valid() || !isFault ||
		context.domain != wantDomain || context.code != wantCode {
		return Failure{}, ErrInvalidFailure
	}
	failure.fault, failure.hasFault = context, true
	return failure, nil
}

func NewRetryableFailure(code FailureCode, retryAfterMillis uint64) (Failure, error) {
	failure, err := NewFailure(code)
	if err != nil || retryAfterMillis == 0 ||
		code != FailureRelayStarting && code != FailureRelayAdmission {
		return Failure{}, ErrInvalidFailure
	}
	failure.retryAfterMillis, failure.hasRetryAfter = retryAfterMillis, true
	return failure, nil
}

func (failure Failure) Code() FailureCode { return failure.code }

func (failure Failure) MessageKey() (SafeMessageKey, bool) {
	return failure.code.MessageKey()
}

func (failure Failure) Fault() (FaultContext, bool) {
	return failure.fault, failure.hasFault && failure.fault.Valid()
}

func (failure Failure) RetryAfterMillis() (uint64, bool) {
	return failure.retryAfterMillis, failure.hasRetryAfter && failure.retryAfterMillis != 0
}

func (failure Failure) Valid() bool {
	if _, ok := failure.code.definition(); !ok {
		return false
	}
	if failure.hasFault {
		wantDomain, wantCode, ok := failureFaultDefinition(failure.code)
		if !ok || !failure.fault.Valid() || failure.fault.domain != wantDomain ||
			failure.fault.code != wantCode || failure.hasRetryAfter {
			return false
		}
	}
	if failure.hasRetryAfter && (failure.retryAfterMillis == 0 ||
		failure.code != FailureRelayStarting && failure.code != FailureRelayAdmission) {
		return false
	}
	return true
}

func failureFaultDefinition(code FailureCode) (FaultDomain, uint16, bool) {
	switch code {
	case FailureSourceUnavailable:
		return FaultSource, 1, true
	case FailureSourceRevisionChanged:
		return FaultSource, 2, true
	case FailureSourceRevisionInvalidated:
		return FaultSource, 3, true
	case FailureSourcePermanent:
		return FaultSource, 4, true
	case FailureCatalogUnavailable:
		return FaultCatalog, 1, true
	case FailureCatalogDirectoryStale:
		return FaultCatalog, 2, true
	case FailureCatalogInvalidGeneration:
		return FaultCatalog, 3, true
	case FailureSessionTransport:
		return FaultSession, 1, true
	case FailureSessionProtocol:
		return FaultSession, 2, true
	case FailureSessionResourceBudget:
		return FaultSession, 3, true
	case FailureSessionDependencyContract:
		return FaultSession, 4, true
	case FailureOutputStateIO:
		return FaultOutput, 1, true
	case FailureOutputOwnership:
		return FaultOutput, 2, true
	case FailureOutputNamespaceUnsafe:
		return FaultOutput, 3, true
	case FailureOutputUnsupportedFilesystem:
		return FaultOutput, 4, true
	case FailureOutputDirectoryBinding:
		return FaultOutput, 5, true
	case FailureOutputDirectoryMetadata:
		return FaultOutput, 6, true
	case FailureOutputFileAlreadyActive:
		return FaultOutput, 7, true
	case FailureOutputResourceBudget:
		return FaultOutput, 8, true
	case FailureOutputMutationAmbiguous:
		return FaultOutput, 9, true
	case FailureOutputContract:
		return FaultOutput, 10, true
	case FailureCheckpointBusy:
		return FaultCheckpoint, 1, true
	case FailureCheckpointCorruptRecord:
		return FaultCheckpoint, 2, true
	case FailureCheckpointUnsafeInstall:
		return FaultCheckpoint, 3, true
	case FailureCheckpointOwnershipMismatch:
		return FaultCheckpoint, 4, true
	case FailureCheckpointStateIO:
		return FaultCheckpoint, 5, true
	default:
		return 0, 0, false
	}
}
