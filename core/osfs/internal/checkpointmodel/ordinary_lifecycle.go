package checkpointmodel

import "errors"

var ErrInvalidOrdinaryLifecycle = errors.New("ordinary operation lifecycle transition is invalid")

type OrdinaryOperationLifecycle uint8

const (
	OrdinaryOperationActive OrdinaryOperationLifecycle = iota + 1
	OrdinaryOperationNeedsAttention
	OrdinaryOperationCompleted
	OrdinaryOperationDiscarded
	OrdinaryOperationCleanupPending
)

func (state OrdinaryOperationLifecycle) Valid() bool {
	return state >= OrdinaryOperationActive && state <= OrdinaryOperationCleanupPending
}

func (state OrdinaryOperationLifecycle) String() string {
	switch state {
	case OrdinaryOperationActive:
		return "active"
	case OrdinaryOperationNeedsAttention:
		return "operation-needs-attention"
	case OrdinaryOperationCompleted:
		return "completed"
	case OrdinaryOperationDiscarded:
		return "discarded"
	case OrdinaryOperationCleanupPending:
		return "cleanup-pending"
	default:
		return ""
	}
}

func (state OrdinaryOperationLifecycle) ParticipatesInActiveLookup() bool {
	return state == OrdinaryOperationActive || state == OrdinaryOperationNeedsAttention
}

type OrdinaryLeaseState uint8

const (
	OrdinaryLeaseReleased OrdinaryLeaseState = iota + 1
	OrdinaryLeaseHeld
)

func (state OrdinaryLeaseState) Valid() bool {
	return state == OrdinaryLeaseReleased || state == OrdinaryLeaseHeld
}

type OrdinaryClosedReason uint8

const (
	OrdinaryReasonNone OrdinaryClosedReason = iota + 1
	OrdinaryReasonDestinationOwnershipUnknown
	OrdinaryReasonRegistryOwnershipUnknown
	OrdinaryReasonLeaseOwnershipUnknown
	OrdinaryReasonOperationOwnershipUnknown
	OrdinaryReasonCleanupUncertain
)

func (reason OrdinaryClosedReason) Valid() bool {
	return reason >= OrdinaryReasonNone && reason <= OrdinaryReasonCleanupUncertain
}

func (reason OrdinaryClosedReason) IsAttentionReason() bool {
	return reason >= OrdinaryReasonDestinationOwnershipUnknown &&
		reason <= OrdinaryReasonOperationOwnershipUnknown
}

func (reason OrdinaryClosedReason) IsCleanupReason() bool {
	return reason == OrdinaryReasonCleanupUncertain
}

func (reason OrdinaryClosedReason) String() string {
	switch reason {
	case OrdinaryReasonNone:
		return "none"
	case OrdinaryReasonDestinationOwnershipUnknown:
		return "destination-ownership-unknown"
	case OrdinaryReasonRegistryOwnershipUnknown:
		return "registry-ownership-unknown"
	case OrdinaryReasonLeaseOwnershipUnknown:
		return "lease-ownership-unknown"
	case OrdinaryReasonOperationOwnershipUnknown:
		return "operation-ownership-unknown"
	case OrdinaryReasonCleanupUncertain:
		return "cleanup-uncertain"
	default:
		return ""
	}
}

type OrdinaryLifecycleEvent uint8

const (
	OrdinaryLifecycleContinue OrdinaryLifecycleEvent = iota + 1
	OrdinaryLifecycleRequireAttention
	OrdinaryLifecycleComplete
	OrdinaryLifecycleDiscard
	OrdinaryLifecycleCleanupFailed
	OrdinaryLifecycleCleanupFinished
)

func (event OrdinaryLifecycleEvent) Valid() bool {
	return event >= OrdinaryLifecycleContinue && event <= OrdinaryLifecycleCleanupFinished
}

// ReduceOrdinaryOperationLifecycle owns the five-state persistent vocabulary.
// Item collisions, permission failures, blocked files, and session pauses emit
// Continue; only authority uncertainty may emit RequireAttention.
func ReduceOrdinaryOperationLifecycle(
	current OrdinaryOperationLifecycle,
	event OrdinaryLifecycleEvent,
	reason OrdinaryClosedReason,
) (OrdinaryOperationLifecycle, OrdinaryClosedReason, error) {
	if !current.Valid() || !event.Valid() || !reason.Valid() {
		return 0, 0, ErrInvalidOrdinaryLifecycle
	}
	switch event {
	case OrdinaryLifecycleContinue:
		if current != OrdinaryOperationActive || reason != OrdinaryReasonNone {
			return 0, 0, ErrInvalidOrdinaryLifecycle
		}
		return OrdinaryOperationActive, OrdinaryReasonNone, nil
	case OrdinaryLifecycleRequireAttention:
		if current != OrdinaryOperationActive || !reason.IsAttentionReason() {
			return 0, 0, ErrInvalidOrdinaryLifecycle
		}
		return OrdinaryOperationNeedsAttention, reason, nil
	case OrdinaryLifecycleComplete:
		if current != OrdinaryOperationActive || reason != OrdinaryReasonNone {
			return 0, 0, ErrInvalidOrdinaryLifecycle
		}
		return OrdinaryOperationCompleted, OrdinaryReasonNone, nil
	case OrdinaryLifecycleDiscard:
		if current != OrdinaryOperationActive && current != OrdinaryOperationNeedsAttention || reason != OrdinaryReasonNone {
			return 0, 0, ErrInvalidOrdinaryLifecycle
		}
		return OrdinaryOperationDiscarded, OrdinaryReasonNone, nil
	case OrdinaryLifecycleCleanupFailed:
		if current != OrdinaryOperationCompleted && current != OrdinaryOperationDiscarded || !reason.IsCleanupReason() {
			return 0, 0, ErrInvalidOrdinaryLifecycle
		}
		return OrdinaryOperationCleanupPending, reason, nil
	case OrdinaryLifecycleCleanupFinished:
		if current != OrdinaryOperationCleanupPending || reason != OrdinaryReasonNone {
			return 0, 0, ErrInvalidOrdinaryLifecycle
		}
		// A cleaned terminal record is deleted by the registry, so no durable next
		// state exists. Returning zero prevents terminal history from being retained.
		return 0, OrdinaryReasonNone, nil
	default:
		return 0, 0, ErrInvalidOrdinaryLifecycle
	}
}
