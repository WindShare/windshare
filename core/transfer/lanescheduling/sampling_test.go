package lanescheduling

import (
	"testing"
	"time"
)

func TestSamplingSeparatesSettlementAndCapacityEvidence(t *testing.T) {
	start := time.Unix(1, 0)
	var sample Sampling
	sample.Offered(100, start)
	sample.Offered(100, start.Add(50*time.Millisecond))
	sample.Settled(100, start.Add(100*time.Millisecond), SampleCompleted)
	if sample.Snapshot(start.Add(150*time.Millisecond)).BusyMilliseconds != 150 {
		t.Fatal("overlapping busy time counted twice")
	}
	sample.Settled(100, start.Add(200*time.Millisecond), SampleCancelled)
	want := SamplingEvidence{OfferedBytes: 200, SettledBytes: 200, CompletedBytes: 100, CompletedAttempts: 1, CancelledAttempts: 1, BusyMilliseconds: 200}
	if got := sample.Snapshot(start.Add(5 * time.Second)); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestSamplingBoundsOpportunityWithoutDiscardingWork(t *testing.T) {
	now := time.Unix(1, 0)
	var sample Sampling
	if sample.Available(1, now) {
		t.Fatal("sampling without an owner")
	}
	sample.Start(now)
	sample.Offered(SampleByteBudget, now)
	if sample.Available(1, now) {
		t.Fatal("byte allowance exceeded")
	}
	sample.Settled(SampleByteBudget, now.Add(time.Millisecond), SampleFailed)
	sample.Start(now)
	for range SampleAttemptBudget {
		if !sample.Available(1, now) {
			t.Fatal("allowance ended early")
		}
		sample.Offered(1, now)
		sample.Settled(1, now.Add(time.Millisecond), SampleSuperseded)
	}
	if sample.Available(1, now) {
		t.Fatal("attempt allowance exceeded")
	}
	sample.Start(now)
	if sample.Available(1, now.Add(SampleLifetime)) {
		t.Fatal("expired sampling continued")
	}
	sample.Start(now)
	sample.Stop()
	if sample.Available(1, now) {
		t.Fatal("withdrawn sampling continued")
	}
	evidence := sample.Snapshot(now)
	if evidence.FailedAttempts != 1 || evidence.SupersededAttempts != SampleAttemptBudget {
		t.Fatalf("lost negative evidence: %+v", evidence)
	}
}
