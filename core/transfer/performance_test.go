package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	r8LaneBenchmarkBlockBytes = uint32(64 << 10)
	r8LaneBenchmarkBlocks     = 64
	r8WindowTestBlocks        = 16
)

var r8LaneCounts = [...]int{1, 2, 4, 8}

func r8TransferIdentity[T ~[catalog.IdentityBytes]byte](seed byte) T {
	var identity T
	for index := range identity {
		identity[index] = seed + byte(index)
	}
	return identity
}

func r8TransferData(tb testing.TB, blockBytes uint32, blockCount int) (content.FileRevisionDescriptor, []records.BlockRecord) {
	tb.Helper()
	share := r8TransferIdentity[catalog.ShareInstance](13)
	file := r8TransferIdentity[catalog.FileID](37)
	revision := r8TransferIdentity[content.FileRevision](61)
	geometry, err := content.NewFileGeometry(uint64(blockBytes)*uint64(blockCount), blockBytes)
	if err != nil {
		tb.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(share, file, revision, geometry, catalog.ModifiedTime{})
	if err != nil {
		tb.Fatal(err)
	}
	result := make([]records.BlockRecord, blockCount)
	for index := range result {
		result[index], err = records.NewBlockRecord(
			descriptor, uint64(index), bytes.Repeat([]byte{byte(index)}, int(blockBytes)),
		)
		if err != nil {
			tb.Fatal(err)
		}
	}
	return descriptor, result
}

type r8ImmediateLane struct {
	records []records.BlockRecord
	calls   atomic.Uint64
	seen    []atomic.Uint64
}

func newR8ImmediateLane(recordsByIndex []records.BlockRecord) *r8ImmediateLane {
	return &r8ImmediateLane{records: recordsByIndex, seen: make([]atomic.Uint64, len(recordsByIndex))}
}

func (lane *r8ImmediateLane) FetchBlock(ctx context.Context, demand BlockDemand) (records.BlockRecord, error) {
	if err := ctx.Err(); err != nil {
		return records.BlockRecord{}, err
	}
	if demand.Index >= uint64(len(lane.records)) {
		return records.BlockRecord{}, ErrInvalidDemand
	}
	lane.calls.Add(1)
	lane.seen[demand.Index].Add(1)
	return lane.records[demand.Index], nil
}

func BenchmarkR8FileLocalMultiLane(b *testing.B) {
	descriptor, recordSet := r8TransferData(b, r8LaneBenchmarkBlockBytes, r8LaneBenchmarkBlocks)
	exactBytes := descriptor.ExactSize()
	lease := r8TransferIdentity[content.LeaseID](83)
	for _, laneCount := range r8LaneCounts {
		b.Run(fmt.Sprintf("lanes=%02d/window=%02d/block_bytes=%07d", laneCount, laneCount, r8LaneBenchmarkBlockBytes), func(b *testing.B) {
			lanes, err := NewLaneSet(LaneSetConfig{
				ProtocolSessionID: r8TransferIdentity[protocolsession.ProtocolSessionID](101),
				RaceWidth:         1,
			})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(lanes.Close)
			probes := make([]*r8ImmediateLane, laneCount)
			for index := range probes {
				probes[index] = newR8ImmediateLane(recordSet)
				if err := lanes.Add(LaneIdentity{ID: uint32(index + 1)}, probes[index]); err != nil {
					b.Fatal(err)
				}
			}
			process, err := NewPlaintextBudget(exactBytes * 2)
			if err != nil {
				b.Fatal(err)
			}
			broker, err := NewBlockBroker(BlockBrokerConfig{
				ShareInstance: descriptor.ShareInstance(), Lanes: lanes, MaxBytes: exactBytes,
				ProcessBudget: process, MaxConcurrentBlocks: laneCount,
			})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(broker.Close)

			b.ReportAllocs()
			b.SetBytes(int64(exactBytes))
			b.ResetTimer()
			for range b.N {
				var written uint64
				err := broker.ReadRange(
					context.Background(), lease, descriptor, content.Range{Offset: 0, End: exactBytes},
					RangeSinkFunc(func(_ context.Context, _ uint64, data []byte) error {
						written += uint64(len(data))
						return nil
					}),
				)
				if err != nil {
					b.Fatal(err)
				}
				if written != exactBytes {
					b.Fatalf("file-local range wrote %d bytes, want %d", written, exactBytes)
				}
				broker.InvalidateRevision(descriptor.FileID(), descriptor.FileRevision())
				if broker.UsedBytes() != 0 || process.Used() != 0 {
					b.Fatalf("invalidation retained plaintext: broker=%d process=%d", broker.UsedBytes(), process.Used())
				}
			}
			b.StopTimer()
			var calls uint64
			for _, probe := range probes {
				calls += probe.calls.Load()
			}
			wantCalls := uint64(r8LaneBenchmarkBlocks * b.N)
			if calls != wantCalls {
				b.Fatalf("lane fetches = %d, want %d", calls, wantCalls)
			}
			for blockIndex := range r8LaneBenchmarkBlocks {
				var fetches uint64
				for _, probe := range probes {
					fetches += probe.seen[blockIndex].Load()
				}
				if fetches != uint64(b.N) {
					b.Fatalf("block %d fetched %d times, want %d", blockIndex, fetches, b.N)
				}
			}
			b.ReportMetric(float64(calls)/float64(b.N), "lane-fetches/op")
			b.ReportMetric(0, "duplicate-fetches/op")
			b.ReportMetric(float64(laneCount), "window-blocks")
		})
	}
}

type r8WindowProbe struct {
	records []records.BlockRecord
	started chan struct{}
	release chan struct{}
	active  atomic.Int64
	peak    atomic.Int64
	calls   atomic.Uint64
	seen    []atomic.Uint32
}

func newR8WindowProbe(recordSet []records.BlockRecord) *r8WindowProbe {
	return &r8WindowProbe{
		records: recordSet, started: make(chan struct{}, len(recordSet)), release: make(chan struct{}),
		seen: make([]atomic.Uint32, len(recordSet)),
	}
}

func (probe *r8WindowProbe) FetchBlock(ctx context.Context, demand BlockDemand) (records.BlockRecord, error) {
	if demand.Index >= uint64(len(probe.records)) {
		return records.BlockRecord{}, ErrInvalidDemand
	}
	active := probe.active.Add(1)
	for {
		peak := probe.peak.Load()
		if active <= peak || probe.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	probe.calls.Add(1)
	probe.seen[demand.Index].Add(1)
	select {
	case probe.started <- struct{}{}:
	case <-ctx.Done():
		probe.active.Add(-1)
		return records.BlockRecord{}, ctx.Err()
	}
	select {
	case <-probe.release:
		probe.active.Add(-1)
		return probe.records[demand.Index], nil
	case <-ctx.Done():
		probe.active.Add(-1)
		return records.BlockRecord{}, ctx.Err()
	}
}

func TestR8BlockBrokerHonorsExactWindowAcrossLaneWidths(t *testing.T) {
	descriptor, recordSet := r8TransferData(t, catalog.MinChunkSize, r8WindowTestBlocks)
	exactBytes := descriptor.ExactSize()
	lease := r8TransferIdentity[content.LeaseID](83)
	for _, laneCount := range r8LaneCounts {
		t.Run(fmt.Sprintf("lanes=%02d/window=%02d", laneCount, laneCount), func(t *testing.T) {
			lanes, err := NewLaneSet(LaneSetConfig{
				ProtocolSessionID: r8TransferIdentity[protocolsession.ProtocolSessionID](101), RaceWidth: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer lanes.Close()
			probe := newR8WindowProbe(recordSet)
			for index := range laneCount {
				if err := lanes.Add(LaneIdentity{ID: uint32(index + 1)}, probe); err != nil {
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
				for range r8WindowTestBlocks {
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
			if probe.calls.Load() != r8WindowTestBlocks || written.Load() != exactBytes {
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
