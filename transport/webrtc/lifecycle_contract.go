package webrtc

import (
	"math"

	"github.com/windshare/windshare/core/framechannel"
)

type LifecycleContractViolation uint8

const (
	LifecycleContractValid LifecycleContractViolation = iota
	LifecycleContractUnknownEnum
	LifecycleContractInvalidIdentity
	LifecycleContractInvalidStageFields
)

// ValidateLifecycleTrace owns the complete matrix emitted by the WebRTC
// lifecycle. Refusals use the state at admission, so connecting and closed are
// valid evidence rather than projection defects.
func ValidateLifecycleTrace(event LifecycleTrace) LifecycleContractViolation {
	if !validLifecycleOperation(event.Operation) || !validLifecycleTransition(event.Transition) ||
		!validLifecycleDisposition(event.Disposition) || !validLifecycleState(event.State) ||
		!validLifecycleTerminal(event.Terminal) || !validLifecycleCauseValue(event.Cause) {
		return LifecycleContractUnknownEnum
	}
	if event.ChannelID == 0 {
		return LifecycleContractInvalidIdentity
	}
	if event.Transition == LifecycleTransitionTraceDropped {
		return validateDroppedLifecycleTrace(event)
	}
	if event.Dropped != 0 || !validLifecycleTransitionShape(event) {
		return LifecycleContractInvalidStageFields
	}
	return LifecycleContractValid
}

func validateDroppedLifecycleTrace(event LifecycleTrace) LifecycleContractViolation {
	if event.OperationID != 0 {
		return LifecycleContractInvalidIdentity
	}
	if event.Operation != LifecycleOperationChannel || event.Disposition != 0 ||
		event.Cause != LifecycleCauseNone || event.Dropped == 0 {
		return LifecycleContractInvalidStageFields
	}
	return LifecycleContractValid
}

func validLifecycleTransitionShape(event LifecycleTrace) bool {
	switch event.Transition {
	case LifecycleTransitionSendAccepted:
		return validSendAcceptedLifecycleTrace(event)
	case LifecycleTransitionSendRejected:
		return validSendRejectedLifecycleTrace(event)
	case LifecycleTransitionSendRetired:
		return validSendRetiredLifecycleTrace(event)
	case LifecycleTransitionRemoteTerminalReserved:
		return validRemoteTerminalLifecycleTrace(event)
	case LifecycleTransitionTerminationPending:
		return validTerminationPendingLifecycleTrace(event)
	case LifecycleTransitionClosedClean:
		return validClosedCleanLifecycleTrace(event)
	case LifecycleTransitionClosedFailed:
		return validClosedFailedLifecycleTrace(event)
	default:
		return false
	}
}

func isLifecycleSendOperation(operation LifecycleOperation) bool {
	return operation == LifecycleOperationSend || operation == LifecycleOperationSendTerminal
}

func validSendAcceptedLifecycleTrace(event LifecycleTrace) bool {
	if !isLifecycleSendOperation(event.Operation) || event.OperationID == 0 ||
		event.Disposition != framechannel.SendAccepted || event.State != framechannel.Open {
		return false
	}
	validTerminal := event.Operation == LifecycleOperationSendTerminal &&
		event.Terminal == LifecycleTerminalLocalPending && event.Cause == LifecycleCauseNone
	validOrdinaryFailure := event.Operation == LifecycleOperationSend &&
		event.Terminal == LifecycleTerminalNone && event.Cause != LifecycleCauseNone
	return validTerminal || validOrdinaryFailure
}

func validSendRejectedLifecycleTrace(event LifecycleTrace) bool {
	return isLifecycleSendOperation(event.Operation) && event.OperationID != 0 &&
		event.Disposition == framechannel.SendRejected && event.Cause != LifecycleCauseNone
}

func validSendRetiredLifecycleTrace(event LifecycleTrace) bool {
	return isLifecycleSendOperation(event.Operation) && event.OperationID != 0 &&
		event.Disposition == framechannel.SendRetired && event.Cause == LifecycleCauseNaturalRetirement
}

func validRemoteTerminalLifecycleTrace(event LifecycleTrace) bool {
	return event.Operation == LifecycleOperationChannel && event.OperationID == 0 &&
		event.Disposition == 0 && event.State == framechannel.Open &&
		event.Terminal == LifecycleTerminalRemotePending && event.Cause == LifecycleCauseNone
}

func validTerminationPendingLifecycleTrace(event LifecycleTrace) bool {
	return event.Operation == LifecycleOperationChannel && event.OperationID == 0 &&
		event.Disposition == 0 && event.State != framechannel.Closed && event.Cause != LifecycleCauseNone
}

func validClosedCleanLifecycleTrace(event LifecycleTrace) bool {
	return event.Operation == LifecycleOperationChannel && event.OperationID == 0 &&
		event.Disposition == 0 && event.State == framechannel.Closed && event.Cause == LifecycleCauseNone
}

func validClosedFailedLifecycleTrace(event LifecycleTrace) bool {
	return event.Operation == LifecycleOperationChannel && event.OperationID == 0 &&
		event.Disposition == 0 && event.State == framechannel.Closed && event.Cause != LifecycleCauseNone
}

func validLifecycleOperation(operation LifecycleOperation) bool {
	return operation == LifecycleOperationChannel || operation == LifecycleOperationSend ||
		operation == LifecycleOperationSendTerminal
}

func validLifecycleTransition(transition LifecycleTransition) bool {
	switch transition {
	case LifecycleTransitionSendAccepted, LifecycleTransitionSendRejected,
		LifecycleTransitionSendRetired, LifecycleTransitionRemoteTerminalReserved,
		LifecycleTransitionTerminationPending, LifecycleTransitionClosedClean,
		LifecycleTransitionClosedFailed, LifecycleTransitionTraceDropped:
		return true
	default:
		return false
	}
}

func validLifecycleDisposition(disposition framechannel.SendDisposition) bool {
	return disposition == 0 || disposition == framechannel.SendAccepted ||
		disposition == framechannel.SendRejected || disposition == framechannel.SendRetired
}

func validLifecycleState(state framechannel.ChannelState) bool {
	return state == framechannel.Connecting || state == framechannel.Open || state == framechannel.Closed
}

func validLifecycleTerminal(terminal LifecycleTerminalState) bool {
	return terminal == LifecycleTerminalNone || terminal == LifecycleTerminalLocalPending ||
		terminal == LifecycleTerminalRemotePending
}

func validLifecycleCauseValue(cause LifecycleCause) bool {
	switch cause {
	case LifecycleCauseNone, LifecycleCauseCanceled, LifecycleCauseDeadline,
		LifecycleCauseNotOpen, LifecycleCauseNaturalRetirement, LifecycleCauseRemoteClosed,
		LifecycleCauseTerminalUnacknowledged, LifecycleCausePeerProtocol,
		LifecycleCauseTransport, LifecycleCauseOther:
		return true
	default:
		return false
	}
}

// LifecycleObservationLoss describes only observations the bounded stream
// could not retain before the producer admission cut.
type LifecycleObservationLoss struct {
	CapacityDropped uint64
}

func (loss LifecycleObservationLoss) Total() uint64 {
	return loss.CapacityDropped
}

// LifecycleObservationCompletion proves stream admission has closed. Enqueued
// does not imply that a consumer received or projected an observation.
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
