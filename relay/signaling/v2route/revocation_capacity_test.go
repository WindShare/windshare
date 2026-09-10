package v2route

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestPermanentStopsReleaseCapacityAcrossRepeatedUseAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stopped.bin")
	store := openTestTombstones(t, path)
	now := time.Unix(1_700_000_000, 0)
	const capacity = 1
	const completedShares = 4
	registry := newRegistry(t, &now, store, capacity)
	fixtures := make([]routeFixture, completedShares)
	for index := range fixtures {
		fixtures[index] = makeFixture(t, byte(index+1))
		fixture := fixtures[index]
		publishRoute(t, registry, fixture, routeTestConnection("sender"))
		if err := registry.BeginRegistration(makeFixture(t, 90).init, routeTestConnection("other")); !errors.Is(err, ErrAdmission) {
			t.Fatalf("active capacity not enforced: %v", err)
		}
		if _, err := registry.Stop(context.Background(), fixture.stop, fixture.stopAuth); err != nil {
			t.Fatal(err)
		}
		if len(registry.routes) != 0 {
			t.Fatal("durable STOP retained an active route")
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestTombstones(t, path)
	registry = newRegistry(t, &now, store, capacity)
	publishRoute(t, registry, makeFixture(t, 90), routeTestConnection("fresh"))
	for _, fixture := range fixtures {
		if result, err := registry.Join(fixture.init.ShareID, routeTestConnection("receiver")); err != nil || result.Status != JoinStopped {
			t.Fatalf("restored STOP = %+v, %v", result, err)
		}
		if err := registry.BeginRegistration(fixture.init, routeTestConnection("replacement")); !errors.Is(err, ErrStopped) {
			t.Fatalf("restored STOP registered again: %v", err)
		}
		resume := fixture.init
		resume.Mode = v2.RegistrationResume
		if err := registry.ValidateResumeCredential(resume, fixture.token); !errors.Is(err, ErrStopped) {
			t.Fatalf("restored STOP passed resume precheck: %v", err)
		}
		if err := registry.Resume(resume, resumeAuthority(t, fixture, resume), routeTestConnection("replacement"), fixture.token); !errors.Is(err, ErrStopped) {
			t.Fatalf("restored STOP resumed: %v", err)
		}
		if _, err := registry.Stop(context.Background(), fixture.stop, fixture.stopAuth); err != nil {
			t.Fatalf("restored STOP retry failed: %v", err)
		}
	}
}

func TestRevocationHistoryLargerThanDefaultRouteBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stopped.bin")
	store := openTestTombstones(t, path)
	const historicalStops = 1_025
	records := make([]Tombstone, historicalStops)
	for index := range records {
		records[index] = validFileTombstone(t)
	}
	// Batch fixture setup avoids adding a thousand synchronous disk commits to
	// the local gate while exercising the actual persistent index and reopen.
	if err := store.db.Update(func(tx *bolt.Tx) error {
		for _, record := range records {
			if err := tx.Bucket(tombstoneBucket).Put(record.ShareID[:], encodeTombstoneRecord(record)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestTombstones(t, path)
	now := time.Unix(1_700_000_000, 0)
	registry := newRegistry(t, &now, store, 1)
	publishRoute(t, registry, makeFixture(t, 80), routeTestConnection("fresh"))
	for _, record := range []Tombstone{records[0], records[len(records)-1]} {
		if result, err := registry.Join(record.ShareID, routeTestConnection("receiver")); err != nil || result.Status != JoinStopped {
			t.Fatalf("historical STOP lost: %+v, %v", result, err)
		}
	}
}

func TestPendingAndUncertainStopsRetainCapacityUntilCommit(t *testing.T) {
	for _, outcome := range []CommitOutcome{CommitNotCommitted, CommitUnknown} {
		t.Run(map[CommitOutcome]string{CommitNotCommitted: "definite failure", CommitUnknown: "uncertain"}[outcome], func(t *testing.T) {
			now := time.Unix(1_700_000_000, 0)
			store := newBlockingCommitStore()
			registry := newRegistry(t, &now, store, 1)
			fixture, next := makeFixture(t, 20), makeFixture(t, 21)
			publishRoute(t, registry, fixture, routeTestConnection("sender"))
			assertFull := func() {
				t.Helper()
				if err := registry.BeginRegistration(next.init, routeTestConnection("next")); !errors.Is(err, ErrAdmission) {
					t.Fatalf("unresolved STOP released capacity: %v", err)
				}
			}
			first := startStop(registry, fixture)
			<-store.entered
			assertFull()
			store.replies <- commitReply{outcome: outcome, err: errInjectedTombstone}
			<-first
			assertFull()
			retry := startStop(registry, fixture)
			<-store.entered
			store.replies <- commitReply{outcome: CommitCommitted}
			if result := <-retry; result.err != nil {
				t.Fatal(result.err)
			}
			if err := registry.BeginRegistration(next.init, routeTestConnection("next")); err != nil {
				t.Fatalf("resolved STOP did not release capacity: %v", err)
			}
		})
	}
}

type blockingLookupStore struct {
	memoryTombstones
	target  v2.ShareID
	muOnce  sync.Mutex
	blocked bool
	entered chan struct{}
	release chan struct{}
}

func (store *blockingLookupStore) Lookup(ctx context.Context, shareID v2.ShareID) (Tombstone, bool, error) {
	record, found, err := store.memoryTombstones.Lookup(ctx, shareID)
	store.muOnce.Lock()
	block := shareID == store.target && !store.blocked
	if block {
		store.blocked = true
	}
	store.muOnce.Unlock()
	if block {
		close(store.entered)
		select {
		case <-ctx.Done():
			return Tombstone{}, false, ctx.Err()
		case <-store.release:
		}
	}
	return record, found, err
}

func TestAbsentLookupCannotRaceStopIntoResurrection(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fixture, healthy := makeFixture(t, 30), makeFixture(t, 31)
	store := &blockingLookupStore{
		target: fixture.init.ShareID, entered: make(chan struct{}), release: make(chan struct{}),
	}
	registry := newRegistry(t, &now, store, 2)
	publishRoute(t, registry, healthy, routeTestConnection("healthy"))
	done := make(chan error, 1)
	go func() { done <- registry.BeginRegistration(fixture.init, routeTestConnection("stale")) }()
	<-store.entered
	// An absent lookup deliberately holds no registry lock; unrelated active
	// work and even a complete lifecycle of this same ID must remain possible.
	if result, err := registry.Join(healthy.init.ShareID, routeTestConnection("receiver")); err != nil || result.Status != JoinReady {
		t.Fatalf("slow lookup stalled healthy route: %+v, %v", result, err)
	}
	publishRoute(t, registry, fixture, routeTestConnection("owner"))
	if _, err := registry.Stop(context.Background(), fixture.stop, fixture.stopAuth); err != nil {
		t.Fatal(err)
	}
	close(store.release)
	if err := <-done; !errors.Is(err, ErrStopped) {
		t.Fatalf("stale absence resurrected stopped share: %v", err)
	}
}

func TestRevocationLookupFailureNeverAuthorizesAbsentShare(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	store := &memoryTombstones{lookupErr: errInjectedTombstone}
	registry := newRegistry(t, &now, store, 1)
	fixture := makeFixture(t, 40)
	if err := registry.BeginRegistration(fixture.init, routeTestConnection("sender")); !errors.Is(err, errInjectedTombstone) || !errors.Is(err, ErrAdmission) {
		t.Fatalf("failed lookup admitted registration: %v", err)
	}
	if _, err := registry.Join(fixture.init.ShareID, routeTestConnection("receiver")); !errors.Is(err, ErrAdmission) {
		t.Fatalf("failed lookup reported absence: %v", err)
	}
	if _, err := registry.Stop(context.Background(), fixture.stop, fixture.stopAuth); !errors.Is(err, ErrAdmission) {
		t.Fatalf("failed lookup reported a STOP outcome: %v", err)
	}
	resume := fixture.init
	resume.Mode = v2.RegistrationResume
	if err := registry.ValidateResumeCredential(resume, fixture.token); !errors.Is(err, ErrAdmission) {
		t.Fatalf("failed lookup passed resume precheck: %v", err)
	}
	if err := registry.Resume(resume, resumeAuthority(t, fixture, resume), routeTestConnection("sender"), fixture.token); !errors.Is(err, ErrAdmission) {
		t.Fatalf("failed lookup passed resume: %v", err)
	}
}

func TestUnrelatedStopDoesNotInvalidateAnAbsentLookup(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fresh, stopping := makeFixture(t, 50), makeFixture(t, 51)
	store := &blockingLookupStore{
		target: fresh.init.ShareID, entered: make(chan struct{}), release: make(chan struct{}),
	}
	registry := newRegistry(t, &now, store, 2)
	publishRoute(t, registry, stopping, routeTestConnection("stopping"))
	done := make(chan error, 1)
	go func() { done <- registry.BeginRegistration(fresh.init, routeTestConnection("fresh")) }()
	<-store.entered
	if _, err := registry.Stop(context.Background(), stopping.stop, stopping.stopAuth); err != nil {
		t.Fatal(err)
	}
	// The first read already returned a valid absence. An unrelated STOP
	// must not turn that answer into another disk operation that can fail.
	store.memoryTombstones.mu.Lock()
	store.lookupErr = errInjectedTombstone
	store.memoryTombstones.mu.Unlock()
	close(store.release)
	if err := <-done; err != nil {
		t.Fatalf("unrelated STOP forced another lookup: %v", err)
	}
	if len(registry.revocationLookups) != 0 {
		t.Fatal("completed lookup retained historical state")
	}
}
