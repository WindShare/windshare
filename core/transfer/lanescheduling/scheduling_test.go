package lanescheduling

import (
	"testing"
	"time"
)

func TestBusyThroughputAndCompletionEstimate(t *testing.T) {
	const block = uint64(1024)
	now := time.Unix(100, 0)
	var p Performance
	p.Begin(now, block)
	p.Begin(now, block)
	p.Complete(now.Add(time.Second), block, true)
	if p.PendingBytes != block || p.BytesPerSecond != 1024 || !p.HasSuccessfulSample {
		t.Fatal(p)
	}
	p.Complete(now.Add(2*time.Second), block, true)
	if p.PendingBytes != 0 || p.BytesPerSecond != 1024 {
		t.Fatal(p)
	}
	p.Begin(now.Add(time.Hour), block)
	p.Complete(now.Add(time.Hour+time.Second), block, true)
	if p.BytesPerSecond != 1024 {
		t.Fatal("idle time poisoned throughput", p)
	}
	if Cost(time.Second, 1) >= Cost(time.Second, UserRelayCostFactor) ||
		Cost(time.Second, UserRelayCostFactor) >= Cost(time.Second, RelayCostFactor) ||
		Cost(time.Second, RelayCostFactor) >= Cost(2*time.Second, 1) {
		t.Fatal("relay cost overrides useful acceleration")
	}
}

func TestCensoredAndCancelledSamples(t *testing.T) {
	now := time.Unix(100, 0)
	var p Performance
	p.Begin(now, 100)
	p.Complete(now.Add(time.Second), 100, false)
	if p.BytesPerSecond != 0 || p.HasSuccessfulSample || p.PendingBytes != 0 {
		t.Fatal(p)
	}
	p.Superseded(0, time.Second)
	p.Superseded(100, 0)
	if p.BytesPerSecond != 0 {
		t.Fatal("invalid censored sample")
	}
	p.Superseded(100, time.Second)
	p.Superseded(100, time.Millisecond)
	if p.BytesPerSecond != 100 || p.HasSuccessfulSample {
		t.Fatal("cancellation became proof of completion", p)
	}
	p.Begin(now, 100)
	p.Complete(now.Add(-time.Second), 100, true)
	if p.BytesPerSecond <= 0 || p.BytesPerSecond > 100000 {
		t.Fatal("clock regression poisoned authenticated throughput", p)
	}
}

func TestIndependentExplorationBudgets(t *testing.T) {
	now := time.Unix(100, 0)
	var e Exploration
	var p Performance
	if !ProbeDue(&p, now) || !e.Acquire(Probe, now) || e.Acquire(Probe, now) {
		t.Fatal("probe admission")
	}
	if !e.Acquire(Rescue, now) || e.Acquire(Rescue, now) {
		t.Fatal("duplicate work was unbounded")
	}
	e.Release(Probe)
	if e.Acquire(Probe, now.Add(time.Second)) {
		t.Fatal("probe rate budget bypassed")
	}
	e.Release(Rescue)
	if !e.Acquire(Probe, now.Add(ProbeInterval)) {
		t.Fatal("probe did not recover")
	}
	e.Release(Probe)
	p.Begin(now, 100)
	if ProbeDue(&p, now.Add(ProbeInterval)) {
		t.Fatal("busy lane probed")
	}
	p.Complete(now, 100, true)
	if ProbeDue(&p, now.Add(time.Second)) || !ProbeDue(&p, now.Add(ProbeInterval)) {
		t.Fatal("sample aging")
	}
}

func TestBusyPathDoesNotKeepLifetimeAverageAfterCongestion(t *testing.T) {
	now := time.Unix(100, 0)
	var p Performance
	p.Begin(now, 1024)
	p.Begin(now, 1024)
	p.Complete(now.Add(time.Second), 1024, true)
	p.Begin(now.Add(time.Second), 1024)
	p.Complete(now.Add(11*time.Second), 1024, true)
	if p.BytesPerSecond != 102.4 || p.PendingBytes != 1024 {
		t.Fatal("old fast samples concealed current congestion", p)
	}
}
