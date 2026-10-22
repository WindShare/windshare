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
	first, second := budget.Account(8), budget.Account(8)
	if !first.Reserve(2) {
		t.Fatal("initial reservation failed")
	}
	cached := first.Store([]byte{1, 2})
	copyData, live := cached.Copy()
	if !live || !bytes.Equal(copyData, []byte{1, 2}) || first.Used() != 4 {
		t.Fatal("copy was not independently charged")
	}
	changed := budget.Changes()
	if !second.Reserve(6) {
		t.Fatal("other receiver could not reclaim idle cache")
	}
	if cached.Live() || first.Used() != 2 || budget.Used() != 8 {
		t.Fatal("cache reclamation changed active ownership")
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
	if second.Reserve(1) {
		t.Fatal("pending buffers were reclaimed")
	}
	first.Release(2)
	second.Release(6)
	if budget.Used() != 0 {
		t.Fatal("released buffers remained charged")
	}
}

func TestAccountCeilingAndPinnedCacheSource(t *testing.T) {
	budget, _ := New(8)
	account := budget.Account(4)
	if !account.Reserve(3) {
		t.Fatal("initial reservation failed")
	}
	cached := account.Store([]byte{1, 2, 3})
	if data, live := cached.Copy(); data != nil || !live {
		t.Fatal("copy bypassed the session ceiling or evicted its own source")
	}
	if account.Used() != 3 || !cached.Live() {
		t.Fatal("failed copy changed ownership")
	}
	if !account.Reserve(4) || cached.Live() {
		t.Fatal("new content could not reclaim cache")
	}
	account.Release(4)
	if account.Used() != 0 || budget.Used() != 0 {
		t.Fatal("account retained released content")
	}
}
