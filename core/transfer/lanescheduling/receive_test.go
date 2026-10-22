package lanescheduling

import (
	"testing"
	"time"
)

func TestReceiptReplenishesSupplyBeforeAuthenticationCompletes(t *testing.T) {
	var window ReceiveWindow
	start := time.Unix(1, 0)
	first := window.TryReserve(1024, start)
	if first == nil || window.Available() {
		t.Fatal("cold lane must admit exactly one immediate request")
	}
	first.Receive(1024, start.Add(10*time.Millisecond))
	second := window.TryReserve(1024, start.Add(11*time.Millisecond))
	if second == nil || window.active != 2 {
		t.Fatal("received bytes did not replenish unfinished-response capacity")
	}
	first.Close()
	first.Close()
	second.Close()
	if window.active != 0 || window.remaining != 0 || !window.Available() {
		t.Fatal("settlement leaked receive capacity")
	}
}

func TestReceiptReportsReadinessEdgesWithoutDiscardingRateChanges(t *testing.T) {
	const blockBytes = 1 << 20
	const fragmentBytes = 64 << 10
	var window ReceiveWindow
	start := time.Unix(1, 0)
	permit := window.TryReserve(blockBytes, start)
	if !permit.Receive(fragmentBytes, start.Add(time.Millisecond)) {
		t.Fatal("fast first receipt did not announce capacity")
	}
	if permit.Receive(fragmentBytes, start.Add(time.Second)) || window.Available() {
		t.Fatal("slow streaming estimate did not close admission quietly")
	}
	if !permit.Receive(fragmentBytes, start.Add(time.Second+time.Millisecond)) {
		t.Fatal("recovered supply did not announce a new readiness edge")
	}
	if permit.Receive(fragmentBytes, start.Add(time.Second+2*time.Millisecond)) {
		t.Fatal("already available capacity generated another readiness edge")
	}
	permit.Close()
	if permit.Receive(fragmentBytes, start.Add(2*time.Second)) || permit.Receive(0, start) {
		t.Fatal("settled permit announced capacity")
	}
	var absent *ReceivePermit
	if absent.Receive(fragmentBytes, start) {
		t.Fatal("absent permit announced capacity")
	}
}

func TestCancelledUnfinishedResponseReleasesOnlyItsRemainingBytes(t *testing.T) {
	var window ReceiveWindow
	start := time.Unix(1, 0)
	first := window.TryReserve(1024, start)
	first.Receive(128, start.Add(time.Second))
	first.Close()
	first.Receive(1024, start.Add(2*time.Second))
	if window.remaining != 0 || window.receiving != 0 {
		t.Fatal("late receipt mutated a settled permit")
	}
	if window.TryReserve(1024, start.Add(3*time.Second)) == nil {
		t.Fatal("idle lane could not restart")
	}
}
