package lanes

import (
	"context"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

func TestSamplingOpportunityPreservesFrontierAndGeneration(t *testing.T) {
	set := newBoundaryLaneSet(t, 135)
	now := time.Unix(1, 0)
	set.now = func() time.Time { return now }
	demand := validDemand(t, transferDescriptor(t, 1), 0)
	bytes := blockDemandBytes(demand)
	source := laneFunction(func(context.Context, Demand) (records.BlockRecord, error) { return records.BlockRecord{}, nil })
	for _, id := range []uint32{1, 2} {
		if err := set.Add(Identity{ID: id, Epoch: 1}, RouteDirect, source); err != nil {
			t.Fatal(err)
		}
	}
	fast, slow := set.lanes[1], set.lanes[2]
	fast.performance = lanescheduling.Performance{HasSuccessfulSample: true, BytesPerSecond: float64(bytes) * 100, PendingBytes: bytes}
	fast.inflight = 1
	slow.performance = lanescheduling.Performance{HasSuccessfulSample: true, BytesPerSecond: float64(bytes), LastAttempt: now}
	seedDelivery(fast, bytes, 10*time.Millisecond, now)
	seedDelivery(slow, bytes, time.Second, now)
	permit := fast.receive.TryReserve(bytes*30, now)
	defer permit.Close()
	pending := &pendingAdmission{request: Request{Demand: demand}, order: WorkOrder{Distance: 1}}
	ordered := []*laneState{fast, slow}
	set.SampleLane(Identity{ID: 2, Epoch: 0})
	if lane, _ := set.pendingCandidateLocked(pending, ordered, now); lane != nil {
		t.Fatal("stale grant admitted sampling")
	}
	set.SampleLane(slow.identity)
	if lane, purpose := set.pendingCandidateLocked(pending, ordered, now); lane != slow || purpose != lanescheduling.Sample {
		t.Fatal("sampling waited for periodic probe")
	}
	pending.order = WorkOrder{Priority: Preview}
	if lane, _ := set.pendingCandidateLocked(pending, ordered, now); lane != nil {
		t.Fatal("sample stole the preview frontier")
	}
	pending.order = WorkOrder{}
	if lane, _ := set.pendingCandidateLocked(pending, ordered, now); lane != nil {
		t.Fatal("sample stole the only output frontier")
	}
	pending.order.Distance = 1
	set.StopSampling(slow.identity)
	if lane, _ := set.pendingCandidateLocked(pending, ordered, now); lane != nil {
		t.Fatal("sampling survived withdrawal")
	}
	if err := set.Add(Identity{ID: 2, Epoch: 2}, RouteDirect, source); err != nil {
		t.Fatal(err)
	}
	replacement := set.lanes[2]
	set.SampleLane(replacement.identity)
	set.StopSampling(slow.identity)
	if !replacement.sampling.Available(bytes, now) {
		t.Fatal("old trial withdrew replacement sampling")
	}
	if _, live := set.SamplingEvidence(slow.identity); live {
		t.Fatal("old epoch acquired replacement evidence")
	}
	if evidence, live := set.SamplingEvidence(replacement.identity); !live || evidence.OfferedBytes != 0 {
		t.Fatal("replacement inherited old work")
	}
	// This test reserves capacity directly rather than starting physical work.
	fast.inflight = 0
}

func TestSamplingFailureKeepsRecoveryAllowance(t *testing.T) {
	var exploration lanescheduling.Exploration
	now := time.Unix(1, 0)
	if !exploration.Acquire(lanescheduling.Sample, now) {
		t.Fatal("sample not admitted")
	}
	if exploration.Acquire(lanescheduling.Sample, now) || exploration.Acquire(lanescheduling.Probe, now) {
		t.Fatal("concurrent sampling escaped its allowance")
	}
	if !exploration.Acquire(lanescheduling.Rescue, now) {
		t.Fatal("sampling took the recovery reserve")
	}
	exploration.Release(lanescheduling.Sample)
	exploration.Release(lanescheduling.Rescue)
	if exploration.Available(lanescheduling.Probe, now) {
		t.Fatal("ordinary probes ignored the last sample")
	}
	if !exploration.Available(lanescheduling.Sample, now) {
		t.Fatal("bounded trial inherited periodic probe cooldown")
	}
}
