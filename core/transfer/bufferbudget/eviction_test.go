package bufferbudget

import (
	"bytes"
	"slices"
	"testing"
)

func TestEvictionRecordsIdentifyInstancesAndExcludeOwnerDrops(t *testing.T) {
	budget, _ := New(2)
	account := NewAccount[string](budget, 2)
	const key = "reused"
	var stored []*Cached[string]
	for range 3 {
		held, ok := account.Reserve(2)
		if !ok {
			t.Fatal("reservation failed")
		}
		stored = append(stored, storeTestBuffer(t, account, key, []byte{1, 2}, held))
	}
	if evicted := account.TakeEvicted(); !slices.Equal(evicted, stored[:2]) {
		t.Fatal("eviction records collapsed distinct instances of the same key")
	}
	for _, cached := range stored {
		if cached.Key() != key {
			t.Fatal("eviction lost the cache key")
		}
	}
	if !stored[2].Live() || account.Used() != 2 {
		t.Fatal("taking evictions changed the replacement's ownership")
	}
	for _, cached := range stored {
		cached.Drop()
		cached.Drop()
	}
	if len(account.TakeEvicted()) != 0 || budget.Used() != 0 {
		t.Fatal("owner drops produced eviction records or retained bytes")
	}
}

func TestCopyReportsEvictionsWithoutEvictingItsSource(t *testing.T) {
	budget, _ := New(4)
	account := NewAccount[string](budget, 4)
	held, ok := account.Reserve(4)
	if !ok {
		t.Fatal("reservation failed")
	}
	source := storeTestBuffer(t, account, "source", []byte{1, 2}, splitTestReservation(t, held, 2))
	other := storeTestBuffer(t, account, "other", []byte{3, 4}, held)
	if len(account.TakeEvicted()) != 0 {
		t.Fatal("storing live cache reported an eviction")
	}
	copied, copyHeld, live := source.Copy()
	if !live || !bytes.Equal(copied, []byte{1, 2}) || !source.Live() {
		t.Fatal("copy lost its pinned source")
	}
	if evicted := account.TakeEvicted(); !slices.Equal(evicted, []*Cached[string]{other}) {
		t.Fatal("copy did not report exactly the reclaimed peer entry")
	}
	source.Drop()
	if !bytes.Equal(copied, []byte{1, 2}) || account.Used() != 2 {
		t.Fatal("dropping the cache changed its independent delivery")
	}
	copyHeld.Release()
	if len(account.TakeEvicted()) != 0 || budget.Used() != 0 {
		t.Fatal("copy settlement left eviction records or reservations")
	}
}

func TestFailedAdmissionStillReportsReclaimedCache(t *testing.T) {
	budget, _ := New(4)
	owner, requester := NewAccount[string](budget, 4), NewAccount[int](budget, 4)
	held, ok := owner.Reserve(3)
	if !ok {
		t.Fatal("reservation failed")
	}
	cached := storeTestBuffer(t, owner, "cached", []byte{1, 2}, splitTestReservation(t, held, 2))
	if _, ok := requester.Reserve(4); ok {
		t.Fatal("admission reclaimed the owner's retained byte")
	}
	if evicted := owner.TakeEvicted(); !slices.Equal(evicted, []*Cached[string]{cached}) {
		t.Fatal("failed admission lost the cache eviction")
	}
	if owner.Used() != 1 || budget.Used() != 1 {
		t.Fatal("failed admission changed retained ownership")
	}
	held.Release()
}

func TestConcurrentEvictionDrainsDoNotLoseOrRepeatRecords(t *testing.T) {
	const entries = 128
	budget, _ := New(entries)
	owner, requester := NewAccount[int](budget, entries), NewAccount[string](budget, entries)
	held, ok := owner.Reserve(entries)
	if !ok {
		t.Fatal("reservation failed")
	}
	want := make([]*Cached[int], entries)
	for key := range entries {
		want[key] = storeTestBuffer(t, owner, key, []byte{byte(key)}, splitTestReservation(t, held, 1))
	}
	retained := make([]Reservation, entries)
	start := make(chan struct{})
	admitted := make(chan bool, 1)
	drained := make(chan []*Cached[int], 1)
	go func() {
		<-start
		for index := range entries {
			reservation, ok := requester.Reserve(1)
			if !ok {
				admitted <- false
				return
			}
			retained[index] = reservation
		}
		admitted <- true
	}()
	go func() {
		<-start
		var records []*Cached[int]
		for range entries {
			records = append(records, owner.TakeEvicted()...)
		}
		drained <- records
	}()
	close(start)
	if !<-admitted {
		t.Fatal("requester could not reclaim the cache")
	}
	got := append(<-drained, owner.TakeEvicted()...)
	if !slices.Equal(got, want) {
		t.Fatalf("concurrent drains changed eviction order or identity: got %d, want %d", len(got), len(want))
	}
	for _, reservation := range retained {
		reservation.Release()
	}
	if len(owner.TakeEvicted()) != 0 || owner.Used() != 0 || budget.Used() != 0 {
		t.Fatal("concurrent eviction retained records or bytes")
	}
}
