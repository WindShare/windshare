package transfer

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/transfer/bufferbudget"
)

// Each operation fills an initially empty cache below its byte ceiling. Small
// files expose indexing work that a benchmark with a few large blocks hides.
func BenchmarkBlockBrokerCacheInsertion(b *testing.B) {
	const entryBytes = 1 << 10
	for _, count := range []int{1_000, 4_000, 16_000} {
		b.Run(fmt.Sprintf("entries=%d", count), func(b *testing.B) {
			b.StopTimer()
			keys := make([]blockKey, count)
			data := make([][]byte, count)
			for i := range keys {
				binary.LittleEndian.PutUint64(keys[i].file[:], uint64(i+1))
				data[i] = make([]byte, entryBytes)
				data[i][0] = byte(i)
			}
			var route downloadmetrics.Route
			b.ReportAllocs()
			for range b.N {
				process, err := NewPlaintextBudget(DefaultProcessPlaintextBytes)
				if err != nil {
					b.Fatal(err)
				}
				broker := &BlockBroker{
					process: process,
					account: bufferbudget.NewAccount[blockKey](process, DefaultSessionPlaintextBytes),
					entries: make(map[blockKey]*cachedBlock),
				}
				reservation, ok := broker.account.Reserve(uint64(count * entryBytes))
				if !ok {
					b.Fatal("reservation failed")
				}
				// Production claims each block before fetching it. Keep preparation
				// outside insertion timing, just like the aggregate reservation.
				parts := make([]bufferbudget.Reservation, count)
				for i := range parts {
					var ok bool
					parts[i], ok = reservation.Split(entryBytes)
					if !ok {
						b.Fatal("split failed")
					}
				}
				b.StartTimer()
				for i, key := range keys {
					broker.mu.Lock()
					broker.storeReservedLocked(key, data[i], route, parts[i])
					broker.mu.Unlock()
				}
				b.StopTimer()
				if len(broker.entries) != count || process.Used() != uint64(count*entryBytes) {
					b.Fatal("cache did not retain the full insertion batch")
				}
			}
		})
	}
}
