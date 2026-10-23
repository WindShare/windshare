package transfer

import (
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/transfer/bufferbudget"
)

func newBrokerCacheFixture(t *testing.T, limit uint64) *BlockBroker {
	t.Helper()
	broker, lanes, _, _, _ := newBrokerFixture(t, 1, &countingLane{}, limit, limit)
	t.Cleanup(lanes.Close)
	t.Cleanup(func() {
		if err := broker.Close(); err != nil {
			t.Error(err)
		}
	})
	return broker
}

func storeBrokerCache(t *testing.T, broker *BlockBroker, key blockKey, data []byte) *cachedBlock {
	t.Helper()
	broker.mu.Lock()
	defer broker.mu.Unlock()
	reservation, err := broker.reserveLocked(uint64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var route downloadmetrics.Route
	broker.storeReservedLocked(key, data, route, reservation)
	return broker.entries[key]
}

func TestBlockBrokerPrunesProcessEvictionsWithoutDroppingLiveCache(t *testing.T) {
	broker := newBrokerCacheFixture(t, 6)
	oldKey, liveKey, newKey := blockKey{index: 1}, blockKey{index: 2}, blockKey{index: 3}
	old := storeBrokerCache(t, broker, oldKey, []byte{1, 2})
	live := storeBrokerCache(t, broker, liveKey, []byte{3, 4})
	requester := bufferbudget.NewAccount[string](broker.process, 6)
	held, ok := requester.Reserve(4)
	if !ok {
		t.Fatal("process could not reclaim the oldest entry")
	}
	if old.buffer.Live() || !live.buffer.Live() {
		t.Fatal("process pressure reclaimed the wrong entry")
	}
	held.Release()
	added := storeBrokerCache(t, broker, newKey, []byte{5, 6})
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if len(broker.entries) != 2 || broker.entries[liveKey] != live || broker.entries[newKey] != added {
		t.Fatal("cache insertion did not remove exactly the evicted index")
	}
	if len(broker.account.TakeEvicted()) != 0 || broker.account.Used() != 4 {
		t.Fatal("cache insertion retained eviction records or changed live ownership")
	}
}

func TestBlockBrokerEvictionCleanupPreservesReplacementInstances(t *testing.T) {
	broker := newBrokerCacheFixture(t, 4)
	missingKey, replacedKey := blockKey{index: 1}, blockKey{index: 2}
	storeBrokerCache(t, broker, missingKey, []byte{1})
	storeBrokerCache(t, broker, replacedKey, []byte{2})
	requester := bufferbudget.NewAccount[int](broker.process, 4)
	held, ok := requester.Reserve(4)
	if !ok {
		t.Fatal("process could not reclaim both entries")
	}
	held.Release()

	broker.mu.Lock()
	defer broker.mu.Unlock()
	consumer := &blockConsumer{ready: func() bool { return true }}
	if copied, waiting := broker.copyCachedLocked(missingKey, consumer); copied.data != nil || waiting {
		t.Fatal("an evicted entry remained readable")
	}
	// The index may already describe a replacement when a pending record is consumed.
	replacementHeld, ok := broker.account.Reserve(1)
	if !ok {
		t.Fatal("replacement reservation failed")
	}
	buffer, err := broker.account.Store(replacedKey, []byte{3}, replacementHeld)
	if err != nil {
		t.Fatal(err)
	}
	replacement := &cachedBlock{buffer: buffer}
	broker.entries[replacedKey] = replacement
	broker.pruneEvictedLocked()
	if len(broker.entries) != 1 || broker.entries[replacedKey] != replacement || !replacement.buffer.Live() {
		t.Fatal("an old eviction removed a replacement or restored a missing index")
	}
	if len(broker.account.TakeEvicted()) != 0 {
		t.Fatal("eviction records were consumed more than once")
	}
}

func TestBlockBrokerInvalidationSettlesPendingEvictions(t *testing.T) {
	broker := newBrokerCacheFixture(t, 6)
	first := blockKey{file: transferID[catalog.FileID](1), revision: transferID[content.FileRevision](2)}
	other := blockKey{file: transferID[catalog.FileID](3), revision: first.revision}
	last := first
	last.index = 1
	storeBrokerCache(t, broker, first, []byte{1})
	retained := storeBrokerCache(t, broker, other, []byte{2})
	storeBrokerCache(t, broker, last, []byte{3})
	requester := bufferbudget.NewAccount[int](broker.process, 6)
	held, ok := requester.Reserve(4)
	if !ok {
		t.Fatal("process could not reclaim the first entry")
	}
	held.Release()

	broker.InvalidateRevision(first.file, first.revision)
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if len(broker.entries) != 1 || broker.entries[other] != retained || broker.account.Used() != 1 {
		t.Fatal("revision invalidation changed another file's cache")
	}
	if len(broker.account.TakeEvicted()) != 0 {
		t.Fatal("revision invalidation retained pending eviction records")
	}
}

func TestBlockBrokerStopReleasesPendingEvictionRecords(t *testing.T) {
	broker := newBrokerCacheFixture(t, 4)
	storeBrokerCache(t, broker, blockKey{index: 1}, []byte{1})
	storeBrokerCache(t, broker, blockKey{index: 2}, []byte{2})
	requester := bufferbudget.NewAccount[int](broker.process, 4)
	held, ok := requester.Reserve(3)
	if !ok {
		t.Fatal("process could not reclaim one entry")
	}
	broker.Stop()
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if len(broker.entries) != 0 || len(broker.account.TakeEvicted()) != 0 || broker.account.Used() != 0 {
		t.Fatal("stopped broker retained cache indexes, eviction records, or bytes")
	}
	if requester.Used() != 3 || broker.process.Used() != 3 {
		t.Fatal("stopping a broker changed another account's reservation")
	}
	held.Release()
}
