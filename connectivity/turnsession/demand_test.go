package turnsession

import (
	"context"
	"testing"
	"time"

	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/lanes"
)

func TestDemandOnlySpendsFallbackOnUndersuppliedContent(t *testing.T) {
	origin := time.Unix(1, 0)
	for _, tc := range []struct {
		name  string
		s     lanes.SchedulingDemand
		speed uint64
		want  p.TURNMode
	}{
		{"fast relay", lanes.SchedulingDemand{NetworkWaitingBytes: 1, RemainingBytes: 8 << 20}, 2 << 20, p.TURNParallel},
		{"network backoff with zero inflight", lanes.SchedulingDemand{NetworkWaitingBytes: 1, RemainingBytes: 8 << 20}, 0, p.TURNAll},
		{"occupied slow window", lanes.SchedulingDemand{InFlightBytes: 1 << 20, UnfinishedBytes: 1 << 20, RemainingBytes: 8 << 20}, 100 << 10, p.TURNAll},
		{"sink pressure", lanes.SchedulingDemand{CapacityWaitingBytes: 1, InFlightBytes: 1 << 20, RemainingBytes: 8 << 20}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d Demand
			s := tc.s
			var mode p.TURNMode
			for i := 0; i <= 5; i++ {
				s.UsefulBytes = uint64(i) * tc.speed
				mode = d.Observe(origin.Add(time.Duration(i)*time.Second), s)
			}
			if mode != tc.want {
				t.Fatalf("got %v want %v", mode, tc.want)
			}
		})
	}
	var d Demand
	short := lanes.SchedulingDemand{NetworkWaitingBytes: 1, RemainingBytes: 1 << 20}
	if d.Observe(origin, short) != 0 || d.Observe(origin.Add(2*time.Second), short) != 0 {
		t.Fatal("short download issued TURN")
	}
}

func TestDemandAveragesDeliveredBlocksAcrossSamplingTicks(t *testing.T) {
	origin := time.Unix(1, 0)
	const block = 1 << 20
	s := lanes.SchedulingDemand{InFlightBytes: block, RemainingBytes: 16 * block}
	var d Demand
	for tick := 0; tick <= 15; tick++ {
		// A 0.4 MiB/s path completes one default content block every 2.5s.
		s.UsefulBytes = uint64(2*tick/5) * block
		want := p.TURNParallel
		if tick >= 5 {
			want = p.TURNAll
		}
		if got := d.Observe(origin.Add(time.Duration(tick)*time.Second), s); got != want {
			t.Fatalf("at %ds: got %v, want %v", tick, got, want)
		}
	}
}

func TestDemandUsesElapsedPressureTimeAndIndependentStallClock(t *testing.T) {
	origin := time.Unix(1, 0)
	const block = 1 << 20
	var d Demand
	s := lanes.SchedulingDemand{NetworkWaitingBytes: 1, RemainingBytes: 16 * block}
	observe := func(elapsed time.Duration, useful uint64, want p.TURNMode) {
		t.Helper()
		s.UsefulBytes = useful
		if got := d.Observe(origin.Add(elapsed), s); got != want {
			t.Fatalf("at %s with %d bytes: got %v, want %v", elapsed, useful, got, want)
		}
	}
	observe(0, 0, p.TURNParallel)
	observe(2500*time.Millisecond, 2*block, p.TURNParallel)
	observe(5100*time.Millisecond, 5*block, p.TURNAll)
	observe(5100*time.Millisecond, 9*block, p.TURNAll)
	observe(6*time.Second, 9*block, p.TURNParallel)
	observe(9*time.Second, 9*block, p.TURNAll)

	// Sink pressure ends this delivery window, including its stall clock.
	s.NetworkWaitingBytes, s.CapacityWaitingBytes = 0, block
	observe(10*time.Second, 9*block, 0)
	s.NetworkWaitingBytes, s.CapacityWaitingBytes = 1, 0
	observe(11*time.Second, 9*block, p.TURNParallel)
}

func TestSharedNegotiationCapacityIsReturnedExactlyOnce(t *testing.T) {
	c := NewCapacity()
	var release []func()
	for range p.MaxGrantsPerMessage {
		give, err := c.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		release = append(release, give)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.Acquire(cancelled); err == nil {
		t.Fatal("exhausted process capacity admitted another task")
	}
	release[0]()
	release[0]()
	give, err := c.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire(cancelled); err == nil {
		t.Fatal("permit returned twice")
	}
	give()
	for _, f := range release {
		f()
	}
}
