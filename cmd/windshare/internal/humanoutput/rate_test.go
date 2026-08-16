package humanoutput

import (
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
)

func TestRateWindowUsesInjectedMonotonicSamples(t *testing.T) {
	t.Parallel()
	base := time.Unix(1_000, 0)
	window := rateWindow{}
	observe := func(offset time.Duration, bytes uint64, exact bool) rateEstimate {
		return window.observe(base.Add(offset), mustSnapshot(t, clievent.ProgressSpec{
			DiscoveredBytes: 20_000_000, VerifiedBytes: bytes, NewlyVerifiedBytes: bytes,
			Discovery: clievent.DiscoveryOpen, CountersExact: exact,
		}))
	}
	if got := observe(0, 0, true); got.valid {
		t.Fatalf("first sample = %+v", got)
	}
	if got := observe(time.Second, 1_000_000, true); !got.valid || got.stable || got.bytesPerSecond != 1_000_000 {
		t.Fatalf("second sample = %+v", got)
	}
	if got := observe(2*time.Second, 2_000_000, true); !got.valid || !got.stable || got.bytesPerSecond != 1_000_000 {
		t.Fatalf("stable sample = %+v", got)
	}
	if got := observe(3*time.Second, 1_000_000, true); got.valid {
		t.Fatalf("counter regression should reset: %+v", got)
	}
	if got := observe(9*time.Second, 2_000_000, true); got.valid {
		t.Fatalf("out-of-window gap should reset: %+v", got)
	}
	if got := observe(10*time.Second, 3_000_000, false); got.valid {
		t.Fatalf("inexact sample should reset: %+v", got)
	}
	if got := observe(10*time.Second, 3_000_000, true); got.valid {
		t.Fatalf("non-increasing clock should reset: %+v", got)
	}
}

func TestRateWindowRejectsUnstableAndStalledSamples(t *testing.T) {
	t.Parallel()
	base := time.Unix(1_000, 0)
	window := rateWindow{}
	snapshot := func(bytes uint64) clievent.ProgressSnapshot {
		return mustSnapshot(t, clievent.ProgressSpec{
			DiscoveredBytes: 100_000_000, VerifiedBytes: bytes, NewlyVerifiedBytes: bytes,
			Discovery: clievent.DiscoveryComplete, CountersExact: true,
		})
	}
	window.observe(base, snapshot(0))
	window.observe(base.Add(time.Second), snapshot(1_000_000))
	if got := window.observe(base.Add(2*time.Second), snapshot(4_000_000)); !got.valid || got.stable {
		t.Fatalf("unstable estimate = %+v", got)
	}
	if got := window.observe(base.Add(3*time.Second), snapshot(4_000_000)); !got.valid || got.stable {
		t.Fatalf("stalled segment estimate = %+v", got)
	}
}

func TestBytesPerSecondSaturates(t *testing.T) {
	t.Parallel()
	if got := bytesPerSecond(^uint64(0), time.Nanosecond); got != ^uint64(0) {
		t.Fatalf("bytesPerSecond saturation = %d", got)
	}
	if got := bytesPerSecond(0, time.Second); got != 0 {
		t.Fatalf("zero delta rate = %d", got)
	}
}
