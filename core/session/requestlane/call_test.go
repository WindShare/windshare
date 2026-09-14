package requestlane

import (
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestCallReleasesRetriesAndRetainsDecisionAfterClose(t *testing.T) {
	now := time.Unix(100, 0)
	kind := protocolsession.MessageReleaseLease
	first, second := New(time.Millisecond), New(2*time.Millisecond)
	var call Call
	if !call.Reserve(first.Reserve(kind, now), first.Estimate(kind, now, 0)) {
		t.Fatal("first reservation")
	}
	decision := second.Estimate(kind, now, 0)
	if !call.Reserve(second.Reserve(kind, now), decision) {
		t.Fatal("proven-unsent retry")
	}
	if first.Estimate(kind, now, 0).Pending != 0 {
		t.Fatal("retry leaked old reservation")
	}
	call.Complete(now.Add(5 * time.Millisecond))
	call.Close()
	call.Close()
	call.Complete(now.Add(time.Second))
	if call.Estimate() != decision || second.Estimate(kind, now, 0).Response != 5*time.Millisecond {
		t.Fatal("cleanup changed response sample or dispatch evidence")
	}
	if call.Reserve(second.Reserve(kind, now), decision) || second.Estimate(kind, now, 0).Pending != 0 {
		t.Fatal("closed call leaked reservation")
	}
}

func TestCompletionAndCancellationShareOneReservationSettlement(t *testing.T) {
	now := time.Unix(100, 0)
	kind := protocolsession.MessageOpenRevisions
	lane := New(time.Millisecond)
	var call Call
	call.Reserve(lane.Reserve(kind, now), lane.Estimate(kind, now, 0))
	var joined sync.WaitGroup
	joined.Go(func() { call.Complete(now.Add(20 * time.Millisecond)) })
	joined.Go(call.Close)
	joined.Wait()
	got := lane.Estimate(kind, now, 0)
	if got.Pending != 0 || (got.Response != time.Millisecond && got.Response != 20*time.Millisecond) {
		t.Fatalf("settlement race corrupted accounting: %+v", got)
	}
}
