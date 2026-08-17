package relayv2

import (
	"math"

	"github.com/windshare/windshare/core/framechannel"
)

// LifecycleContractViolation is a closed producer-owned reason. Consumers can
// distinguish an unknown enum from a malformed identity or stage shape without
// independently rebuilding the relay lifecycle matrix.
type LifecycleContractViolation uint8

const (
	LifecycleContractValid LifecycleContractViolation = iota
	LifecycleContractUnknownEnum
	LifecycleContractInvalidIdentity
	LifecycleContractInvalidStageFields
)

// ValidateLifecycleTrace is the sole authority for shapes emitted by relayv2.
// Keeping this beside the constructors prevents diagnostic projections from
// narrowing valid retirement consequences as the transport evolves.
func ValidateLifecycleTrace(event LifecycleTrace) LifecycleContractViolation {
	if !validLifecycleStage(event.Stage) || !validLifecycleDisposition(event.Disposition) ||
		!validLifecycleRetirementSource(event.RetirementSource) ||
		!validLifecycleCause(event.Cause) || !validLifecycleCause(event.DrainCause) {
		return LifecycleContractUnknownEnum
	}
	if event.LinkID == 0 {
		return LifecycleContractInvalidIdentity
	}
	switch event.Stage {
	case LifecycleTraceDropped:
		return validateDroppedLifecycleTrace(event)
	case LifecycleLinkRetiring, LifecycleLinkClosed:
		return validateLinkLifecycleTrace(event)
	default:
		return validateSessionLifecycleTrace(event)
	}
}

func validateDroppedLifecycleTrace(event LifecycleTrace) LifecycleContractViolation {
	if hasLifecycleSession(event) || event.OperationID != 0 {
		return LifecycleContractInvalidIdentity
	}
	if event.Terminal || event.Disposition != 0 ||
		event.RetirementSource != LifecycleRetirementNone ||
		event.Cause != LifecycleCauseNone || event.DrainCause != LifecycleCauseNone ||
		event.Dropped == 0 {
		return LifecycleContractInvalidStageFields
	}
	return LifecycleContractValid
}

func validateLinkLifecycleTrace(event LifecycleTrace) LifecycleContractViolation {
	if hasLifecycleSession(event) || event.OperationID == 0 {
		return LifecycleContractInvalidIdentity
	}
	if event.Terminal || event.Disposition != 0 ||
		event.RetirementSource == LifecycleRetirementNone || event.Dropped != 0 ||
		(event.Stage == LifecycleLinkClosed && event.DrainCause != LifecycleCauseNone) {
		return LifecycleContractInvalidStageFields
	}
	return LifecycleContractValid
}

func validateSessionLifecycleTrace(event LifecycleTrace) LifecycleContractViolation {
	if !hasLifecycleSession(event) || event.OperationID == 0 {
		return LifecycleContractInvalidIdentity
	}
	if event.Dropped != 0 || !validSessionLifecycleStage(event) {
		return LifecycleContractInvalidStageFields
	}
	return LifecycleContractValid
}

func hasLifecycleSession(event LifecycleTrace) bool {
	return event.RelaySessionID != ([len(event.RelaySessionID)]byte{})
}

func validSessionLifecycleStage(event LifecycleTrace) bool {
	switch event.Stage {
	case LifecycleTerminalReserved:
		return event.Terminal && event.Disposition == 0 &&
			event.RetirementSource == LifecycleRetirementNone &&
			event.Cause == LifecycleCauseNone && event.DrainCause == LifecycleCauseNone
	case LifecycleSendAdmitted:
		return event.Disposition == framechannel.SendAccepted &&
			event.RetirementSource == LifecycleRetirementNone && event.DrainCause == LifecycleCauseNone &&
			((event.Terminal && event.Cause == LifecycleCauseNone) ||
				(!event.Terminal && event.Cause != LifecycleCauseNone))
	case LifecycleSendRejected:
		return (event.Disposition == framechannel.SendRejected || event.Disposition == framechannel.SendRetired) &&
			event.RetirementSource == LifecycleRetirementNone && event.Cause != LifecycleCauseNone &&
			event.DrainCause == LifecycleCauseNone
	case LifecycleSendRolledBack:
		return event.Disposition == framechannel.SendRejected &&
			event.RetirementSource == LifecycleRetirementNone && event.Cause != LifecycleCauseNone &&
			event.DrainCause == LifecycleCauseNone
	case LifecycleRetirementDeferred, LifecycleRetired:
		// DrainCause records the competing retirement consequence and therefore
		// may legitimately be non-none.
		return event.Disposition == 0 && event.RetirementSource != LifecycleRetirementNone
	case LifecycleTerminalSettled:
		return event.Terminal && event.Disposition == framechannel.SendAccepted &&
			event.RetirementSource == LifecycleRetirementNone && event.DrainCause == LifecycleCauseNone
	default:
		return false
	}
}

func validLifecycleStage(stage LifecycleStage) bool {
	switch stage {
	case LifecycleTerminalReserved, LifecycleSendAdmitted, LifecycleSendRejected,
		LifecycleSendRolledBack, LifecycleRetirementDeferred, LifecycleRetired,
		LifecycleTerminalSettled, LifecycleLinkRetiring, LifecycleLinkClosed,
		LifecycleTraceDropped:
		return true
	default:
		return false
	}
}

func validLifecycleDisposition(disposition framechannel.SendDisposition) bool {
	return disposition == 0 || disposition == framechannel.SendAccepted ||
		disposition == framechannel.SendRejected || disposition == framechannel.SendRetired
}

func validLifecycleRetirementSource(source LifecycleRetirementSource) bool {
	switch source {
	case LifecycleRetirementNone, LifecycleRetirementLocalClose, LifecycleRetirementTerminal,
		LifecycleRetirementRelaySession, LifecycleRetirementLinkClose,
		LifecycleRetirementLinkFailure, LifecycleRetirementIngressFailure:
		return true
	default:
		return false
	}
}

func validLifecycleCause(cause LifecycleCause) bool {
	switch cause {
	case LifecycleCauseNone, LifecycleCauseCanceled, LifecycleCauseDeadline,
		LifecycleCauseFrameBounds, LifecycleCauseEgressOverflow, LifecycleCauseIngressOverflow,
		LifecycleCauseSessionRetired, LifecycleCauseProtocol, LifecycleCauseClosed,
		LifecycleCauseTransport:
		return true
	default:
		return false
	}
}

// LifecycleObservationLoss is cumulative through the producer admission cut.
// It describes only observations the bounded stream could not retain.
type LifecycleObservationLoss struct {
	CapacityDropped uint64
}

func (loss LifecycleObservationLoss) Total() uint64 {
	return loss.CapacityDropped
}

// LifecycleObservationCompletion proves that stream admission has closed.
// Enqueued does not imply that a consumer received or projected an observation.
type LifecycleObservationCompletion struct {
	Enqueued uint64
	Loss     LifecycleObservationLoss
}

func saturatingLifecycleCount(current, increment uint64) uint64 {
	if math.MaxUint64-current < increment {
		return math.MaxUint64
	}
	return current + increment
}
