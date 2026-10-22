package transfer

import (
	"context"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	receiptBenchmarkBlockBytes    = 1 << 20
	receiptBenchmarkFragmentBytes = 16 << 10
)

type fragmentedReceiptLane struct {
	record records.BlockRecord
}

func (lane fragmentedReceiptLane) FetchBlock(ctx context.Context, _ BlockDemand, received func(uint64)) (records.BlockRecord, error) {
	for remaining := uint64(lane.record.DataLength()); remaining != 0; {
		if err := ctx.Err(); err != nil {
			return records.BlockRecord{}, err
		}
		bytes := min(remaining, receiptBenchmarkFragmentBytes)
		received(bytes)
		remaining -= bytes
	}
	return lane.record, nil
}

// This isolates receive accounting and admission under concurrent callers.
// Payloads are prebuilt: the result is not network, decryption, or disk throughput.
func BenchmarkLaneFragmentReceipt(b *testing.B) {
	descriptor, blocks := benchmarkTransferData(b, receiptBenchmarkBlockBytes, 1)
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: benchmarkTransferIdentity[protocolsession.ProtocolSessionID](113),
		RaceWidth:         1,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(lanes.Close)
	lanes.BindDownloadMetrics(downloadmetrics.New("receipt-benchmark", nil, false))
	if err := lanes.Add(LaneIdentity{ID: 1}, LaneRouteDirect, fragmentedReceiptLane{record: blocks[0]}); err != nil {
		b.Fatal(err)
	}
	demand := BlockDemand{
		Descriptor: descriptor,
		LeaseID:    benchmarkTransferIdentity[content.LeaseID](114),
	}
	request := blockRequest{demand: demand, validate: validateTransferRecord(demand)}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := lanes.fetch(context.Background(), request); err != nil {
				b.Error(err)
				return
			}
		}
	})
	lanes.attempts.Wait()
}
