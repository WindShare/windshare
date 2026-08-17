package transfer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestBlockBrokerHonorsExactWindowAcrossLaneWidths(t *testing.T) {
	descriptor, recordSet := benchmarkTransferData(t, catalog.MinChunkSize, windowTestBlocks)
	exactBytes := descriptor.ExactSize()
	lease := benchmarkTransferIdentity[content.LeaseID](83)
	for _, laneCount := range laneCounts {
		t.Run(fmt.Sprintf("lanes=%02d/window=%02d", laneCount, laneCount), func(t *testing.T) {
			lanes, err := NewLaneSet(LaneSetConfig{
				ProtocolSessionID: benchmarkTransferIdentity[protocolsession.ProtocolSessionID](101), RaceWidth: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer lanes.Close()
			probe := newExactWindowProbe(recordSet)
			for index := range laneCount {
				if err := lanes.Add(LaneIdentity{ID: uint32(index + 1)}, LaneRouteRelay, probe); err != nil {
					t.Fatal(err)
				}
			}
			process, _ := NewPlaintextBudget(exactBytes * 2)
			broker, err := NewBlockBroker(BlockBrokerConfig{
				ShareInstance: descriptor.ShareInstance(), Lanes: lanes, MaxBytes: exactBytes,
				ProcessBudget: process, MaxConcurrentBlocks: laneCount,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer broker.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var written atomic.Uint64
			result := make(chan error, 1)
			go func() {
				result <- broker.ReadRange(
					ctx, lease, descriptor, content.Range{Offset: 0, End: exactBytes},
					RangeSinkFunc(func(_ context.Context, _ uint64, data []byte) error {
						written.Add(uint64(len(data)))
						return nil
					}),
				)
			}()
			for range laneCount {
				select {
				case <-probe.started:
				case <-ctx.Done():
					t.Fatal(context.Cause(ctx))
				}
			}
			feedDone := make(chan struct{})
			go func() {
				defer close(feedDone)
				for range windowTestBlocks {
					select {
					case probe.release <- struct{}{}:
					case <-ctx.Done():
						return
					}
				}
			}()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(context.Cause(ctx))
			}
			<-feedDone
			if probe.peak.Load() != int64(laneCount) || probe.active.Load() != 0 {
				t.Fatalf("inflight window peak=%d active=%d want peak=%d", probe.peak.Load(), probe.active.Load(), laneCount)
			}
			if probe.calls.Load() != windowTestBlocks || written.Load() != exactBytes {
				t.Fatalf("range calls=%d bytes=%d", probe.calls.Load(), written.Load())
			}
			for index := range probe.seen {
				if probe.seen[index].Load() != 1 {
					t.Fatalf("block %d fetched %d times", index, probe.seen[index].Load())
				}
			}
			if broker.UsedBytes() != exactBytes || process.Used() != exactBytes {
				t.Fatalf("successful range budget broker=%d process=%d", broker.UsedBytes(), process.Used())
			}
			broker.InvalidateRevision(descriptor.FileID(), descriptor.FileRevision())
			if broker.UsedBytes() != 0 || process.Used() != 0 {
				t.Fatal(errors.New("revision invalidation retained plaintext"))
			}
		})
	}
}
