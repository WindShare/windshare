package bufferbudget

import (
	"errors"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/capacityledger"
)

func TestCacheHandoffRejectsWrongOwnerAndSizeWithoutConsuming(t *testing.T) {
	budget, _ := New(8)
	first, second := NewAccount[string](budget, 8), NewAccount[string](budget, 8)
	held, _ := first.Reserve(4)
	for _, target := range []*Account[string]{first, second} {
		if _, err := target.Store("wrong", []byte{1, 2}, held); !errors.Is(err, ErrReservation) {
			t.Fatal("mismatched handoff accepted")
		}
	}
	if _, err := second.Store("foreign", []byte{1, 2, 3, 4}, held); !errors.Is(err, ErrReservation) {
		t.Fatal("other account consumed the reservation")
	}
	otherBudget, _ := New(8)
	other := NewAccount[string](otherBudget, 8)
	if _, err := other.Store("foreign", []byte{1, 2, 3, 4}, held); !errors.Is(err, ErrReservation) {
		t.Fatal("other process consumed the reservation")
	}
	if held.Bytes() != 4 {
		t.Fatal("failed handoff consumed capacity")
	}
	alias := held
	cached := storeTestBuffer(t, first, "live", []byte{1, 2, 3, 4}, held)
	if held.Bytes() != 0 || alias.Release() || budget.Used() != 4 {
		t.Fatal("old owner released the cache's capacity")
	}
	if _, err := first.Store("duplicate", []byte{1, 2, 3, 4}, alias); !errors.Is(err, ErrReservation) {
		t.Fatal("one reservation was consumed twice")
	}
	cached.Drop()
	cached.Drop()
	if err := budget.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAccountCloseReportsIndependentDeliveryAndAllowsItsCleanup(t *testing.T) {
	budget, _ := New(8)
	account := NewAccount[string](budget, 8)
	held, _ := account.Reserve(2)
	cached := storeTestBuffer(t, account, "source", []byte{1, 2}, held)
	data, delivery, live := cached.Copy()
	if !live || len(data) != 2 {
		t.Fatal("copy failed")
	}
	var leak *capacityledger.UnreleasedError
	if err := account.Close(); !errors.As(err, &leak) || leak.Used != 2 || leak.Holds != 1 {
		t.Fatalf("close did not report the independent delivery: %v", err)
	}
	if cached.Live() || budget.Used() != 2 {
		t.Fatal("close revoked or lost the independent delivery")
	}
	if _, ok := account.Reserve(1); ok {
		t.Fatal("closed account admitted capacity")
	}
	if err := budget.Close(); !errors.As(err, &leak) {
		t.Fatal("process close lost retained ownership")
	}
	changed := budget.Changes()
	if !delivery.Release() || delivery.Release() {
		t.Fatal("delivery was not released exactly once")
	}
	select {
	case <-changed:
	default:
		t.Fatal("cleanup did not wake waiters")
	}
	if err := account.Close(); err != nil {
		t.Fatal(err)
	}
	if err := budget.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentReleaseAndTransferHaveOneOwner(t *testing.T) {
	budget, _ := New(1)
	account := NewAccount[int](budget, 1)
	for range 64 {
		held, ok := account.Reserve(1)
		if !ok {
			t.Fatal("capacity leaked between rounds")
		}
		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		go func() { defer workers.Done(); <-start; held.Release() }()
		go func() {
			defer workers.Done()
			<-start
			next, transferred := held.Transfer()
			if transferred {
				next.Release()
			}
		}()
		close(start)
		workers.Wait()
		if budget.Used() != 0 {
			t.Fatal("release/transfer race corrupted accounting")
		}
	}
	if err := account.Close(); err != nil {
		t.Fatal(err)
	}
	if err := budget.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestClosedProcessCannotEvictOrAdmitThroughExistingAccounts(t *testing.T) {
	budget, _ := New(2)
	account := NewAccount[string](budget, 2)
	held, _ := account.Reserve(2)
	if err := budget.Close(); err == nil {
		t.Fatal("close lost the reservation")
	}
	if _, err := account.Store("late", []byte{1, 2}, held); !errors.Is(err, ErrReservation) {
		t.Fatal("cache accepted a late transfer")
	}
	if _, ok := account.Reserve(1); ok {
		t.Fatal("closed process admitted capacity")
	}
	held.Release()
	var empty Reservation
	if empty.Bytes() != 0 || empty.Release() {
		t.Fatal("empty reservation became live")
	}
	if _, ok := empty.Transfer(); ok {
		t.Fatal("empty transfer succeeded")
	}
	if _, ok := empty.Split(1); ok {
		t.Fatal("empty split succeeded")
	}
	if err := budget.Close(); err != nil {
		t.Fatal(err)
	}
}
