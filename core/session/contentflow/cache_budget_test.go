package contentflow

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/capacityledger"
)

func TestCacheReservationsAggregateAndCloseWithoutRevokingEntries(t *testing.T) {
	budget, err := NewProcessCacheBudget(8)
	if err != nil {
		t.Fatal(err)
	}
	first, second := cacheReservation{budget: budget}, cacheReservation{budget: budget}
	if !first.grow(2) || !first.grow(2) || !second.grow(4) || first.grow(1) {
		t.Fatal("aggregate cache admission bypassed process capacity")
	}
	var retained *capacityledger.UnreleasedError
	if err := budget.Close(); !errors.As(err, &retained) || retained.Used != 8 || retained.Holds != 2 {
		t.Fatalf("close did not preserve per-cache aggregate ownership: %v", err)
	}
	first.shrink(2)
	if budget.Used() != 6 || first.grow(1) {
		t.Fatal("closed budget admitted growth or lost retained entries")
	}
	first.shrink(2)
	second.shrink(4)
	if first.grow(1) || budget.Used() != 0 {
		t.Fatal("settled cache bypassed process close")
	}
	if err := budget.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCacheReservationReopensAfterFullEviction(t *testing.T) {
	budget, _ := NewProcessCacheBudget(4)
	held := cacheReservation{budget: budget}
	if !held.grow(4) {
		t.Fatal("initial reservation failed")
	}
	held.shrink(4)
	if !held.grow(3) || budget.Used() != 3 {
		t.Fatal("full eviction prevented a later cache fill")
	}
	held.shrink(3)
	if err := budget.Close(); err != nil {
		t.Fatal(err)
	}
}
