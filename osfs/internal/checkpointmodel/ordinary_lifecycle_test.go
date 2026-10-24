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

func TestOrdinaryClosedReasonVocabularyAndPredicates(t *testing.T) {
	reasons := []struct {
		reason      OrdinaryClosedReason
		wantStr     string
		isAttention bool
		isCleanup   bool
	}{
		{OrdinaryReasonNone, "none", false, false},
		{OrdinaryReasonDestinationOwnershipUnknown, "destination-ownership-unknown", true, false},
		{OrdinaryReasonRegistryOwnershipUnknown, "registry-ownership-unknown", true, false},
		{OrdinaryReasonLeaseOwnershipUnknown, "lease-ownership-unknown", true, false},
		{OrdinaryReasonOperationOwnershipUnknown, "operation-ownership-unknown", true, false},
		{OrdinaryReasonCleanupUncertain, "cleanup-uncertain", false, true},
	}
	for _, tc := range reasons {
		if !tc.reason.Valid() {
			t.Fatalf("reason %v should be valid", tc.reason)
		}
		if got := tc.reason.String(); got != tc.wantStr {
			t.Fatalf("reason %v string = %q, want %q", tc.reason, got, tc.wantStr)
		}
		if got := tc.reason.IsAttentionReason(); got != tc.isAttention {
			t.Fatalf("reason %v IsAttentionReason = %v, want %v", tc.reason, got, tc.isAttention)
		}
		if got := tc.reason.IsCleanupReason(); got != tc.isCleanup {
			t.Fatalf("reason %v IsCleanupReason = %v, want %v", tc.reason, got, tc.isCleanup)
		}
	}
	if OrdinaryClosedReason(0).Valid() || OrdinaryClosedReason(255).Valid() {
		t.Fatal("invalid closed reason was marked valid")
	}
	if got := OrdinaryClosedReason(255).String(); got != "" {
		t.Fatalf("invalid reason string = %q, want empty", got)
	}
	if got := OrdinaryOperationLifecycle(255).String(); got != "" {
		t.Fatalf("invalid state string = %q, want empty", got)
	}
	if OrdinaryLeaseState(0).Valid() || OrdinaryLeaseState(255).Valid() ||
		!OrdinaryLeaseReleased.Valid() || !OrdinaryLeaseHeld.Valid() {
		t.Fatal("lease state validity semantics drifted")
	}
	if OrdinaryLifecycleEvent(0).Valid() || OrdinaryLifecycleEvent(255).Valid() ||
		!OrdinaryLifecycleContinue.Valid() || !OrdinaryLifecycleCleanupFinished.Valid() {
		t.Fatal("lifecycle event validity semantics drifted")
	}
}

func TestReduceOrdinaryOperationLifecycleInvalidTransitions(t *testing.T) {
	// Invalid parameters
	if _, _, err := ReduceOrdinaryOperationLifecycle(0, OrdinaryLifecycleContinue, OrdinaryReasonNone); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("invalid current state succeeded")
	}
	if _, _, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationActive, 0, OrdinaryReasonNone); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("invalid event succeeded")
	}
	if _, _, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationActive, OrdinaryLifecycleContinue, 0); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("invalid reason succeeded")
	}

	// Continue with non-None reason
	if _, _, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationActive, OrdinaryLifecycleContinue, OrdinaryReasonCleanupUncertain); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("continue with reason succeeded")
	}
	// Continue from non-Active state
	if _, _, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationNeedsAttention, OrdinaryLifecycleContinue, OrdinaryReasonNone); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("continue from attention succeeded")
	}

	// RequireAttention with non-attention reason
	if _, _, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationActive, OrdinaryLifecycleRequireAttention, OrdinaryReasonNone); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("attention with none reason succeeded")
	}
	// RequireAttention from non-Active state
	if _, _, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationNeedsAttention, OrdinaryLifecycleRequireAttention, OrdinaryReasonLeaseOwnershipUnknown); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("attention from attention succeeded")
	}

	// Complete with non-None reason
	if _, _, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationActive, OrdinaryLifecycleComplete, OrdinaryReasonCleanupUncertain); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("complete with reason succeeded")
	}
	// Complete from non-Active state
	if _, _, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationNeedsAttention, OrdinaryLifecycleComplete, OrdinaryReasonNone); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("complete from attention succeeded")
	}

	// Discard with non-None reason
	if _, _, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationActive, OrdinaryLifecycleDiscard, OrdinaryReasonCleanupUncertain); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("discard with reason succeeded")
	}
	// Discard from invalid state (e.g. Completed)
	if _, _, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationCompleted, OrdinaryLifecycleDiscard, OrdinaryReasonNone); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("discard from completed succeeded")
	}
	// Discard from Active
	if state, reason, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationActive, OrdinaryLifecycleDiscard, OrdinaryReasonNone); err != nil || state != OrdinaryOperationDiscarded || reason != OrdinaryReasonNone {
		t.Fatalf("discard from active = (%v, %v, %v)", state, reason, err)
	}

	// CleanupFailed from non-terminal state
	if _, _, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationActive, OrdinaryLifecycleCleanupFailed, OrdinaryReasonCleanupUncertain); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("cleanup failed from active succeeded")
	}
	// CleanupFailed with non-cleanup reason
	if _, _, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationDiscarded, OrdinaryLifecycleCleanupFailed, OrdinaryReasonNone); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("cleanup failed with none reason succeeded")
	}
	// CleanupFailed from Completed
	if state, reason, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationCompleted, OrdinaryLifecycleCleanupFailed, OrdinaryReasonCleanupUncertain); err != nil || state != OrdinaryOperationCleanupPending || reason != OrdinaryReasonCleanupUncertain {
		t.Fatalf("cleanup failed from completed = (%v, %v, %v)", state, reason, err)
	}

	// CleanupFinished with non-None reason
	if _, _, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationCleanupPending, OrdinaryLifecycleCleanupFinished, OrdinaryReasonCleanupUncertain); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("cleanup finished with reason succeeded")
	}
	// CleanupFinished from non-CleanupPending state
	if _, _, err := ReduceOrdinaryOperationLifecycle(OrdinaryOperationActive, OrdinaryLifecycleCleanupFinished, OrdinaryReasonNone); !errors.Is(err, ErrInvalidOrdinaryLifecycle) {
		t.Fatal("cleanup finished from active succeeded")
	}
}
