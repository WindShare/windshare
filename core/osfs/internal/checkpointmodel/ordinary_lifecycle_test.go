package checkpointmodel

import (
	"errors"
	"testing"
)

func TestOrdinaryLifecycleUsesExactlyFivePersistentStates(t *testing.T) {
	states := []OrdinaryOperationLifecycle{
		OrdinaryOperationActive,
		OrdinaryOperationNeedsAttention,
		OrdinaryOperationCompleted,
		OrdinaryOperationDiscarded,
		OrdinaryOperationCleanupPending,
	}
	for _, state := range states {
		if !state.Valid() || state.String() == "" {
			t.Fatalf("state %v is invalid", state)
		}
	}
	if OrdinaryOperationNeedsAttention.String() != "operation-needs-attention" ||
		OrdinaryReasonCleanupUncertain.String() != "cleanup-uncertain" {
		t.Fatal("stable ordinary lifecycle vocabulary drifted")
	}
	if OrdinaryOperationLifecycle(0).Valid() || OrdinaryOperationLifecycle(6).Valid() {
		t.Fatal("ordinary lifecycle is an open union")
	}
	if !OrdinaryOperationActive.ParticipatesInActiveLookup() ||
		!OrdinaryOperationNeedsAttention.ParticipatesInActiveLookup() ||
		OrdinaryOperationCompleted.ParticipatesInActiveLookup() ||
		OrdinaryOperationDiscarded.ParticipatesInActiveLookup() ||
		OrdinaryOperationCleanupPending.ParticipatesInActiveLookup() {
		t.Fatal("terminal lifecycle participated in active lookup")
	}
}

func TestOrdinaryLifecycleReducerSeparatesAttentionAndCleanup(t *testing.T) {
	active, activeReason, err := ReduceOrdinaryOperationLifecycle(
		OrdinaryOperationActive, OrdinaryLifecycleContinue, OrdinaryReasonNone,
	)
	if err != nil || active != OrdinaryOperationActive || activeReason != OrdinaryReasonNone {
		t.Fatalf("continue transition = %v/%v/%v", active, activeReason, err)
	}
	completed, completedReason, err := ReduceOrdinaryOperationLifecycle(
		OrdinaryOperationActive, OrdinaryLifecycleComplete, OrdinaryReasonNone,
	)
	if err != nil || completed != OrdinaryOperationCompleted || completedReason != OrdinaryReasonNone {
		t.Fatalf("complete transition = %v/%v/%v", completed, completedReason, err)
	}
	attention, reason, err := ReduceOrdinaryOperationLifecycle(
		OrdinaryOperationActive, OrdinaryLifecycleRequireAttention,
		OrdinaryReasonLeaseOwnershipUnknown,
	)
	if err != nil || attention != OrdinaryOperationNeedsAttention || reason != OrdinaryReasonLeaseOwnershipUnknown {
		t.Fatalf("attention transition = %v/%v/%v", attention, reason, err)
	}
	discarded, reason, err := ReduceOrdinaryOperationLifecycle(
		attention, OrdinaryLifecycleDiscard, OrdinaryReasonNone,
	)
	if err != nil || discarded != OrdinaryOperationDiscarded || reason != OrdinaryReasonNone {
		t.Fatalf("discard transition = %v/%v/%v", discarded, reason, err)
	}
	pending, reason, err := ReduceOrdinaryOperationLifecycle(
		discarded, OrdinaryLifecycleCleanupFailed, OrdinaryReasonCleanupUncertain,
	)
	if err != nil || pending != OrdinaryOperationCleanupPending || reason != OrdinaryReasonCleanupUncertain {
		t.Fatalf("cleanup-pending transition = %v/%v/%v", pending, reason, err)
	}
	deleted, reason, err := ReduceOrdinaryOperationLifecycle(
		pending, OrdinaryLifecycleCleanupFinished, OrdinaryReasonNone,
	)
	if err != nil || deleted != 0 || reason != OrdinaryReasonNone {
		t.Fatalf("cleanup-finished transition = %v/%v/%v", deleted, reason, err)
	}
	if _, _, err := ReduceOrdinaryOperationLifecycle(
		OrdinaryOperationActive, OrdinaryLifecycleRequireAttention, OrdinaryReasonCleanupUncertain,
	); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatalf("cleanup reason became attention = %v", err)
	}
	if _, _, err := ReduceOrdinaryOperationLifecycle(
		OrdinaryOperationCompleted, OrdinaryLifecycleContinue, OrdinaryReasonNone,
	); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatalf("terminal operation reopened = %v", err)
	}
}
