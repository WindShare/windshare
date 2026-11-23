package contentflow

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type recordedTransferActivity struct {
	enters atomic.Int32
	leaves atomic.Int32
	active atomic.Int32
}

func (a *recordedTransferActivity) BeginTransfer() func() {
	a.enters.Add(1)
	a.active.Add(1)
	return func() {
		a.active.Add(-1)
		a.leaves.Add(1)
	}
}

func TestSenderServiceTransferActivityTracksContentDeliveryOnly(t *testing.T) {
	t.Parallel()

	fixture := newRuntimeFixture(t, 1)
	defer fixture.close(t)

	activity := &recordedTransferActivity{}
	fixture.service.activity = activity

	// Failed revision opens do not count as active content transfers.
	missingReq, err := NewOpenRequest([]OpenItem{{FileID: flowID[catalog.FileID](99)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Open(context.Background(), missingReq); err != nil {
		t.Fatal(err)
	}
	if activity.enters.Load() != 0 {
		t.Fatalf("enters after failed open = %d, want 0", activity.enters.Load())
	}

	// Valid revision open signals transfer onset and balances immediately.
	openReq, err := NewOpenRequest([]OpenItem{{FileID: fixture.file}})
	if err != nil {
		t.Fatal(err)
	}
	openResults, err := fixture.service.Open(context.Background(), openReq)
	if err != nil {
		t.Fatal(err)
	}
	leaseID := openResults.Items()[0].Lease.ID()
	if activity.enters.Load() != 1 || activity.leaves.Load() != 1 || activity.active.Load() != 0 {
		t.Fatalf("after valid open: enters=%d leaves=%d active=%d, want 1/1/0",
			activity.enters.Load(), activity.leaves.Load(), activity.active.Load())
	}

	// Lease renewal keeps metadata alive for idle previews without marking active transfer.
	fixture.clock.Advance(content.LeaseRenewWindow)
	if _, err := fixture.service.Renew(leaseID); err != nil {
		t.Fatal(err)
	}
	if activity.enters.Load() != 1 {
		t.Fatalf("enters after renew = %d, want 1", activity.enters.Load())
	}

	// Unowned block requests must not trigger transfer activity.
	unownedReq, err := NewBlockRequest(flowID[content.LeaseID](77), []uint64{0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.ServeBlocks(context.Background(), protocolsession.OperationID(1), unownedReq, func(context.Context, protocolsession.Message) error {
		return nil
	}); err == nil {
		t.Fatal("expected error serving blocks for unowned lease")
	}
	if activity.enters.Load() != 1 {
		t.Fatalf("enters after rejected block request = %d, want 1", activity.enters.Load())
	}

	// Serving blocks holds transfer activity open until block delivery completes.
	fixture.stable.readStart = make(chan struct{}, 1)
	fixture.stable.readGate = make(chan struct{})
	blockReq, err := NewBlockRequest(leaseID, []uint64{0})
	if err != nil {
		t.Fatal(err)
	}
	serveErr := make(chan error, 1)
	go func() {
		_, err := fixture.service.ServeBlocks(context.Background(), protocolsession.OperationID(2), blockReq, func(context.Context, protocolsession.Message) error {
			return nil
		})
		serveErr <- err
	}()
	<-fixture.stable.readStart
	if activity.active.Load() != 1 || activity.enters.Load() != 2 || activity.leaves.Load() != 1 {
		t.Fatalf("during block read: enters=%d leaves=%d active=%d, want 2/1/1",
			activity.enters.Load(), activity.leaves.Load(), activity.active.Load())
	}
	close(fixture.stable.readGate)
	if err := <-serveErr; err != nil {
		t.Fatal(err)
	}
	if activity.active.Load() != 0 || activity.enters.Load() != 2 || activity.leaves.Load() != 2 {
		t.Fatalf("after block read: enters=%d leaves=%d active=%d, want 2/2/0",
			activity.enters.Load(), activity.leaves.Load(), activity.active.Load())
	}

	// Releasing the lease does not count as active transfer.
	if err := fixture.service.Release(leaseID); err != nil {
		t.Fatal(err)
	}
	if activity.enters.Load() != 2 || activity.leaves.Load() != 2 {
		t.Fatalf("after release: enters=%d leaves=%d, want 2/2", activity.enters.Load(), activity.leaves.Load())
	}
}
