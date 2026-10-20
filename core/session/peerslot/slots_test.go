package peerslot

import (
	"errors"
	"testing"
)

func TestReplacementReusesOnlyJoinedSlotsAndPreservesWatermarks(t *testing.T) {
	var pool Pool
	var live [Count]*Lease
	for i := range live {
		var err error
		live[i], err = pool.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		if first, err := live[i].Next(); err != nil || first != (Attempt{Slot: uint8(i), Sequence: 1}) {
			t.Fatal(first, err)
		}
	}
	if _, err := pool.Acquire(); !errors.Is(err, ErrCapacity) {
		t.Fatal("live replacement capacity was stolen", err)
	}
	for sequence := uint64(2); sequence < 100; sequence++ {
		old := live[1]
		old.Close()
		next, err := pool.Acquire()
		if err != nil {
			t.Fatal(err)
		}
		old.Close()
		if _, err := old.Next(); !errors.Is(err, ErrClosed) {
			t.Fatal("retired producer borrowed its successor", err)
		}
		if ordinal, err := next.Next(); err != nil || ordinal != (Attempt{Slot: 1, Sequence: sequence}) {
			t.Fatal("replacement reset the sequence", ordinal, err)
		}
		live[1] = next
	}
	for _, lease := range live {
		lease.Close()
	}
}

func TestSequenceExhaustionAndClosedLease(t *testing.T) {
	var pool Pool
	lease, _ := pool.Acquire()
	pool.slots[0].sequence = ^uint64(0)
	if _, err := lease.Next(); !errors.Is(err, ErrSequence) {
		t.Fatal(err)
	}
	lease.Close()
	next, err := pool.Acquire()
	if err != nil || next.slot != 1 {
		t.Fatal("exhausted sequence was reused", next, err)
	}
	var missing *Lease
	if _, err := missing.Next(); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	missing.Close()
}
