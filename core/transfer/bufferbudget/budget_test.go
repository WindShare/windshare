package bufferbudget

import (
	"bytes"
	"testing"
)

func TestProcessPressureReclaimsCacheWithoutRevokingIndependentCopies(t *testing.T) {
	budget, err := New(8)
	if err != nil {
		t.Fatal(err)
	}
	first, second := NewAccount[string](budget, 8), NewAccount[int](budget, 8)
	firstHeld, ok := first.Reserve(2)
	if !ok {
		t.Fatal("initial reservation failed")
	}
	cached := storeTestBuffer(t, first, "first", []byte{1, 2}, firstHeld)
	copyData, copyHeld, live := cached.Copy()
	if !live || !bytes.Equal(copyData, []byte{1, 2}) || first.Used() != 4 {
		t.Fatal("copy was not independently charged")
	}
	changed := budget.Changes()
	secondHeld, ok := second.Reserve(6)
	if !ok {
		t.Fatal("other receiver could not reclaim idle cache")
	}
	if cached.Live() || first.Used() != 2 || budget.Used() != 8 {
		t.Fatal("cache reclamation changed active ownership")
	}
	if evicted := first.TakeEvicted(); len(evicted) != 1 || evicted[0] != cached || evicted[0].Key() != "first" {
		t.Fatal("process eviction did not identify the owning account's cache")
	}
	if len(second.TakeEvicted()) != 0 {
		t.Fatal("process eviction was reported to the requesting account")
	}
	if !bytes.Equal(copyData, []byte{1, 2}) {
		t.Fatal("eviction mutated the output copy")
	}
	select {
	case <-changed:
	default:
		t.Fatal("eviction did not wake admissions")
	}
	cached.Drop()
	if len(first.TakeEvicted()) != 0 {
		t.Fatal("repeated drop reported an already-consumed eviction")
	}
	if _, ok := second.Reserve(1); ok {
		t.Fatal("pending buffers were reclaimed")
	}
	copyHeld.Release()
	secondHeld.Release()
	if budget.Used() != 0 {
		t.Fatal("released buffers remained charged")
	}
}

func TestAccountCeilingAndPinnedCacheSource(t *testing.T) {
	budget, _ := New(8)
	account := NewAccount[string](budget, 4)
	held, ok := account.Reserve(3)
	if !ok {
		t.Fatal("initial reservation failed")
	}
	cached := storeTestBuffer(t, account, "source", []byte{1, 2, 3}, held)
	if data, _, live := cached.Copy(); data != nil || !live {
		t.Fatal("copy bypassed the session ceiling or evicted its own source")
	}
	if account.Used() != 3 || !cached.Live() {
		t.Fatal("failed copy changed ownership")
	}
	replacement, ok := account.Reserve(4)
	if !ok || cached.Live() {
		t.Fatal("new content could not reclaim cache")
	}
	if evicted := account.TakeEvicted(); len(evicted) != 1 || evicted[0] != cached {
		t.Fatal("session pressure did not report its cache eviction")
	}
	replacement.Release()
	if account.Used() != 0 || budget.Used() != 0 {
		t.Fatal("account retained released content")
	}
}

func storeTestBuffer[K comparable](t testing.TB, account *Account[K], key K, data []byte, reservation Reservation) *Cached[K] {
	t.Helper()
	cached, err := account.Store(key, data, reservation)
	if err != nil {
		t.Fatal(err)
	}
	return cached
}
func splitTestReservation(t testing.TB, reservation Reservation, bytes uint64) Reservation {
	t.Helper()
	split, ok := reservation.Split(bytes)
	if !ok {
		t.Fatal("reservation split failed")
	}
	return split
}
