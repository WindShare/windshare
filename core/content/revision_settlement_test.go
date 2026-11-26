package content

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

func TestExplicitLeaseEndingsRetireIdleRevisionImmediately(t *testing.T) {
	for _, kind := range []LeaseEndKind{LeaseRelinquished, LeaseUndelivered} {
		t.Run(leaseEndKindName(kind), func(t *testing.T) {
			clock := &testClock{now: time.Unix(100, 0)}
			file, record := fileRecord(t, 1)
			stable := &testStableFile{data: []byte{1}}
			store := newRevisionStore(t, &testRevisionSource{files: []*testStableFile{stable}}, clock, nil, file, record)
			var traces []RevisionTrace
			store.tracer = RevisionTracerFunc(func(event RevisionTrace) { traces = append(traces, event) })
			session := generousSession(t, store, "session")
			lease, err := store.OpenRevision(context.Background(), file, session)
			if err != nil {
				t.Fatal(err)
			}

			if err := store.EndLease(lease.ID(), kind); err != nil {
				t.Fatal(err)
			}
			if stable.closed.Load() != 1 {
				t.Fatalf("stable source closes=%d", stable.closed.Load())
			}
			if store.CapacitySnapshot().Process().Used() != (CapacityUsage{}) ||
				store.CapacitySnapshot().Share().Used() != (CapacityUsage{}) ||
				session.Snapshot().Used() != (CapacityUsage{}) {
				t.Fatalf("capacity retained after ending %v", kind)
			}
			var settlement RevisionTrace
			for _, trace := range traces {
				if trace.Stage() == RevisionTraceStageLeaseSettlement {
					settlement = trace
					break
				}
			}
			if settlement.LeaseID() != lease.ID() || settlement.SessionID() != session.SessionID() {
				t.Fatalf("lease settlement correlation = lease %x session %q", settlement.LeaseID(), settlement.SessionID())
			}
		})
	}
}

func TestInitialRangeFailureRollsBackUndeliveredLeaseImmediately(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store := newRevisionStore(t, &testRevisionSource{files: []*testStableFile{stable}}, clock, nil, file, record)
	session := generousSession(t, store, "session")
	outside, err := NewRangeSet([]Range{{Offset: 0, End: 2}})
	if err != nil {
		t.Fatal(err)
	}

	results, err := store.OpenRevisions(context.Background(), []OpenRevisionRequest{{FileID: file, InitialRanges: outside}}, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !errors.Is(results[0].Err, ErrBlockOutOfRange) || !results[0].Lease.ID().IsZero() {
		t.Fatalf("initial-range rollback results=%+v", results)
	}
	if stable.closed.Load() != 1 {
		t.Fatalf("undelivered initial-range source closes=%d", stable.closed.Load())
	}
	if store.CapacitySnapshot().Process().Used() != (CapacityUsage{}) ||
		store.CapacitySnapshot().Share().Used() != (CapacityUsage{}) ||
		session.Snapshot().Used() != (CapacityUsage{}) {
		t.Fatal("capacity retained after initial-range rollback")
	}
}

func TestDetachedDeadlineSurvivesActiveAndLaterRelinquishedLeases(t *testing.T) {
	startedAt := time.Unix(100, 0)
	clock := &testClock{now: startedAt}
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	source := &testRevisionSource{files: []*testStableFile{stable}}
	store := newRevisionStore(t, source, clock, nil, file, record)
	sessionA := generousSession(t, store, "session-a")
	sessionB := generousSession(t, store, "session-b")

	detached, err := store.OpenRevision(context.Background(), file, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.OpenRevision(context.Background(), file, sessionB)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EndLease(detached.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}
	expectedDeadline := startedAt.Add(RevisionResumeGrace)
	store.pool.mu.Lock()
	deadline := store.revisions[file].recoveryUntil
	store.pool.mu.Unlock()
	if deadline != expectedDeadline {
		t.Fatalf("detached recovery deadline=%v want=%v", deadline, expectedDeadline)
	}

	clock.Advance(RevisionResumeGrace / 2)
	if err := store.EndLease(active.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	if stable.closed.Load() != 0 || store.CapacitySnapshot().Process().Used() != (CapacityUsage{StableHandles: 1}) {
		t.Fatalf("earlier detachment was erased: closes=%d capacity=%+v", stable.closed.Load(), store.CapacitySnapshot().Process().Used())
	}

	reopened, err := store.OpenRevision(context.Background(), file, sessionB)
	if err != nil {
		t.Fatal(err)
	}
	if source.Calls() != 1 {
		t.Fatalf("live detached recovery reopened source %d times", source.Calls())
	}
	if err := store.EndLease(reopened.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	store.pool.mu.Lock()
	deadline = store.revisions[file].recoveryUntil
	store.pool.mu.Unlock()
	if deadline != expectedDeadline {
		t.Fatalf("lease acquisition changed detached deadline=%v want=%v", deadline, expectedDeadline)
	}

	clock.Advance(RevisionResumeGrace / 2)
	if err := store.ValidateLease(reopened.ID(), reopened.Descriptor()); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("ended lease validation=%v", err)
	}
	if stable.closed.Load() != 1 || store.CapacitySnapshot().Process().Used() != (CapacityUsage{}) {
		t.Fatalf("expired detached recovery closes=%d capacity=%+v", stable.closed.Load(), store.CapacitySnapshot().Process().Used())
	}
}

func TestDetachedLeaseEndingExtendsRevisionRecoveryToLatestDeadline(t *testing.T) {
	startedAt := time.Unix(100, 0)
	clock := &testClock{now: startedAt}
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store := newRevisionStore(t, &testRevisionSource{files: []*testStableFile{stable}}, clock, nil, file, record)
	session := generousSession(t, store, "session")
	first, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EndLease(first.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	if err := store.EndLease(second.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}

	clock.Advance(RevisionResumeGrace - time.Second)
	if err := store.ValidateLease(first.ID(), first.Descriptor()); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("detached lease validation=%v", err)
	}
	if stable.closed.Load() != 0 {
		t.Fatal("first detached deadline retired the later recovery window")
	}
	clock.Advance(time.Second)
	if err := store.ValidateLease(second.ID(), second.Descriptor()); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("second detached lease validation=%v", err)
	}
	if stable.closed.Load() != 1 {
		t.Fatalf("latest detached deadline source closes=%d", stable.closed.Load())
	}
}

func TestEndLeaseRejectsMissingEvidenceKind(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store := newRevisionStore(t, &testRevisionSource{files: []*testStableFile{stable}}, clock, nil, file, record)
	session := generousSession(t, store, "session")
	lease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EndLease(lease.ID(), LeaseEndKind(0)); !errors.Is(err, ErrInvalidLeaseEnd) {
		t.Fatalf("invalid lease ending error=%v", err)
	}
	if err := store.ValidateLease(lease.ID(), lease.Descriptor()); err != nil {
		t.Fatalf("invalid ending mutated active lease: %v", err)
	}
}

func TestEndLeasePinsStoreCloseUntilCleanupAndObserversComplete(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	pool := newCapacityTestPool(t, CapacityLimits{StableHandles: 2, ActiveLeases: 2}, nil)
	limits := CapacityLimits{StableHandles: 2, ActiveLeases: 2}
	file, record := capacityRecord(t, 87)
	stable := &capacityStableFile{
		data: []byte{1}, closeStarted: make(chan struct{}), closeRelease: make(chan struct{}),
	}
	cache := &recordingInvalidator{}
	traces := &revisionTraceRecorder{}
	store := newObservedCapacityStore(
		t, pool, 1, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{file: stable}, calls: make(map[catalog.FileID]int)},
		cache, traces,
	)
	var releaseOnce sync.Once
	releaseClose := func() { releaseOnce.Do(func() { close(stable.closeRelease) }) }
	t.Cleanup(releaseClose)

	session := capacitySession(t, store, "close-join-session", limits)
	lease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}

	endErr := make(chan error, 1)
	go func() {
		endErr <- store.EndLease(lease.ID(), LeaseRelinquished)
	}()
	<-stable.closeStarted

	closeErr := make(chan error, 1)
	go func() {
		closeErr <- store.Close()
	}()
	for !store.CapacitySnapshot().Closed() {
		runtime.Gosched()
	}
	if err := store.EndLease(lease.ID(), LeaseRelinquished); err != nil {
		t.Fatalf("EndLease on closing store = %v", err)
	}
	select {
	case err := <-closeErr:
		t.Fatalf("store Close returned while EndLease cleanup was in flight: %v", err)
	default:
	}

	releaseClose()
	if err := <-closeErr; err != nil {
		t.Fatalf("store Close during in-flight EndLease = %v", err)
	}
	// Once store.Close returns, the detached handle close, handleHold release,
	// cache retirement, and revision traces must already be settled.
	if stable.closed.Load() != 1 {
		t.Fatalf("stable closes = %d, want 1", stable.closed.Load())
	}
	invalidated, retired := cache.snapshot()
	if len(invalidated) != 0 || len(retired) != 1 || retired[0] != lease.Descriptor().FileRevision() {
		t.Fatalf("cache events on Close return invalidated=%v retired=%v", invalidated, retired)
	}
	events := traces.snapshot()
	if !hasRevisionTrace(events, RevisionTraceStageLeaseSettlement, RevisionTraceCauseRelinquished) ||
		!hasRevisionTrace(events, RevisionTraceStageCleanRelease, RevisionTraceCauseRelinquished) {
		t.Fatalf("missing EndLease traces on Close return: %+v", events)
	}
	if err := <-endErr; err != nil {
		t.Fatalf("in-flight EndLease = %v", err)
	}
	if snapshot := store.CapacitySnapshot(); snapshot.Process().Used() != (CapacityUsage{}) {
		t.Fatalf("post-close capacity retained: %+v", snapshot)
	}
}

func leaseEndKindName(kind LeaseEndKind) string {
	switch kind {
	case LeaseRelinquished:
		return "relinquished"
	case LeaseUndelivered:
		return "undelivered"
	default:
		return "unknown"
	}
}
