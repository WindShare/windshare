package transfer

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/capacityledger"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
)

func TestBrokerCloseJoinsOutputBeforeAuditingDeliveredBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		descriptor := transferDescriptor(t, 1)
		lane := &countingLane{records: map[uint64]records.BlockRecord{0: transferRecord(t, descriptor, 0)}}
		limit := uint64(catalog.MinChunkSize) * 3
		broker, lanes, _, lease, budget := newBrokerFixture(t, 1, lane, limit, limit)
		defer lanes.Close()
		writing, resume := make(chan struct{}), make(chan struct{})
		readDone, closeDone := make(chan error, 1), make(chan error, 1)
		go func() {
			readDone <- broker.ReadRange(context.Background(), lease, descriptor, content.Range{End: descriptor.ExactSize()},
				RangeSinkFunc(func(ctx context.Context, _ uint64, _ []byte) error {
					close(writing)
					<-resume
					return ctx.Err()
				}))
		}()
		<-writing
		go func() { closeDone <- broker.Close() }()
		synctest.Wait()
		select {
		case err := <-closeDone:
			t.Fatalf("close audited an active output: %v", err)
		default:
		}
		if budget.Used() == 0 {
			t.Fatal("shutdown revoked a buffer still used by output")
		}
		close(resume)
		if err := <-readDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("read shutdown: %v", err)
		}
		if err := <-closeDone; err != nil {
			t.Fatal(err)
		}
		if err := budget.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestBrokerCloseReportsAnUnreturnedReservation(t *testing.T) {
	broker := newBrokerCacheFixture(t, 8)
	held, ok := broker.account.Reserve(2)
	if !ok {
		t.Fatal("reservation failed")
	}
	var retained *capacityledger.UnreleasedError
	if err := broker.Close(); !errors.As(err, &retained) || retained.Used != 2 {
		t.Fatalf("close lost outstanding ownership: %v", err)
	}
	if broker.UsedBytes() != 2 {
		t.Fatal("close erased leaked accounting")
	}
	held.Release()
	if err := broker.Close(); err != nil {
		t.Fatal(err)
	}
}
