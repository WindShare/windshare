package transfer

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
)

func TestRangeAdmissionKeepsOutputFrontierAheadOfLaterWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		descriptor := transferDescriptor(t, 8)
		lane := &countingLane{records: make(map[uint64]records.BlockRecord)}
		for index := range uint64(8) {
			lane.records[index] = transferRecord(t, descriptor, index)
		}
		bytes := uint64(catalog.MinChunkSize)
		broker, lanes, _, lease, process := newBrokerFixture(t, 8, lane, 3*bytes, 3*bytes)
		defer lanes.Close()
		defer broker.Close()
		written := make(chan struct{})
		resume := make(chan struct{})
		done := make(chan error, 1)
		var offsets []uint64
		go func() {
			done <- broker.ReadRange(t.Context(), lease, descriptor, content.Range{Offset: 0, End: descriptor.ExactSize()},
				RangeSinkFunc(func(_ context.Context, offset uint64, _ []byte) error {
					offsets = append(offsets, offset)
					if offset == 0 {
						close(written)
						<-resume
					}
					return nil
				}))
		}()
		<-written
		synctest.Wait()
		if !slices.Equal(lane.indices(), []uint64{0}) || process.Used() > 3*bytes {
			t.Fatalf("paused output lost its bound: calls=%v bytes=%d", lane.indices(), process.Used())
		}
		close(resume)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		for index, offset := range offsets {
			if offset != uint64(index)*bytes {
				t.Fatalf("out-of-order offset %d", offset)
			}
		}
		if len(offsets) != 8 {
			t.Fatal("range did not complete")
		}
	})
}
