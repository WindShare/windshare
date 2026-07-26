package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
)

func TestBlockBrokerReadRangeRejectsForeignSelectionBeforeFetching(t *testing.T) {
	fetched := false
	broker, lanes, descriptor, lease, _ := newBrokerFixture(
		t,
		1,
		laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			fetched = true
			return records.BlockRecord{}, nil
		}),
		uint64(catalog.MinChunkSize),
		uint64(catalog.MinChunkSize)*2,
	)
	defer broker.Close()
	defer lanes.Close()

	foreign, err := content.NewFileRevisionDescriptor(
		transferID[catalog.ShareInstance](77),
		descriptor.FileID(),
		descriptor.FileRevision(),
		descriptor.Geometry(),
		descriptor.ModifiedTime(),
	)
	if err != nil {
		t.Fatal(err)
	}
	err = broker.ReadRange(
		context.Background(),
		lease,
		foreign,
		content.Range{Offset: 0, End: 1},
		RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil }),
	)
	if !errors.Is(err, ErrInvalidDemand) || fetched {
		t.Fatalf("foreign selection read = %v, fetched = %v", err, fetched)
	}
}

func TestBlockBrokerReadRangePreservesAsynchronousBlockFailure(t *testing.T) {
	want := errors.New("source block unavailable")
	broker, lanes, descriptor, lease, _ := newBrokerFixture(
		t,
		1,
		laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			return records.BlockRecord{}, want
		}),
		uint64(catalog.MinChunkSize),
		uint64(catalog.MinChunkSize)*2,
	)
	defer broker.Close()
	defer lanes.Close()

	sinkCalled := false
	err := broker.ReadRange(
		context.Background(),
		lease,
		descriptor,
		content.Range{Offset: 0, End: 1},
		RangeSinkFunc(func(context.Context, uint64, []byte) error {
			sinkCalled = true
			return nil
		}),
	)
	if !errors.Is(err, want) || sinkCalled {
		t.Fatalf("failed block read = %v, sink called = %v", err, sinkCalled)
	}
}

func TestBlockBrokerReadRangeRejectsAlreadyCancelledWork(t *testing.T) {
	broker, lanes, descriptor, lease, _ := newBrokerFixture(
		t,
		1,
		laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
			return records.BlockRecord{}, errors.New("cancelled read unexpectedly fetched content")
		}),
		uint64(catalog.MinChunkSize),
		uint64(catalog.MinChunkSize)*2,
	)
	defer broker.Close()
	defer lanes.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sinkCalled := false
	err := broker.ReadRange(
		ctx,
		lease,
		descriptor,
		content.Range{Offset: 0, End: 1},
		RangeSinkFunc(func(context.Context, uint64, []byte) error {
			sinkCalled = true
			return nil
		}),
	)
	if !errors.Is(err, context.Canceled) || sinkCalled {
		t.Fatalf("cancelled range read = %v, sink called = %v", err, sinkCalled)
	}
}
