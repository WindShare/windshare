package resumeauthority

import (
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
)

type DiscardAction uint8

const (
	DiscardTransitionAndCleanup DiscardAction = iota + 1
	DiscardCleanup
)

func ReduceRecovery(snapshot Snapshot) (Summary, error) {
	if !snapshot.Valid() {
		return Summary{}, ErrInvalidContract
	}
	state, err := operationState(snapshot.header.record, snapshot.items)
	if err != nil {
		return Summary{}, err
	}
	return newSummary(snapshot.header, state, snapshot.items, false)
}

func ReduceHeader(header Header, busy bool) (Summary, error) {
	if !header.Valid() {
		return Summary{}, ErrInvalidContract
	}
	state, err := operationState(header.record, nil)
	if err != nil {
		return Summary{}, err
	}
	return newSummary(header, state, nil, busy)
}

func operationState(
	record checkpointmodel.OrdinaryOperationRecord,
	items []Item,
) (OperationState, error) {
	if !record.Valid() {
		return 0, ErrInvalidContract
	}
	switch record.Lifecycle() {
	case checkpointmodel.OrdinaryOperationActive:
		for _, item := range items {
			if !item.Valid() {
				return 0, ErrInvalidContract
			}
			if item.State() == ItemResumable {
				return OperationResumable, nil
			}
		}
		return OperationIncomplete, nil
	case checkpointmodel.OrdinaryOperationNeedsAttention:
		return OperationNeedsAttention, nil
	case checkpointmodel.OrdinaryOperationCompleted,
		checkpointmodel.OrdinaryOperationDiscarded,
		checkpointmodel.OrdinaryOperationCleanupPending:
		// Completed and discarded rows exist only at a cleanup crash cut. They are
		// inventory work, never durable terminal history.
		return OperationCleanupPending, nil
	default:
		return 0, ErrInvalidContract
	}
}

func ReduceDiscard(record checkpointmodel.OrdinaryOperationRecord) (DiscardAction, error) {
	if !record.Valid() {
		return 0, ErrInvalidContract
	}
	switch record.Lifecycle() {
	case checkpointmodel.OrdinaryOperationActive,
		checkpointmodel.OrdinaryOperationNeedsAttention:
		return DiscardTransitionAndCleanup, nil
	case checkpointmodel.OrdinaryOperationCompleted,
		checkpointmodel.OrdinaryOperationDiscarded,
		checkpointmodel.OrdinaryOperationCleanupPending:
		return DiscardCleanup, nil
	default:
		return 0, ErrInvalidContract
	}
}
