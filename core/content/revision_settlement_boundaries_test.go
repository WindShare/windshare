package content

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

type failingLeaseIDs struct {
	err error
}

func (ids failingLeaseIDs) NewLeaseID() (LeaseID, error) {
	return LeaseID{}, ids.err
}

func TestAcquireLeaseFailureRollsBackEveryProvisionalAuthority(t *testing.T) {
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store, process, share := newRevisionStore(
		t, &testRevisionSource{files: []*testStableFile{stable}},
		&testClock{now: time.Unix(100, 0)}, nil, file, record,
	)
	originalSession := generousQuota(t, "original-session")
	original, err := store.OpenRevision(context.Background(), file, originalSession)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	revision := store.revisions[file]
	store.mu.Unlock()

	t.Run("inactive revision", func(t *testing.T) {
		session := generousQuota(t, "inactive-session")
		store.mu.Lock()
		revision.lifecycle = revisionLifecycleReleased
		_, acquireErr := store.acquireLeaseLocked(revision, session, store.clock.Now(), nil)
		revision.lifecycle = revisionLifecycleActive
		store.mu.Unlock()
		if !errors.Is(acquireErr, ErrRevisionDrift) || session.Snapshot().Used != (QuotaUsage{}) {
			t.Fatalf("inactive acquisition = (%v, %+v)", acquireErr, session.Snapshot().Used)
		}
	})

	t.Run("active lease quota", func(t *testing.T) {
		session := limitedQuota(t, "lease-full-session", QuotaLimits{StableHandles: 1, ActiveLeases: 1})
		full, err := reserveQuotaAccounts([]*QuotaAccount{session}, QuotaUsage{ActiveLeases: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer full.Release()
		store.mu.Lock()
		_, acquireErr := store.acquireLeaseLocked(revision, session, store.clock.Now(), nil)
		store.mu.Unlock()
		if !errors.Is(acquireErr, ErrQuotaExceeded) || session.Snapshot().Used != (QuotaUsage{ActiveLeases: 1}) {
			t.Fatalf("lease-full acquisition = (%v, %+v)", acquireErr, session.Snapshot().Used)
		}
	})

	t.Run("stable handle quota", func(t *testing.T) {
		session := limitedQuota(t, "handle-full-session", QuotaLimits{StableHandles: 1, ActiveLeases: 2})
		full, err := reserveQuotaAccounts([]*QuotaAccount{session}, QuotaUsage{StableHandles: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer full.Release()
		store.mu.Lock()
		_, acquireErr := store.acquireLeaseLocked(revision, session, store.clock.Now(), nil)
		store.mu.Unlock()
		if !errors.Is(acquireErr, ErrQuotaExceeded) || session.Snapshot().Used != (QuotaUsage{StableHandles: 1}) {
			t.Fatalf("handle-full acquisition = (%v, %+v)", acquireErr, session.Snapshot().Used)
		}
	})

	t.Run("generator failure", func(t *testing.T) {
		sentinel := errors.New("lease identity source unavailable")
		store.leaseIDs = failingLeaseIDs{err: sentinel}
		assertFailedLeaseAcquisition(t, store, revision, generousQuota(t, "generator-failure-session"), sentinel)
	})

	t.Run("zero identity", func(t *testing.T) {
		store.leaseIDs = fixedIDs{}
		assertFailedLeaseAcquisition(t, store, revision, generousQuota(t, "zero-identity-session"), nil)
	})

	t.Run("live identity reuse", func(t *testing.T) {
		store.leaseIDs = fixedIDs{lease: original.ID()}
		assertFailedLeaseAcquisition(t, store, revision, generousQuota(t, "duplicate-identity-session"), nil)
	})

	store.mu.Lock()
	if len(revision.leases) != 1 || len(revision.sessionHandles) != 1 {
		store.mu.Unlock()
		t.Fatalf("failed acquisition changed published authority: leases=%d handles=%d",
			len(revision.leases), len(revision.sessionHandles))
	}
	store.mu.Unlock()
	if process.Snapshot().Used != (QuotaUsage{StableHandles: 1, ActiveLeases: 1}) ||
		share.Snapshot().Used != (QuotaUsage{StableHandles: 1, ActiveLeases: 1}) {
		t.Fatalf("failed acquisition leaked shared quota: process=%+v share=%+v",
			process.Snapshot().Used, share.Snapshot().Used)
	}
}

func limitedQuota(t *testing.T, name string, limits QuotaLimits) *QuotaAccount {
	t.Helper()
	account, err := NewQuotaAccount(name, limits)
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func assertFailedLeaseAcquisition(
	t *testing.T,
	store *RevisionStore,
	revision *revisionState,
	session *QuotaAccount,
	want error,
) {
	t.Helper()
	store.mu.Lock()
	_, err := store.acquireLeaseLocked(revision, session, store.clock.Now(), nil)
	store.mu.Unlock()
	if err == nil || want != nil && !errors.Is(err, want) {
		t.Fatalf("failed acquisition error = %v, want %v", err, want)
	}
	if session.Snapshot().Used != (QuotaUsage{}) {
		t.Fatalf("failed acquisition retained session quota: %+v", session.Snapshot().Used)
	}
}

func TestCompleteOpenRejectsLateAuthorityWithoutLeakingPhysicalState(t *testing.T) {
	file, record := fileRecord(t, 1)
	newStore := func(t *testing.T) *RevisionStore {
		t.Helper()
		store, _, _ := newRevisionStore(t, &testRevisionSource{}, &testClock{now: time.Unix(100, 0)}, nil, file, record)
		return store
	}
	newResult := func(t *testing.T, stable *testStableFile) openWorkResult {
		t.Helper()
		geometry, err := NewFileGeometry(1, catalog.MinChunkSize)
		if err != nil {
			t.Fatal(err)
		}
		revisionID := contentID[FileRevision](0xa7)
		descriptor, err := NewFileRevisionDescriptor(
			catalogID[catalog.ShareInstance](1), file, revisionID, geometry, catalog.ModifiedTime{},
		)
		if err != nil {
			t.Fatal(err)
		}
		return openWorkResult{
			identity:   revisionIdentity{file: file, revision: revisionID},
			comparison: RevisionComparisonMatch,
			revision: &revisionState{
				descriptor: descriptor, source: stable,
				leases: make(map[LeaseID]*leaseState), sessionHandles: make(map[*QuotaAccount]*sessionHandleState),
				lifecycle: revisionLifecycleActive,
			},
		}
	}
	currentAttempt := func(store *RevisionStore) *openAttempt {
		attempt := &openAttempt{file: file, done: make(chan struct{}), waiters: 1}
		store.opening[file] = attempt
		return attempt
	}

	t.Run("duplicate completion", func(t *testing.T) {
		store := newStore(t)
		stable := &testStableFile{data: []byte{1}}
		store.completeOpen(&openAttempt{completed: true}, newResult(t, stable))
		if stable.closed.Load() != 1 {
			t.Fatal("duplicate completion retained its late physical source")
		}
	})

	tests := []struct {
		name  string
		setup func(*RevisionStore, openWorkResult)
		want  error
	}{
		{
			name: "admission stopped",
			setup: func(store *RevisionStore, _ openWorkResult) {
				store.revisionAdmissionStopped = true
			},
			want: ErrQuotaExceeded,
		},
		{
			name: "identity invalidated",
			setup: func(store *RevisionStore, result openWorkResult) {
				reservation, _ := store.metadataBudget.reserveInvalidation()
				store.invalidated[result.identity] = reservation
			},
			want: ErrRevisionDrift,
		},
		{
			name: "existing revision won race",
			setup: func(store *RevisionStore, result openWorkResult) {
				store.revisions[file] = &revisionState{
					descriptor: result.revision.descriptor,
					leases:     make(map[LeaseID]*leaseState), sessionHandles: make(map[*QuotaAccount]*sessionHandleState),
					lifecycle: revisionLifecycleActive,
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newStore(t)
			stable := &testStableFile{data: []byte{1}}
			result := newResult(t, stable)
			test.setup(store, result)
			attempt := currentAttempt(store)
			store.completeOpen(attempt, result)
			if attempt.err == nil || test.want != nil && !errors.Is(attempt.err, test.want) {
				t.Fatalf("settlement error = %v, want %v", attempt.err, test.want)
			}
			if stable.closed.Load() != 1 || attempt.revision != nil {
				t.Fatalf("rejected settlement retained authority: closes=%d revision=%+v",
					stable.closed.Load(), attempt.revision)
			}
			select {
			case <-attempt.done:
			default:
				t.Fatal("settlement result was not published")
			}
		})
	}

	t.Run("empty provider result", func(t *testing.T) {
		store := newStore(t)
		attempt := currentAttempt(store)
		store.completeOpen(attempt, openWorkResult{comparison: RevisionComparisonUnknown})
		if !errors.Is(attempt.err, context.Canceled) || attempt.revision != nil {
			t.Fatalf("empty settlement = (revision=%+v, %v)", attempt.revision, attempt.err)
		}
	})
}

func TestRevisionInvalidationRegistryRemainsIdempotentAcrossActiveReaders(t *testing.T) {
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store, _, _ := newRevisionStore(
		t, &testRevisionSource{files: []*testStableFile{stable}},
		&testClock{now: time.Unix(100, 0)}, nil, file, record,
	)
	lease, err := store.OpenRevision(context.Background(), file, generousQuota(t, "reader-session"))
	if err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	revision := store.revisions[file]
	revision.readers = 1
	store.readingRevisions[revision] = struct{}{}
	identity := revision.identity()
	cleanups, invalidateErr := store.invalidateIdentityLocked(identity, revision)
	if invalidateErr != nil || len(cleanups) != 0 {
		store.mu.Unlock()
		t.Fatalf("reader invalidation = (cleanups=%d, %v)", len(cleanups), invalidateErr)
	}
	if err := store.recordInvalidationLocked(identity); err != nil {
		store.mu.Unlock()
		t.Fatalf("idempotent invalidation = %v", err)
	}
	store.revisionAdmissionStopped = true
	otherIdentity := revisionIdentity{file: catalogID[catalog.FileID](0xb7), revision: lease.Descriptor().FileRevision()}
	if err := store.recordInvalidationLocked(otherIdentity); !errors.Is(err, ErrQuotaExceeded) {
		store.mu.Unlock()
		t.Fatalf("invalidation after admission stop = %v", err)
	}
	delete(store.readingRevisions, revision)
	revision.readers = 0
	cleanup, ready := store.invalidateStateLocked(revision, store.clock.Now())
	store.mu.Unlock()
	if !ready {
		t.Fatal("last reader did not release invalidated physical state")
	}
	cleanup.run()
	if stable.closed.Load() != 1 || store.metadataBudget.Snapshot().Used != 1 {
		t.Fatalf("invalidation cleanup = (closes=%d metadata=%+v)",
			stable.closed.Load(), store.metadataBudget.Snapshot())
	}
}
