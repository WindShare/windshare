package content

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content/revisioncapacity"
)

type failingLeaseIDs struct {
	err error
}

func (ids failingLeaseIDs) NewLeaseID() (LeaseID, error) {
	return LeaseID{}, ids.err
}

func TestResidentAdmissionFailurePreservesPublishedAuthorityAndAccounting(t *testing.T) {
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store := newRevisionStore(
		t, &testRevisionSource{files: []*testStableFile{stable}},
		&testClock{now: time.Unix(100, 0)}, nil, file, record,
	)
	originalSession := generousSession(t, store, "original-session")
	original, err := store.OpenRevision(context.Background(), file, originalSession)
	if err != nil {
		t.Fatal(err)
	}

	sentinel := errors.New("lease identity source unavailable")
	store.leaseIDs = failingLeaseIDs{err: sentinel}
	failedSession := generousSession(t, store, "failed-session")
	if _, err := store.OpenRevision(context.Background(), file, failedSession); !errors.Is(err, sentinel) {
		t.Fatalf("resident generator failure=%v", err)
	}
	if store.CapacitySnapshot().Process().Used() != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) ||
		store.CapacitySnapshot().Share().Used() != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) ||
		originalSession.Snapshot().Used() != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) ||
		failedSession.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
		t.Fatalf("failed resident admission changed capacity: %+v", store.CapacitySnapshot())
	}
	if err := store.ValidateLease(original.ID(), original.Descriptor()); err != nil || stable.closed.Load() != 0 {
		t.Fatalf("failed resident admission changed source authority: validate=%v closes=%d", err, stable.closed.Load())
	}
}

func TestRevisionInvalidationRemainsIdempotentAcrossActiveReaders(t *testing.T) {
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store := newRevisionStore(
		t, &testRevisionSource{files: []*testStableFile{stable}},
		&testClock{now: time.Unix(100, 0)}, nil, file, record,
	)
	session := generousSession(t, store, "reader-session")
	if _, err := store.OpenRevision(context.Background(), file, session); err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	revision := store.revisions[file]
	revision.readers = 1
	store.readingRevisions[revision] = struct{}{}
	identity := revision.identity()
	cleanups, releases := store.invalidateIdentityLocked(identity, revision)
	if len(cleanups) != 0 {
		store.mu.Unlock()
		t.Fatalf("reader invalidation closed active handle prematurely: cleanups=%d", len(cleanups))
	}
	extraCleanups, extraReleases := store.invalidateIdentityLocked(identity, revision)
	if len(extraCleanups) != 0 || len(extraReleases) != 0 {
		store.mu.Unlock()
		t.Fatalf("repeated invalidation was not idempotent: cleanups=%d releases=%d", len(extraCleanups), len(extraReleases))
	}
	delete(store.readingRevisions, revision)
	revision.readers = 0
	cleanup, finalReleases, ready := store.invalidateStateLocked(revision, store.clock.Now())
	store.mu.Unlock()
	for _, release := range append(releases, finalReleases...) {
		if err := release.run(); err != nil {
			t.Fatal(err)
		}
	}
	if !ready {
		t.Fatal("last reader did not release invalidated physical state")
	}
	if err := cleanup.run(); err != nil {
		t.Fatal(err)
	}
	if stable.closed.Load() != 1 || session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
		t.Fatalf("invalidation cleanup closes=%d capacity=%+v", stable.closed.Load(), session.Snapshot())
	}
}
