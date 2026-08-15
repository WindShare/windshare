package ordinaryoutput

import (
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestOrdinaryOutputPolicyV1IsFrozen(t *testing.T) {
	if OrdinaryOutputPolicyVersion != 1 ||
		MaximumResultNameReservationAttemptsV1 != 32 ||
		MaximumLiveCleanupTicketsV1 != 256 {
		t.Fatalf(
			"policy version/limits = %d/%d/%d",
			OrdinaryOutputPolicyVersion,
			MaximumResultNameReservationAttemptsV1,
			MaximumLiveCleanupTicketsV1,
		)
	}
	budget := DefaultShapeProbeBudgetV1
	if !budget.Valid() || budget.DirectoryRequests() != 32 ||
		budget.AuthenticatedPages() != 128 || budget.Entries() != 32_768 ||
		budget.AuthenticatedMetadataBytes() != 8*1024*1024 ||
		budget.Depth() != uint32(catalog.MaxPathDepth) || catalog.MaxPathDepth != 256 {
		t.Fatalf("default shape budget = %+v", budget)
	}
}

func TestShapeProbeBudgetIsClosedAndImmutable(t *testing.T) {
	if _, ok := NewShapeProbeBudget(0, 1, 1, 1, 1); ok {
		t.Fatal("zero directory-request limit accepted")
	}
	if _, ok := NewShapeProbeBudget(1, 0, 1, 1, 1); ok {
		t.Fatal("zero page limit accepted")
	}
	if _, ok := NewShapeProbeBudget(1, 1, 0, 1, 1); ok {
		t.Fatal("zero entry limit accepted")
	}
	if _, ok := NewShapeProbeBudget(1, 1, 1, 0, 1); ok {
		t.Fatal("zero metadata limit accepted")
	}
	if _, ok := NewShapeProbeBudget(1, 1, 1, 1, 0); ok {
		t.Fatal("zero depth accepted")
	}
	if _, ok := NewShapeProbeBudget(1, 1, 1, 1, uint32(catalog.MaxPathDepth+1)); ok {
		t.Fatal("depth beyond canonical catalog limit accepted")
	}
}
