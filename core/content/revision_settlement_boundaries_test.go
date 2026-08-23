package content

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
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
	store, process, share := newRevisionStore(
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
	if process.Snapshot().Used != (QuotaUsage{StableHandles: 1, ActiveLeases: 1}) ||
		share.Snapshot().Used != (QuotaUsage{StableHandles: 1, ActiveLeases: 1}) ||
		originalSession.Snapshot().Used() != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) ||
		failedSession.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
		t.Fatalf("failed resident admission changed capacity: %+v", store.CapacitySnapshot())
	}
	if err := store.ValidateLease(original.ID(), original.Descriptor()); err != nil || stable.closed.Load() != 0 {
		t.Fatalf("failed resident admission changed source authority: validate=%v closes=%d", err, stable.closed.Load())
	}
}

func TestRevisionInvalidationRegistryRemainsIdempotentAcrossActiveReaders(t *testing.T) {
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store, _, _ := newRevisionStore(
		t, &testRevisionSource{files: []*testStableFile{stable}},
		&testClock{now: time.Unix(100, 0)}, nil, file, record,
	)
	session := generousSession(t, store, "reader-session")
	lease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	revision := store.revisions[file]
	revision.readers = 1
	store.readingRevisions[revision] = struct{}{}
	identity := revision.identity()
	cleanups, releases, invalidateErr := store.invalidateIdentityLocked(identity, revision)
	if invalidateErr != nil || len(cleanups) != 0 {
		store.mu.Unlock()
		t.Fatalf("reader invalidation=(cleanups=%d,%v)", len(cleanups), invalidateErr)
	}
	if err := store.recordInvalidationLocked(identity); err != nil {
		store.mu.Unlock()
		t.Fatalf("idempotent invalidation=%v", err)
	}
	store.revisionAdmissionStopped = true
	otherIdentity := revisionIdentity{
		file: catalogID[catalog.FileID](0xb7), revision: lease.Descriptor().FileRevision(),
	}
	if err := store.recordInvalidationLocked(otherIdentity); !errors.Is(err, ErrQuotaExceeded) {
		store.mu.Unlock()
		t.Fatalf("invalidation after admission stop=%v", err)
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
	if stable.closed.Load() != 1 || store.metadataBudget.Snapshot().Used != 1 ||
		session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
		t.Fatalf("invalidation cleanup closes=%d metadata=%+v capacity=%+v",
			stable.closed.Load(), store.metadataBudget.Snapshot(), session.Snapshot())
	}
}
