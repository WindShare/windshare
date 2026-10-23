package capacityledger

import (
	"errors"
	"math"
	"testing"
)

func TestOwnershipSurvivesCopiesSplitsAndTransfers(t *testing.T) {
	process := New(10)
	session := process.Child(8)
	held, ok := session.Reserve(6)
	if !ok {
		t.Fatal("reserve failed")
	}
	alias := held
	part, ok := held.Split(2)
	if !ok || held.Units() != 4 || process.Snapshot().Used != 6 || process.Snapshot().Holds != 2 {
		t.Fatal("split changed total capacity")
	}
	next, ok := held.Transfer()
	if !ok || held.Release() || alias.Release() || held.Units() != 0 {
		t.Fatal("old owner retained authority after transfer")
	}
	if !next.Grow(2) || next.Grow(1) || next.Shrink(7) {
		t.Fatal("growth or shrink bypassed the session ceiling")
	}
	if !next.Shrink(2) || process.Snapshot().Used != 6 {
		t.Fatal("partial release changed another hold")
	}
	partAlias := part
	movedPart, ok := part.Split(2)
	if !ok || partAlias.Release() || !movedPart.Release() || movedPart.Release() {
		t.Fatal("whole split did not consume the old capability")
	}
	if !next.Release() || alias.Release() || process.Snapshot().Used != 0 || process.Snapshot().Holds != 0 {
		t.Fatal("settlement retained or double-released capacity")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHierarchyAdmissionAndLateCleanupAfterClose(t *testing.T) {
	process := New(5)
	first, second := process.Child(4), process.Child(9)
	held, ok := first.Reserve(4)
	if !ok || first.Limit() != 4 || second.Limit() != 5 {
		t.Fatal("invalid hierarchy")
	}
	if _, ok := second.Reserve(2); ok {
		t.Fatal("process ceiling was bypassed")
	}
	if _, ok := first.Reserve(0); ok {
		t.Fatal("zero capacity manufactured a live hold")
	}
	if first.Owns(Hold{}) || second.Owns(held) || !first.Owns(held) {
		t.Fatal("owner identity was lost")
	}
	var leak *UnreleasedError
	if err := process.Close(); !errors.As(err, &leak) || leak.Used != 4 || leak.Holds != 1 || leak.Error() == "" {
		t.Fatalf("close lost live ownership: %v", err)
	}
	if process.Available() != 0 || !process.Snapshot().Closed || held.Grow(1) {
		t.Fatal("closed ancestor still admitted capacity")
	}
	if _, ok := second.Reserve(1); ok {
		t.Fatal("child bypassed closed ancestor")
	}
	if err := first.Close(); !errors.As(err, &leak) {
		t.Fatal("child lost its outstanding hold")
	}
	held.Release()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := process.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReleasedAndEmptyCapabilitiesCannotChangeAccounting(t *testing.T) {
	ledger := New(math.MaxUint64)
	var empty Hold
	if empty.Release() || empty.Grow(1) || empty.Shrink(1) || empty.Units() != 0 {
		t.Fatal("empty hold became live")
	}
	if _, ok := empty.Transfer(); ok {
		t.Fatal("empty transfer succeeded")
	}
	if _, ok := empty.Split(1); ok {
		t.Fatal("empty split succeeded")
	}
	held, ok := ledger.Reserve(math.MaxUint64)
	if !ok || held.Grow(1) {
		t.Fatal("maximum capacity overflowed")
	}
	if _, ok := held.Split(0); ok {
		t.Fatal("zero split succeeded")
	}
	if !held.Shrink(math.MaxUint64) || held.Release() {
		t.Fatal("full shrink did not settle ownership")
	}
	small, _ := ledger.Reserve(1)
	if _, ok := small.Split(2); ok {
		t.Fatal("oversized split succeeded")
	}
	small.state.generation = math.MaxUint64
	small.generation = math.MaxUint64
	if _, ok := small.Transfer(); ok {
		t.Fatal("generation wrapped into an old capability")
	}
	if !small.Release() {
		t.Fatal("exhausted transfer prevented cleanup")
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
}
