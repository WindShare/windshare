package transfer

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	laneBenchmarkBlockBytes = uint32(64 << 10)
	laneBenchmarkBlocks     = 64
	windowTestBlocks        = 16
)

var laneCounts = [...]int{1, 2, 4, 8}

func benchmarkTransferIdentity[T ~[catalog.IdentityBytes]byte](seed byte) T {
	var identity T
	for index := range identity {
		identity[index] = seed + byte(index)
	}
	return identity
}

func benchmarkTransferData(tb testing.TB, blockBytes uint32, blockCount int) (content.FileRevisionDescriptor, []records.BlockRecord) {
	tb.Helper()
	share := benchmarkTransferIdentity[catalog.ShareInstance](13)
	file := benchmarkTransferIdentity[catalog.FileID](37)
	revision := benchmarkTransferIdentity[content.FileRevision](61)
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

type immediateBenchmarkLane struct {
	records []records.BlockRecord
	calls   atomic.Uint64
	seen    []atomic.Uint64
}

func newImmediateBenchmarkLane(recordsByIndex []records.BlockRecord) *immediateBenchmarkLane {
	return &immediateBenchmarkLane{records: recordsByIndex, seen: make([]atomic.Uint64, len(recordsByIndex))}
}

func (lane *immediateBenchmarkLane) FetchBlock(ctx context.Context, demand BlockDemand) (records.BlockRecord, error) {
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

func BenchmarkFileLocalMultiLane(b *testing.B) {
	descriptor, recordSet := benchmarkTransferData(b, laneBenchmarkBlockBytes, laneBenchmarkBlocks)
	exactBytes := descriptor.ExactSize()
	lease := benchmarkTransferIdentity[content.LeaseID](83)
	for _, laneCount := range laneCounts {
		b.Run(fmt.Sprintf("lanes=%02d/window=%02d/block_bytes=%07d", laneCount, laneCount, laneBenchmarkBlockBytes), func(b *testing.B) {
			lanes, err := NewLaneSet(LaneSetConfig{
				ProtocolSessionID: benchmarkTransferIdentity[protocolsession.ProtocolSessionID](101),
				RaceWidth:         1,
			})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(lanes.Close)
			probes := make([]*immediateBenchmarkLane, laneCount)
			for index := range probes {
				probes[index] = newImmediateBenchmarkLane(recordSet)
				if err := lanes.Add(LaneIdentity{ID: uint32(index + 1)}, LaneRouteRelay, probes[index]); err != nil {
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
			wantCalls := uint64(laneBenchmarkBlocks * b.N)
			if calls != wantCalls {
				b.Fatalf("lane fetches = %d, want %d", calls, wantCalls)
			}
			for blockIndex := range laneBenchmarkBlocks {
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

type exactWindowProbe struct {
	records []records.BlockRecord
	started chan struct{}
	release chan struct{}
	active  atomic.Int64
	peak    atomic.Int64
	calls   atomic.Uint64
	seen    []atomic.Uint32
}

func newExactWindowProbe(recordSet []records.BlockRecord) *exactWindowProbe {
	return &exactWindowProbe{
		records: recordSet, started: make(chan struct{}, len(recordSet)), release: make(chan struct{}),
		seen: make([]atomic.Uint32, len(recordSet)),
	}
}

func (probe *exactWindowProbe) FetchBlock(ctx context.Context, demand BlockDemand) (records.BlockRecord, error) {
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
