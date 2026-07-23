package v2peer

import "github.com/windshare/windshare/core/session/protocolsession"

type ReceiverTerminalOwner string

const (
	ReceiverTerminalUnbound ReceiverTerminalOwner = "unbound"
	ReceiverTerminalLocal   ReceiverTerminalOwner = "local"
	ReceiverTerminalRemote  ReceiverTerminalOwner = "remote"
	ReceiverTerminalRuntime ReceiverTerminalOwner = "runtime"
)

type ReceiverAttemptDisposition string

const (
	ReceiverDispositionFallbackAllowed    ReceiverAttemptDisposition = "fallback_allowed"
	ReceiverDispositionSessionUnavailable ReceiverAttemptDisposition = "session_unavailable"
	ReceiverDispositionSessionUnsafe      ReceiverAttemptDisposition = "session_unsafe"
)

type ReceiverTerminalProvenance string

const (
	ReceiverProvenanceUnbound                               ReceiverTerminalProvenance = "unbound"
	ReceiverProvenanceLocalExplicitStop                     ReceiverTerminalProvenance = "local_explicit_stop"
	ReceiverProvenanceLocalContextEnded                     ReceiverTerminalProvenance = "local_context_ended"
	ReceiverProvenanceLocalNegotiationFailure               ReceiverTerminalProvenance = "local_negotiation_failure"
	ReceiverProvenanceLocalAttemptTimeout                   ReceiverTerminalProvenance = "local_attempt_timeout"
	ReceiverProvenanceLocalOperationContract                ReceiverTerminalProvenance = "local_operation_contract"
	ReceiverProvenanceRemoteOperationRejected               ReceiverTerminalProvenance = "remote_operation_rejected"
	ReceiverProvenanceRemoteUnknownControl                  ReceiverTerminalProvenance = "remote_unknown_control"
	ReceiverProvenanceRemoteControlMalformed                ReceiverTerminalProvenance = "remote_control_malformed"
	ReceiverProvenanceRemoteFailureMalformed                ReceiverTerminalProvenance = "remote_failure_malformed"
	ReceiverProvenanceRemoteFailureScopeViolation           ReceiverTerminalProvenance = "remote_failure_scope_violation"
	ReceiverProvenanceRuntimeStopping                       ReceiverTerminalProvenance = "runtime_stopping"
	ReceiverProvenanceSignalingAdapterContract              ReceiverTerminalProvenance = "signaling_adapter_contract"
	ReceiverProvenanceAuthenticatedSecondAnswer             ReceiverTerminalProvenance = "authenticated_second_answer"
	ReceiverProvenanceAuthenticatedFinalConflict            ReceiverTerminalProvenance = "authenticated_final_conflict"
	ReceiverProvenanceAuthenticatedAnswerBindingMismatch    ReceiverTerminalProvenance = "authenticated_answer_binding_mismatch"
	ReceiverProvenanceAuthenticatedCandidateBindingMismatch ReceiverTerminalProvenance = "authenticated_candidate_binding_mismatch"
	ReceiverProvenanceAuthenticatedContinuationAuthority    ReceiverTerminalProvenance = "authenticated_continuation_authority_violation"
)

type receiverAttemptDecision struct {
	transitionOwner       ReceiverTerminalOwner
	transitionProvenance  ReceiverTerminalProvenance
	disposition           ReceiverAttemptDisposition
	consequenceProvenance ReceiverTerminalProvenance
}

func receiverOperationDecision(
	owner ReceiverTerminalOwner,
	provenance ReceiverTerminalProvenance,
) receiverAttemptDecision {
	return receiverAttemptDecision{
		transitionOwner: owner, transitionProvenance: provenance,
		disposition:           ReceiverDispositionFallbackAllowed,
		consequenceProvenance: provenance,
	}
}

func receiverUnsafeConsequence(provenance ReceiverTerminalProvenance) receiverAttemptDecision {
	return receiverAttemptDecision{
		transitionOwner:       ReceiverTerminalUnbound,
		transitionProvenance:  ReceiverProvenanceUnbound,
		disposition:           ReceiverDispositionSessionUnsafe,
		consequenceProvenance: provenance,
	}
}

func receiverUnavailableDecision() receiverAttemptDecision {
	return receiverAttemptDecision{
		transitionOwner:       ReceiverTerminalRuntime,
		transitionProvenance:  ReceiverProvenanceRuntimeStopping,
		disposition:           ReceiverDispositionSessionUnavailable,
		consequenceProvenance: ReceiverProvenanceRuntimeStopping,
	}
}

func validReceiverBoundDecision(decision receiverAttemptDecision) bool {
	switch decision.transitionOwner {
	case ReceiverTerminalLocal:
		switch decision.transitionProvenance {
		case ReceiverProvenanceLocalExplicitStop,
			ReceiverProvenanceLocalContextEnded,
			ReceiverProvenanceLocalNegotiationFailure,
			ReceiverProvenanceLocalAttemptTimeout,
			ReceiverProvenanceLocalOperationContract,
			ReceiverProvenanceSignalingAdapterContract:
		default:
			return false
		}
	case ReceiverTerminalRemote:
		switch decision.transitionProvenance {
		case ReceiverProvenanceRemoteOperationRejected,
			ReceiverProvenanceRemoteUnknownControl,
			ReceiverProvenanceRemoteControlMalformed,
			ReceiverProvenanceRemoteFailureMalformed,
			ReceiverProvenanceRemoteFailureScopeViolation,
			ReceiverProvenanceAuthenticatedSecondAnswer,
			ReceiverProvenanceAuthenticatedFinalConflict,
			ReceiverProvenanceAuthenticatedContinuationAuthority:
		default:
			return false
		}
	case ReceiverTerminalRuntime:
		if decision.transitionProvenance != ReceiverProvenanceRuntimeStopping {
			return false
		}
	default:
		return false
	}

	switch decision.disposition {
	case ReceiverDispositionFallbackAllowed:
		switch decision.consequenceProvenance {
		case ReceiverProvenanceLocalExplicitStop,
			ReceiverProvenanceLocalContextEnded,
			ReceiverProvenanceLocalNegotiationFailure,
			ReceiverProvenanceLocalAttemptTimeout,
			ReceiverProvenanceLocalOperationContract,
			ReceiverProvenanceSignalingAdapterContract,
			ReceiverProvenanceRemoteOperationRejected,
			ReceiverProvenanceRemoteUnknownControl,
			ReceiverProvenanceRemoteControlMalformed:
			return true
		}
	case ReceiverDispositionSessionUnavailable:
		return decision.consequenceProvenance == ReceiverProvenanceRuntimeStopping
	case ReceiverDispositionSessionUnsafe:
		switch decision.consequenceProvenance {
		case ReceiverProvenanceRemoteFailureMalformed,
			ReceiverProvenanceRemoteFailureScopeViolation,
			ReceiverProvenanceAuthenticatedSecondAnswer,
			ReceiverProvenanceAuthenticatedFinalConflict,
			ReceiverProvenanceAuthenticatedAnswerBindingMismatch,
			ReceiverProvenanceAuthenticatedCandidateBindingMismatch,
			ReceiverProvenanceAuthenticatedContinuationAuthority:
			return true
		}
	}
	return false
}

func mergeReceiverAttemptDecisions(
	first receiverAttemptDecision,
	second receiverAttemptDecision,
) receiverAttemptDecision {
	merged := first
	if (merged.transitionOwner == "" || merged.transitionOwner == ReceiverTerminalUnbound) &&
		second.transitionOwner != "" && second.transitionOwner != ReceiverTerminalUnbound {
		merged.transitionOwner = second.transitionOwner
		merged.transitionProvenance = second.transitionProvenance
	}
	if strongerReceiverDisposition(merged.disposition, second.disposition) {
		merged.disposition = second.disposition
		merged.consequenceProvenance = second.consequenceProvenance
	}
	if merged.disposition == "" {
		merged.disposition = ReceiverDispositionFallbackAllowed
	}
	return merged
}

func strongerReceiverDisposition(
	current ReceiverAttemptDisposition,
	candidate ReceiverAttemptDisposition,
) bool {
	switch candidate {
	case ReceiverDispositionSessionUnsafe:
		return current != ReceiverDispositionSessionUnsafe
	case ReceiverDispositionSessionUnavailable:
		return current == "" || current == ReceiverDispositionFallbackAllowed
	case ReceiverDispositionFallbackAllowed:
		return current == ""
	default:
		return false
	}
}

type ReceiverBenignCause string

const (
	ReceiverBenignContextCanceled        ReceiverBenignCause = "context_canceled"
	ReceiverBenignLocalOperationMissing  ReceiverBenignCause = "local_cancel_operation_missing"
	ReceiverBenignRemoteOperationMissing ReceiverBenignCause = "remote_final_operation_missing"
)

type ReceiverCauseClass string

const (
	ReceiverCauseRuntimeClosed    ReceiverCauseClass = "runtime_closed"
	ReceiverCauseConfiguration    ReceiverCauseClass = "configuration"
	ReceiverCauseOperationMissing ReceiverCauseClass = "operation_missing"
	ReceiverCauseAttemptTimeout   ReceiverCauseClass = "attempt_timeout"
	ReceiverCauseCandidateLimit   ReceiverCauseClass = "candidate_limit"
	ReceiverCauseChannelAdmission ReceiverCauseClass = "channel_admission"
	ReceiverCauseEventCapacity    ReceiverCauseClass = "event_capacity"
	ReceiverCauseNegotiation      ReceiverCauseClass = "negotiation"
	ReceiverCauseProtocol         ReceiverCauseClass = "protocol"
	ReceiverCauseDeadline         ReceiverCauseClass = "deadline_exceeded"
	ReceiverCausePeerShutdown     ReceiverCauseClass = "peer_shutdown"
	ReceiverCauseChannelDrain     ReceiverCauseClass = "channel_drain"
	ReceiverCauseUnknown          ReceiverCauseClass = "unknown"
)

// ReceiverAttemptOutcome is a sealed value. Diagnostic accessors expose only
// immutable errors or defensive slice copies, while lifecycle authority remains
// impossible to manufacture with a public literal.
type ReceiverAttemptOutcome struct {
	operationID          protocolsession.OperationID
	localGeneration      uint64
	decision             receiverAttemptDecision
	cause                error
	retainedCause        error
	benignComponents     []ReceiverBenignCause
	retainedCauseClasses []ReceiverCauseClass
	diagnosticsTruncated bool
}

func newReceiverAttemptOutcome(
	operationID protocolsession.OperationID,
	localGeneration uint64,
	decision receiverAttemptDecision,
	cause error,
	retainedCause error,
	benign []ReceiverBenignCause,
	classes []ReceiverCauseClass,
	diagnosticsTruncated bool,
) ReceiverAttemptOutcome {
	return ReceiverAttemptOutcome{
		operationID: operationID, localGeneration: localGeneration,
		decision: decision, cause: cause, retainedCause: retainedCause,
		benignComponents:     append([]ReceiverBenignCause(nil), benign...),
		retainedCauseClasses: append([]ReceiverCauseClass(nil), classes...),
		diagnosticsTruncated: diagnosticsTruncated,
	}
}

func (outcome ReceiverAttemptOutcome) OperationID() protocolsession.OperationID {
	return outcome.operationID
}

func (outcome ReceiverAttemptOutcome) LocalGeneration() uint64 {
	return outcome.localGeneration
}

func (outcome ReceiverAttemptOutcome) Disposition() ReceiverAttemptDisposition {
	if outcome.decision.disposition == "" {
		return ReceiverDispositionFallbackAllowed
	}
	return outcome.decision.disposition
}

func (outcome ReceiverAttemptOutcome) TransitionAuthority() ReceiverTerminalOwner {
	if outcome.decision.transitionOwner == "" {
		return ReceiverTerminalUnbound
	}
	return outcome.decision.transitionOwner
}

func (outcome ReceiverAttemptOutcome) TransitionProvenance() ReceiverTerminalProvenance {
	if outcome.decision.transitionProvenance == "" {
		return ReceiverProvenanceUnbound
	}
	return outcome.decision.transitionProvenance
}

func (outcome ReceiverAttemptOutcome) ConsequenceProvenance() ReceiverTerminalProvenance {
	if outcome.decision.consequenceProvenance == "" {
		return ReceiverProvenanceUnbound
	}
	return outcome.decision.consequenceProvenance
}

func (outcome ReceiverAttemptOutcome) DiagnosticsTruncated() bool {
	return outcome.diagnosticsTruncated
}

func (outcome ReceiverAttemptOutcome) RequiresSessionClose() bool {
	return outcome.Disposition() == ReceiverDispositionSessionUnsafe
}

func (outcome ReceiverAttemptOutcome) Cause() error {
	return outcome.cause
}

func (outcome ReceiverAttemptOutcome) RetainedCause() error {
	return outcome.retainedCause
}

func (outcome ReceiverAttemptOutcome) BenignComponents() []ReceiverBenignCause {
	return append([]ReceiverBenignCause(nil), outcome.benignComponents...)
}

func (outcome ReceiverAttemptOutcome) RetainedCauseClasses() []ReceiverCauseClass {
	return append([]ReceiverCauseClass(nil), outcome.retainedCauseClasses...)
}

func (outcome ReceiverAttemptOutcome) LocallyCanceled() bool {
	if outcome.Disposition() != ReceiverDispositionFallbackAllowed {
		return false
	}
	return outcome.decision.transitionProvenance == ReceiverProvenanceLocalExplicitStop ||
		outcome.decision.transitionProvenance == ReceiverProvenanceLocalContextEnded
}

func (outcome ReceiverAttemptOutcome) HasRetainedCauseClass(class ReceiverCauseClass) bool {
	for _, retained := range outcome.retainedCauseClasses {
		if retained == class {
			return true
		}
	}
	return false
}

type ReceiverTerminationTrace struct {
	operationID           protocolsession.OperationID
	localGeneration       uint64
	transitionOwner       ReceiverTerminalOwner
	disposition           ReceiverAttemptDisposition
	transitionProvenance  ReceiverTerminalProvenance
	consequenceProvenance ReceiverTerminalProvenance
	diagnosticsTruncated  bool
	benignComponents      []ReceiverBenignCause
	retainedCauseClasses  []ReceiverCauseClass
	teardownTransitions   []PeerTeardownTransition
	peerShutdownFailed    bool
	channelDrainFailed    bool
}

func (trace ReceiverTerminationTrace) OperationID() protocolsession.OperationID {
	return trace.operationID
}

func (trace ReceiverTerminationTrace) LocalGeneration() uint64 {
	return trace.localGeneration
}

func (trace ReceiverTerminationTrace) TransitionAuthority() ReceiverTerminalOwner {
	if trace.transitionOwner == "" {
		return ReceiverTerminalUnbound
	}
	return trace.transitionOwner
}

func (trace ReceiverTerminationTrace) Disposition() ReceiverAttemptDisposition {
	if trace.disposition == "" {
		return ReceiverDispositionFallbackAllowed
	}
	return trace.disposition
}

func (trace ReceiverTerminationTrace) RequiresSessionClose() bool {
	return trace.Disposition() == ReceiverDispositionSessionUnsafe
}

func (trace ReceiverTerminationTrace) TransitionProvenance() ReceiverTerminalProvenance {
	if trace.transitionProvenance == "" {
		return ReceiverProvenanceUnbound
	}
	return trace.transitionProvenance
}

func (trace ReceiverTerminationTrace) ConsequenceProvenance() ReceiverTerminalProvenance {
	if trace.consequenceProvenance == "" {
		return ReceiverProvenanceUnbound
	}
	return trace.consequenceProvenance
}

func (trace ReceiverTerminationTrace) DiagnosticsTruncated() bool {
	return trace.diagnosticsTruncated
}

func (trace ReceiverTerminationTrace) BenignComponents() []ReceiverBenignCause {
	return append([]ReceiverBenignCause(nil), trace.benignComponents...)
}

func (trace ReceiverTerminationTrace) RetainedCauseClasses() []ReceiverCauseClass {
	return append([]ReceiverCauseClass(nil), trace.retainedCauseClasses...)
}

func (trace ReceiverTerminationTrace) TeardownTransitions() []PeerTeardownTransition {
	return append([]PeerTeardownTransition(nil), trace.teardownTransitions...)
}

func (trace ReceiverTerminationTrace) PeerShutdownFailed() bool {
	return trace.peerShutdownFailed
}

func (trace ReceiverTerminationTrace) ChannelDrainFailed() bool {
	return trace.channelDrainFailed
}
