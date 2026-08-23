package content

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content/revisioncapacity"
)

func TestExplicitLeaseEndingsRetireIdleRevisionImmediately(t *testing.T) {
	for _, kind := range []LeaseEndKind{LeaseRelinquished, LeaseUndelivered} {
		t.Run(leaseEndKindName(kind), func(t *testing.T) {
			clock := &testClock{now: time.Unix(100, 0)}
			file, record := fileRecord(t, 1)
			stable := &testStableFile{data: []byte{1}}
			store, process, share := newRevisionStore(t, &testRevisionSource{files: []*testStableFile{stable}}, clock, nil, file, record)
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
			if process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) ||
				session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
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
	store, process, share := newRevisionStore(t, &testRevisionSource{files: []*testStableFile{stable}}, clock, nil, file, record)
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
	if process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) ||
		session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
		t.Fatal("capacity retained after initial-range rollback")
	}
}

func TestDetachedDeadlineSurvivesActiveAndLaterRelinquishedLeases(t *testing.T) {
	startedAt := time.Unix(100, 0)
	clock := &testClock{now: startedAt}
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	source := &testRevisionSource{files: []*testStableFile{stable}}
	store, process, _ := newRevisionStore(t, source, clock, nil, file, record)
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
	store.mu.Lock()
	deadline := store.revisions[file].recoveryUntil
	store.mu.Unlock()
	if deadline != expectedDeadline {
		t.Fatalf("detached recovery deadline=%v want=%v", deadline, expectedDeadline)
	}

	clock.Advance(RevisionResumeGrace / 2)
	if err := store.EndLease(active.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	if stable.closed.Load() != 0 || process.Snapshot().Used != (QuotaUsage{StableHandles: 1}) {
		t.Fatalf("earlier detachment was erased: closes=%d quota=%+v", stable.closed.Load(), process.Snapshot().Used)
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
	store.mu.Lock()
	deadline = store.revisions[file].recoveryUntil
	store.mu.Unlock()
	if deadline != expectedDeadline {
		t.Fatalf("lease acquisition changed detached deadline=%v want=%v", deadline, expectedDeadline)
	}

	clock.Advance(RevisionResumeGrace / 2)
	if err := store.ValidateLease(reopened.ID(), reopened.Descriptor()); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("ended lease validation=%v", err)
	}
	if stable.closed.Load() != 1 || process.Snapshot().Used != (QuotaUsage{}) {
		t.Fatalf("expired detached recovery closes=%d quota=%+v", stable.closed.Load(), process.Snapshot().Used)
	}
}

func TestDetachedLeaseEndingExtendsRevisionRecoveryToLatestDeadline(t *testing.T) {
	startedAt := time.Unix(100, 0)
	clock := &testClock{now: startedAt}
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store, _, _ := newRevisionStore(t, &testRevisionSource{files: []*testStableFile{stable}}, clock, nil, file, record)
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
	store, _, _ := newRevisionStore(t, &testRevisionSource{files: []*testStableFile{stable}}, clock, nil, file, record)
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
