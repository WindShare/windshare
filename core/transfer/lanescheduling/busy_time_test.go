package lanescheduling

import (
	"testing"
	"time"
)

func TestBusyTimeExcludesIdleAndCountsOverlappingRequestsOnce(t *testing.T) {
	var performance Performance
	start := time.Time{}
	performance.Begin(start, 100)
	performance.Begin(start.Add(time.Second), 200)
	performance.Complete(start.Add(2*time.Second), 100, true)
	performance.Complete(start.Add(3*time.Second), 200, false)
	if actual := performance.BusyTime(start.Add(time.Hour)); actual != 3*time.Second {
		t.Fatalf("idle or overlapping work inflated busy time: %s", actual)
	}
	performance.Begin(start.Add(time.Hour), 100)
	if actual := performance.BusyTime(start.Add(time.Hour + time.Second)); actual != 4*time.Second {
		t.Fatalf("new busy interval lost prior measurement: %s", actual)
	}
}
