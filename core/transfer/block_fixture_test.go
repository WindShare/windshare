package transfer

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

func transferID[T ~[16]byte](value byte) T {
	var id T
	id[0] = value
	return id
}

func transferDescriptor(t *testing.T, blocks uint64) content.FileRevisionDescriptor {
	t.Helper()
	geometry, err := content.NewFileGeometry(blocks*uint64(catalog.MinChunkSize), catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		transferID[catalog.ShareInstance](1), transferID[catalog.FileID](2), transferID[content.FileRevision](3),
		geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func transferRecord(t *testing.T, descriptor content.FileRevisionDescriptor, index uint64) records.BlockRecord {
	t.Helper()
	length, err := descriptor.Geometry().BlockPlainLength(index)
	if err != nil {
		t.Fatal(err)
	}
	record, err := records.NewBlockRecord(descriptor, index, bytes.Repeat([]byte{byte(index)}, int(length)))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

type laneFunction func(context.Context, transferlanes.Demand) (records.BlockRecord, error)

func (function laneFunction) FetchBlock(ctx context.Context, demand transferlanes.Demand, _ func(uint64)) (records.BlockRecord, error) {
	return function(ctx, demand)
}

func validDemand(t *testing.T, descriptor content.FileRevisionDescriptor, index uint64) transferlanes.Demand {
	t.Helper()
	return transferlanes.Demand{LeaseID: transferID[content.LeaseID](4), Descriptor: descriptor, Index: index}
}

func validateTransferRecord(demand transferlanes.Demand) func(records.BlockRecord) error {
	return func(record records.BlockRecord) error {
		if record.Descriptor() != demand.Descriptor || record.LocalBlockIndex() != demand.Index {
			return ErrBlockIdentity
		}
		return nil
	}
}

// Capacity changes are the externally observable settlement boundary. Tests must
// not join a lane's private wait group to know when its plaintext was released.
func waitForRetainedBytes(tb testing.TB, broker *BlockBroker, process *PlaintextBudget, brokerBytes, processBytes uint64) {
	tb.Helper()
	changed := process.Changes()
	if broker.UsedBytes() == brokerBytes && process.Used() == processBytes {
		return
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-changed:
		case <-timer.C:
			tb.Fatalf("retained bytes: broker=%d process=%d, want %d/%d", broker.UsedBytes(), process.Used(), brokerBytes, processBytes)
		}
		changed = process.Changes()
		if broker.UsedBytes() == brokerBytes && process.Used() == processBytes {
			return
		}
	}
}
