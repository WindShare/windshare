package share

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/diagnostics"
)

type fakeSleepInhibitor struct {
	acquires atomic.Int32
	releases atomic.Int32
	err      error
}

func (f *fakeSleepInhibitor) InhibitSleep(_ string) (func(), error) {
	f.acquires.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return func() { f.releases.Add(1) }, nil
}

func TestSleepGuardRefcountsConcurrentSessions(t *testing.T) {
	t.Parallel()

	inhibitor := &fakeSleepInhibitor{}
	guard := newSleepGuard(inhibitor, diagnostics.Sink{})

	leaveFirst := guard.enter()
	leaveSecond := guard.enter()
	if got := inhibitor.acquires.Load(); got != 1 {
		t.Fatalf("acquires = %d, want 1", got)
	}
	if got := inhibitor.releases.Load(); got != 0 {
		t.Fatalf("releases = %d, want 0", got)
	}

	leaveFirst()
	// Repeated leave calls must be idempotent and keep the second session protected.
	leaveFirst()
	if got := inhibitor.releases.Load(); got != 0 {
		t.Fatalf("releases after first exit = %d, want 0", got)
	}

	leaveSecond()
	if got := inhibitor.releases.Load(); got != 1 {
		t.Fatalf("releases after last exit = %d, want 1", got)
	}

	// Subsequent sessions acquire cleanly again.
	leaveThird := guard.enter()
	if got := inhibitor.acquires.Load(); got != 2 {
		t.Fatalf("acquires on re-entry = %d, want 2", got)
	}
	leaveThird()
	if got := inhibitor.releases.Load(); got != 2 {
		t.Fatalf("releases on re-entry exit = %d, want 2", got)
	}
}

func TestSleepGuardRecordsAcquireFailureWithoutBlocking(t *testing.T) {
	t.Parallel()

	stream, err := diagnostics.NewStream(8, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	inhibitor := &fakeSleepInhibitor{err: errors.New("os assertion unavailable")}
	guard := newSleepGuard(inhibitor, stream.Sink())

	leave := guard.enter()
	leave()
	stream.Close()

	var names []string
	for record := range stream.Records() {
		names = append(names, record.Name())
	}
	if len(names) != 1 || names[0] != "share_sleep_inhibit" {
		t.Fatalf("diagnostic records = %v, want [share_sleep_inhibit]", names)
	}
}
