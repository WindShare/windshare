package lanescheduling

import (
	"testing"
	"time"
)

func TestDeliveryProgressExcludesNewerReservationsAndReceivedBytes(t *testing.T) {
	const block = uint64(1 << 20)
	now := time.Unix(1, 0)
	var window ReceiveWindow
	first := window.TryReserve(block, now)
	defer first.Close()
	now = now.Add(100 * time.Millisecond)
	first.Receive(block/2, now)
	if got := window.EstimateCompletion(block, now); got.Evidence != DeliveryRecent || got.Duration != 300*time.Millisecond {
		t.Fatal(got)
	}
	before := first.Progress(now).Remaining
	next := window.TryReserve(block, now)
	if next == nil {
		t.Fatal("receipt did not replenish supply")
	}
	defer next.Close()
	if first.Progress(now).Remaining != before {
		t.Fatal("newer work postponed the original attempt")
	}
	now = now.Add(100 * time.Millisecond)
	first.Receive(block/2, now)
	if first.Progress(now).RemainingBytes != 0 || window.UnfinishedBytes() != block {
		t.Fatal("authentication charged as network work")
	}
	first.Close()
	next.Close()
	if window.Load(now).PendingResponses != 0 {
		t.Fatal("closed reservations retained network work")
	}
}

func TestIdleDeliveryEvidenceExpiresWithoutPreventingColdStart(t *testing.T) {
	for _, closed := range []bool{true, false} {
		const block = uint64(1 << 20)
		now := time.Unix(1, 0)
		var window ReceiveWindow
		warm := window.TryReserve(block, now)
		now = now.Add(10 * time.Millisecond)
		warm.Receive(block, now)
		if closed {
			warm.Close()
		}
		defer warm.Close()
		now = now.Add(EvidenceLifetime + time.Millisecond)
		if window.Load(now).Evidence != DeliveryStale {
			t.Fatal("old receipt retained current authority")
		}
		first := window.TryReserve(block, now)
		if first == nil {
			t.Fatal("cold start waited for evidence")
		}
		defer first.Close()
		if window.Load(now).Evidence != DeliveryUnknown || window.TryReserve(block, now) != nil {
			t.Fatal("stale capacity admitted a backlog")
		}
	}
}

func TestRecentEvidenceOutranksBootstrapButNotOtherMeasuredCapacity(t *testing.T) {
	recent := DeliveryEstimate{Evidence: DeliveryRecent, Duration: time.Second}
	unknown := DeliveryEstimate{Evidence: DeliveryUnknown, Duration: InitialBlockTime}
	stale := DeliveryEstimate{Evidence: DeliveryStale, Duration: time.Millisecond}
	if CompareDelivery(recent, unknown) >= 0 || CompareDelivery(recent, stale) != 0 ||
		ConservativeDuration(stale) != 2*time.Millisecond ||
		CompareDelivery(recent, DeliveryEstimate{Evidence: DeliveryRecent, Duration: time.Millisecond}) != 0 {
		t.Fatal("bootstrap must rank after observations; stale observations require conservative cost")
	}
}

func TestStreamingCapacitySurvivesStartupButRespondsToSlowerFragments(t *testing.T) {
	const block = uint64(1 << 20)
	now := time.Unix(1, 0)
	var receive ReceiveWindow
	first := receive.TryReserve(block, now)
	now = now.Add(100 * time.Millisecond)
	first.Receive(block/2, now)
	now = now.Add(10 * time.Millisecond)
	first.Receive(block/2, now)
	first.Close()
	before := receive.Load(now)
	next := receive.TryReserve(block, now)
	defer next.Close()
	now = now.Add(100 * time.Millisecond)
	next.Receive(block/2, now)
	if got := receive.Load(now); got.BytesPerSecond != before.BytesPerSecond || got.Startup != before.Startup {
		t.Fatal("new response latency corrupted streaming capacity")
	}
	now = now.Add(100 * time.Millisecond)
	next.Receive(block/4, now)
	if receive.Load(now).BytesPerSecond >= before.BytesPerSecond {
		t.Fatal("slower streaming fragments did not reduce capacity")
	}
}
