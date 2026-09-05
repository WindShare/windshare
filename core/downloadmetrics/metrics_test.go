package downloadmetrics

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"
)

func TestDownloadRecoveryCountsUniqueRevisionRangesAndActiveFallbackWait(t *testing.T) {
	now := time.Unix(0, 0)
	m := New("download", func() time.Time { return now }, true)
	m.Delivered("revision-a", 0, 100, Direct)
	now = now.Add(time.Second)
	m.Availability(false)
	// No queued content: browsing, user pause and output work are all idle.
	now = now.Add(time.Hour)
	wait := m.Pending()
	now = now.Add(3 * time.Second)
	wait()
	wait()
	now = now.Add(time.Hour)
	wait = m.Pending()
	now = now.Add(2 * time.Second)
	m.Delivered("revision-a", 50, 150, TURN)
	now = now.Add(time.Second)
	wait()
	m.Delivered("revision-a", 0, 150, ApplicationRelay)
	m.Delivered("revision-b", 0, 100, ApplicationRelay)
	m.Availability(true)
	got := m.Snapshot(true)
	if got.FirstDirectElapsed == nil || *got.FirstDirectElapsed != 0 || got.FallbackStall != 5*time.Second ||
		got.DirectBytes != 100 || got.TURNBytes != 50 || got.ApplicationRelayBytes != 100 ||
		got.DirectFraction == nil || *got.DirectFraction != 0.4 || got.Incomplete || !got.Final {
		t.Fatalf("%+v", got)
	}
	// Finalization freezes all counters and frees identity retention.
	now = now.Add(time.Hour)
	m.Pending()()
	m.Delivered("later", 0, 10, Direct)
	m.Availability(false)
	m.EvidenceLost()
	if frozen := m.Snapshot(false); frozen.FallbackStall != got.FallbackStall || frozen.Incomplete {
		t.Fatal(frozen)
	}
	if len(m.ranges) != 0 {
		t.Fatal("final summary retained revision identities")
	}
}
func TestFirstAdmissionUnknownAndBounds(t *testing.T) {
	now := time.Unix(0, 0)
	m := New("download", func() time.Time { return now }, false)
	if m.Snapshot(false).FirstDirectElapsed != nil {
		t.Fatal("invented direct")
	}
	now = now.Add(2 * time.Second)
	m.Availability(true)
	m.Delivered("r", 0, 10, Unknown)
	got := m.Snapshot(false)
	if got.FirstDirectElapsed == nil || *got.FirstDirectElapsed != 2*time.Second || got.UnknownBytes != 10 || !got.Incomplete || got.DirectFraction != nil {
		t.Fatal(got)
	}
	m = New("bounded", func() time.Time { return now }, false)
	for index := range MaximumIntervals + 1 {
		m.Delivered(fmt.Sprint(index), 0, 1, Direct)
	}
	if got = m.Snapshot(false); !got.Incomplete || got.DirectBytes != MaximumIntervals || len(m.ranges) != MaximumIntervals {
		t.Fatal(got)
	}
}
func TestIntervalUnionClockRegressionAndInvalidEvidence(t *testing.T) {
	now := time.Unix(0, 0)
	m := New("ranges", func() time.Time { return now }, false)
	m.Delivered("r", 10, 20, Direct)
	m.Delivered("r", 30, 40, TURN)
	m.Delivered("r", 0, 50, ApplicationRelay)
	if got := m.Snapshot(false); got.DirectBytes != 10 || got.TURNBytes != 10 || got.ApplicationRelayBytes != 30 {
		t.Fatal(got)
	}
	m.Delivered("", 0, 1, Direct)
	m.Delivered("r", 2, 1, Direct)
	m.Delivered("r", 0, 1, Route(255))
	now = now.Add(-time.Second)
	m.Availability(true)
	if !m.Snapshot(false).Incomplete {
		t.Fatal("invalid evidence not marked")
	}
	m = New("overflow", nil, false)
	m.bytes[Direct] = math.MaxUint64
	m.Delivered("r", 0, 1, Direct)
	if !m.Snapshot(false).Incomplete {
		t.Fatal("counter overflow not marked")
	}
}
func TestConcurrentDeliveryDeduplicates(t *testing.T) {
	m := New("concurrent", nil, true)
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() { done := m.Pending(); defer done(); m.Delivered("r", 0, 100, Direct); m.Snapshot(false) })
	}
	group.Wait()
	if got := m.Snapshot(true); got.DirectBytes != 100 || got.Incomplete {
		t.Fatal(got)
	}
}
func TestContentActivationStartsAfterPrewarm(t *testing.T) {
	now := time.Unix(0, 0)
	metrics := Prepare(func() time.Time { return now })
	metrics.Pending()()
	metrics.Delivered("r", 0, 10, Direct)
	metrics.Availability(true)
	now = now.Add(time.Hour)
	if metrics.Snapshot(false).FirstDirectElapsed != nil {
		t.Fatal("browse started download clock")
	}
	metrics.Activate("download")
	now = now.Add(time.Second)
	metrics.Activate("download")
	got := metrics.Snapshot(true)
	if got.FirstDirectElapsed == nil || *got.FirstDirectElapsed != 0 || got.DirectBytes != 0 {
		t.Fatal(got)
	}
	metrics.Activate("download")
}
