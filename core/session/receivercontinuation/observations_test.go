package receivercontinuation

import (
	"context"
	"testing"

	"github.com/windshare/windshare/core/session/sessionruntime"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

func TestSchedulingObservationsFollowReplacementAndOwnerLifetime(t *testing.T) {
	f := newFixture(t)
	first, channel := f.connect()
	next, _ := f.connect()
	s, err := New(t.Context(), first, func(context.Context, *sessionruntime.ReceiverRuntime) (*sessionruntime.ReceiverRuntime, error) {
		return next, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	releaseFirst := s.ObserveRemainingWork(func() transferlanes.RemainingWork {
		return transferlanes.RemainingWork{Bytes: 100, Known: true}
	})
	releaseSecond := s.ObserveRemainingWork(func() transferlanes.RemainingWork {
		return transferlanes.RemainingWork{Bytes: 200, Known: true}
	})
	assertRemaining := func(runtime *sessionruntime.ReceiverRuntime, bytes uint64, known bool) {
		t.Helper()
		demand := runtime.LaneSet().SchedulingDemand()
		if demand.RemainingBytes != bytes || demand.RemainingKnown != known {
			t.Fatalf("remaining = %+v; want %d/%t", demand, bytes, known)
		}
	}
	assertRemaining(first, 300, true)
	s.RecordOutput(10)
	_ = channel.Close()
	<-first.Done()
	if err := s.recover(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	assertRemaining(first, 0, false)
	assertRemaining(next, 300, true)
	s.RecordOutput(20)
	if first.LaneSet().SchedulingDemand().OutputBytes != 10 || next.LaneSet().SchedulingDemand().OutputBytes != 20 {
		t.Fatal("output observation escaped its active generation")
	}
	releaseFirst()
	releaseFirst()
	assertRemaining(next, 200, true)
	s.Close()
	assertRemaining(next, 0, false)
	releaseSecond()
	s.ObserveRemainingWork(func() transferlanes.RemainingWork {
		t.Error("closed owner observed work")
		return transferlanes.RemainingWork{}
	})()
	s.RecordOutput(100)
	assertRemaining(next, 0, false)
	if next.LaneSet().SchedulingDemand().OutputBytes != 20 {
		t.Fatal("closed owner recorded output")
	}
}
