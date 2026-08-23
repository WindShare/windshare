package content

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
)

type catalogNodeFunc func(context.Context, catalog.NodeID) (catalog.NodeRecord, bool, error)

func (f catalogNodeFunc) Node(ctx context.Context, id catalog.NodeID) (catalog.NodeRecord, bool, error) {
	return f(ctx, id)
}

type revisionSourceFunc func(context.Context, catalog.NodeRecord) (StableFile, error)

func (f revisionSourceFunc) OpenStable(ctx context.Context, record catalog.NodeRecord) (StableFile, error) {
	return f(ctx, record)
}

type fixedIDs struct {
	lease LeaseID
}

func (g fixedIDs) NewLeaseID() (LeaseID, error) { return g.lease, nil }

func customRevisionStore(t *testing.T, nodeSource CatalogNodeSource, source RevisionSource, clock Clock, ids LeaseIDGenerator) (*RevisionStore, *capacityTestAccount, *capacityTestAccount) {
	t.Helper()
	owner, err := revisioncapacity.NewProcessOwner(revisioncapacity.ProcessConfig{
		Limits:     revisioncapacity.CapacityLimits{StableHandles: 100, ActiveLeases: 100},
		RetryAfter: revisioncapacity.DefaultCapacityRetryAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	deriver := testRevisionDeriver(t)
	store, err := NewRevisionStore(RevisionStoreConfig{
		ShareInstance: catalogID[catalog.ShareInstance](1), ChunkSize: catalog.MinChunkSize,
		Catalog: nodeSource, Source: source, Clock: clock, LeaseIDs: ids,
		CapacityCoordinator: owner.Coordinator(),
		CapacityStore: revisioncapacity.StoreConfig{
			StoreID: "custom-test-store", ShareID: "custom-test-share",
			Limits: revisioncapacity.CapacityLimits{StableHandles: 100, ActiveLeases: 100},
		},
		RevisionDeriver: deriver, MetadataBudget: testRevisionMetadataBudget(t, DefaultRevisionInvalidationEntries),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		_ = owner.Close()
		deriver.Destroy()
	})
	process := &capacityTestAccount{name: "process", snapshot: func() revisioncapacity.ScopeSnapshot { return store.CapacitySnapshot().Process() }}
	share := &capacityTestAccount{name: "share", snapshot: func() revisioncapacity.ScopeSnapshot { return store.CapacitySnapshot().Share() }}
	return store, process, share
}

func TestRevisionStoreRejectsInvalidConfigurationAndInputs(t *testing.T) {
	if _, err := NewRevisionStore(RevisionStoreConfig{}); err == nil {
		t.Fatal("empty revision store configuration was accepted")
	}
	file, record := fileRecord(t, 1)
	store, _, _ := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, &testRevisionSource{files: []*testStableFile{{data: []byte{1}}}}, nil, &sequenceIDs{})
	session := generousSession(t, store, "session")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.OpenRevision(cancelled, file, session); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled open = %v", err)
	}
	if _, err := store.OpenRevision(context.Background(), catalog.FileID{}, session); err == nil {
		t.Fatal("zero file open was accepted")
	}
	if _, err := store.OpenRevision(context.Background(), file, nil); err == nil {
		t.Fatal("nil session capacity registration was accepted")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenRevision(context.Background(), file, session); !errors.Is(err, ErrRevisionStoreClosed) {
		t.Fatalf("closed open = %v", err)
	}
}

func TestNewRevisionRequiresSessionAdmissionBeforeOpeningSource(t *testing.T) {
	firstFile, firstRecord := fileRecord(t, 1)
	secondFile := catalogID[catalog.FileID](10)
	parent := catalogID[catalog.DirectoryID](8)
	locator, _ := catalog.NewLocator(0, "second")
	identity, _ := catalog.NewSourceIdentity([]byte("second-identity"))
	candidate, _ := catalog.NewVersionCandidate([]byte("second-candidate"))
	secondRecord, err := catalog.NewFileNodeRecord(secondFile, parent, "second", locator, identity, candidate, 1, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		limits   revisioncapacity.CapacityLimits
		resource revisioncapacity.CapacityResource
	}{
		{name: "stable handle", limits: revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 2}, resource: revisioncapacity.CapacityResourceStableHandle},
		{name: "active lease", limits: revisioncapacity.CapacityLimits{StableHandles: 2, ActiveLeases: 1}, resource: revisioncapacity.CapacityResourceActiveLease},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &testRevisionSource{files: []*testStableFile{{data: []byte{1}}, {data: []byte{2}}}}
			store, process, share := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{
				firstFile.NodeID(): firstRecord, secondFile.NodeID(): secondRecord,
			}}, source, nil, &sequenceIDs{})
			session := limitedSession(t, store, "limited-session", test.limits)
			first, err := store.OpenRevision(context.Background(), firstFile, session)
			if err != nil {
				t.Fatal(err)
			}
			var busy *revisioncapacity.CapacityBusyError
			if _, err := store.OpenRevision(context.Background(), secondFile, session); !errors.As(err, &busy) {
				t.Fatalf("second open admission = %v", err)
			}
			if busy.Resource() != test.resource || busy.Scope() != revisioncapacity.CapacityScopeSession ||
				busy.DecisionID() == "" || busy.Snapshot().Process().Used().ActiveLeases != 1 {
				t.Fatalf("imprecise capacity busy outcome=%+v", busy)
			}
			if source.Calls() != 1 {
				t.Fatalf("rejected session admission opened %d stable sources", source.Calls())
			}
			if got := session.Snapshot().Used(); got != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) {
				t.Fatalf("failed pre-admission changed session usage: %+v", got)
			}
			if err := store.EndLease(first.ID(), LeaseRelinquished); err != nil {
				t.Fatal(err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) || session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
				t.Fatal("pre-admission test leaked quota")
			}
		})
	}
}

func TestOpenRevisionRollsBackEveryPrepublicationFailure(t *testing.T) {
	file, record := fileRecord(t, 1)
	tests := []struct {
		name    string
		catalog CatalogNodeSource
		source  RevisionSource
		ids     LeaseIDGenerator
	}{
		{
			name: "catalog error",
			catalog: catalogNodeFunc(func(context.Context, catalog.NodeID) (catalog.NodeRecord, bool, error) {
				return catalog.NodeRecord{}, false, errors.New("catalog unavailable")
			}),
			source: &testRevisionSource{}, ids: &sequenceIDs{},
		},
		{
			name: "missing catalog node", catalog: testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{}},
			source: &testRevisionSource{}, ids: &sequenceIDs{},
		},
		{
			name: "source failure", catalog: testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
			source: &testRevisionSource{}, ids: &sequenceIDs{},
		},
		{
			name: "initial verification failure", catalog: testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
			source: &testRevisionSource{files: []*testStableFile{{data: []byte{1}, drifted: atomic.Bool{}}}}, ids: &sequenceIDs{},
		},
		{
			name: "size mismatch", catalog: testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
			source: &testRevisionSource{files: []*testStableFile{{data: []byte{1, 2}}}}, ids: &sequenceIDs{},
		},
		{
			name: "source panic", catalog: testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
			source: revisionSourceFunc(func(context.Context, catalog.NodeRecord) (StableFile, error) { panic("boom") }), ids: &sequenceIDs{},
		},
	}
	// The verification case needs an explicitly drifted source after construction.
	tests[3].source.(*testRevisionSource).files[0].drifted.Store(true)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, process, share := customRevisionStore(t, test.catalog, test.source, nil, test.ids)
			session := generousSession(t, store, "session")
			_, openErr := store.OpenRevision(context.Background(), file, session)
			if openErr == nil {
				t.Fatal("prepublication failure was accepted")
			}
			if test.name == "initial verification failure" && !errors.Is(openErr, ErrRevisionStale) {
				t.Fatalf("initial candidate drift = %v", openErr)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) || session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
				t.Fatalf("prepublication failure leaked quota: process=%+v share=%+v session=%+v", process.Snapshot().Used, share.Snapshot().Used, session.Snapshot().Used())
			}
		})
	}
}

func TestOpenRevisionCancellationCancelsUnpublishedStableOpen(t *testing.T) {
	file, record := fileRecord(t, 1)
	source := &testRevisionSource{files: []*testStableFile{{data: []byte{1}}}, started: make(chan struct{}), release: make(chan struct{})}
	store, process, share := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, source, nil, &sequenceIDs{})
	session := generousSession(t, store, "session")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := store.OpenRevision(ctx, file, session)
		result <- err
	}()
	<-source.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled open = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) || session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
		t.Fatal("cancelled open leaked quota")
	}
}

func TestStoreCloseReleasesPendingOpenAdmissionBeforeWaiterReturns(t *testing.T) {
	file, record := fileRecord(t, 1)
	source := &testRevisionSource{files: []*testStableFile{{data: []byte{1}}}, started: make(chan struct{}), release: make(chan struct{})}
	store, process, share := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, source, nil, &sequenceIDs{})
	session := generousSession(t, store, "session")
	result := make(chan error, 1)
	go func() {
		_, err := store.OpenRevision(context.Background(), file, session)
		result <- err
	}()
	<-source.started
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) || session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
		t.Fatal("store close returned while pending open admission remained charged")
	}
	if err := <-result; err == nil {
		t.Fatal("open completed after its store closed")
	}
}

func TestLeaseErrorsDoNotEvictAnAdmittedRevision(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store, _, _ := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, &testRevisionSource{files: []*testStableFile{stable}}, clock, &sequenceIDs{})
	session := generousSession(t, store, "session")
	lease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RenewLease(contentID[LeaseID](99)); !errors.Is(err, ErrInvalidLease) {
		t.Fatalf("unknown renew = %v", err)
	}
	store.mu.Lock()
	state := store.leases[lease.ID()]
	state.createdAt = clock.Now().Add(-MaxLeaseLifetime)
	state.expiresAt = clock.Now().Add(time.Second)
	store.mu.Unlock()
	if _, err := store.RenewLease(lease.ID()); !errors.Is(err, ErrLeaseLifetime) {
		t.Fatalf("maximum lifetime renew = %v", err)
	}
	store.mu.Lock()
	state.createdAt = clock.Now().Add(-MaxLeaseLifetime + LeaseTTL - time.Millisecond)
	state.expiresAt = clock.Now().Add(LeaseRenewWindow)
	store.mu.Unlock()
	if _, err := store.RenewLease(lease.ID()); !errors.Is(err, ErrLeaseLifetime) {
		t.Fatalf("truncated final renew = %v", err)
	}
	wrongRef := BlockRef{fileID: catalogID[catalog.FileID](88), fileRevision: lease.Descriptor().FileRevision(), localBlockIndex: 0}
	if _, err := store.ReadBlock(context.Background(), lease.ID(), wrongRef); !errors.Is(err, ErrInvalidBlockRef) {
		t.Fatalf("wrong-axis block = %v", err)
	}
	if _, err := store.ReadBlock(context.Background(), contentID[LeaseID](99), wrongRef); !errors.Is(err, ErrInvalidLease) {
		t.Fatalf("unknown lease read = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ReadBlock(cancelled, lease.ID(), wrongRef); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if stable.closed.Load() != 1 || session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
		t.Fatalf("close cleanup: source=%d quota=%+v", stable.closed.Load(), session.Snapshot().Used())
	}
	if _, err := store.RenewLease(lease.ID()); !errors.Is(err, ErrRevisionStoreClosed) {
		t.Fatalf("closed renew = %v", err)
	}
	if _, err := store.ReadBlock(context.Background(), lease.ID(), wrongRef); !errors.Is(err, ErrRevisionStoreClosed) {
		t.Fatalf("closed read = %v", err)
	}
}

func TestOpenRevisionRejectsLeaseIdentityReuseWithoutClosingRevision(t *testing.T) {
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	ids := fixedIDs{lease: contentID[LeaseID](2)}
	store, _, _ := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, &testRevisionSource{files: []*testStableFile{stable}}, nil, ids)
	session := generousSession(t, store, "session")
	first, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenRevision(context.Background(), file, session); err == nil {
		t.Fatal("reused lease identity was accepted")
	}
	ref, _ := NewBlockRef(file, first.Descriptor().FileRevision(), 0, first.Descriptor().Geometry())
	if _, err := store.ReadBlock(context.Background(), first.ID(), ref); err != nil {
		t.Fatalf("failed second admission evicted active revision: %v", err)
	}
	if err := store.EndLease(first.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenRevision(context.Background(), file, session); err == nil {
		t.Fatal("recently released lease identity was reused")
	}
	_ = store.Close()
}

func TestExpiredLeaseGraceUsesActualExpiryAndReusesStableRevision(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, 1)
	firstSource := &testStableFile{data: []byte{1}}
	secondSource := &testStableFile{data: []byte{1}}
	source := &testRevisionSource{files: []*testStableFile{firstSource, secondSource}}
	store, _, _ := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, source, clock, &sequenceIDs{})
	defer store.Close()
	session := generousSession(t, store, "session")
	first, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(LeaseTTL + RevisionResumeGrace + time.Second)
	second, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if first.Descriptor().FileRevision() != second.Descriptor().FileRevision() || first.ID() == second.ID() || source.Calls() != 2 || firstSource.closed.Load() != 1 {
		t.Fatalf("stable revision reopen: first=%x second=%x calls=%d closes=%d", first.Descriptor().FileRevision(), second.Descriptor().FileRevision(), source.Calls(), firstSource.closed.Load())
	}
	if _, err := store.RenewLease(first.ID()); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expired lease tombstone = %v", err)
	}
}

func TestLateRelinquishmentCannotRestartDetachedRecoveryAfterExpiry(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store, process, share := customRevisionStore(t,
		testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		&testRevisionSource{files: []*testStableFile{stable}}, clock, &sequenceIDs{})
	defer store.Close()
	session := generousSession(t, store, "session")
	lease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(LeaseTTL + RevisionResumeGrace + time.Second)
	if err := store.EndLease(lease.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	if stable.closed.Load() != 1 {
		t.Fatal("late relinquishment retained the source for a new recovery interval")
	}
	if process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) || session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
		t.Fatal("late relinquishment retained expired revision quota")
	}
}

func TestDefaultLeaseIdentityIsNonzeroAndDerivedRevisionSurvivesReopen(t *testing.T) {
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store, _, _ := customRevisionStore(t,
		testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		&testRevisionSource{files: []*testStableFile{stable}}, nil, nil,
	)
	session := generousSession(t, store, "session")
	lease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if lease.ID().IsZero() || lease.Descriptor().FileRevision().IsZero() {
		t.Fatal("default identity generator returned zero")
	}
	_ = store.EndLease(lease.ID(), LeaseRelinquished)
	_ = session.Close()
	_ = store.Close()

	clock := &testClock{now: time.Unix(100, 0)}
	firstStable := &testStableFile{data: []byte{1}}
	secondStable := &testStableFile{data: []byte{1}}
	reuseStore, _, _ := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, &testRevisionSource{files: []*testStableFile{firstStable, secondStable}}, clock, &sequenceIDs{})
	reuseSession := generousSession(t, reuseStore, "reuse-session")
	first, err := reuseStore.OpenRevision(context.Background(), file, reuseSession)
	if err != nil {
		t.Fatal(err)
	}
	_ = reuseStore.EndLease(first.ID(), LeaseRelinquished)
	second, err := reuseStore.OpenRevision(context.Background(), file, reuseSession)
	if err != nil || first.Descriptor().FileRevision() != second.Descriptor().FileRevision() || first.ID() == second.ID() {
		t.Fatalf("derived revision did not survive clean release: second=%+v err=%v", second, err)
	}
	_ = reuseStore.Close()
}

type panicAfterPublicationFile struct {
	verified atomic.Int32
	closed   atomic.Int32
}

type panicCloseFile struct{ closed atomic.Int32 }

func (*panicCloseFile) ExactSize() uint64                                   { return 2 }
func (*panicCloseFile) ModifiedTime() catalog.ModifiedTime                  { return catalog.ModifiedTime{} }
func (*panicCloseFile) Verify(context.Context) error                        { return nil }
func (*panicCloseFile) ReadAt(context.Context, []byte, uint64) (int, error) { return 0, nil }
func (f *panicCloseFile) Close() error {
	f.closed.Add(1)
	panic("close panic")
}

func TestFailedOpenQuarantinesCapacityWhenStableClosePanics(t *testing.T) {
	file, record := fileRecord(t, 1)
	stable := &panicCloseFile{}
	store, process, share := customRevisionStore(t,
		testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		revisionSourceFunc(func(context.Context, catalog.NodeRecord) (StableFile, error) { return stable, nil }),
		nil, &sequenceIDs{})
	session := generousSession(t, store, "session")
	if _, err := store.OpenRevision(context.Background(), file, session); !errors.Is(err, ErrRevisionStale) {
		t.Fatalf("size-mismatched stable source = %v", err)
	}
	if stable.closed.Load() != 1 || process.Snapshot().Used != (QuotaUsage{StableHandles: 1}) || share.Snapshot().Used != (QuotaUsage{StableHandles: 1}) ||
		session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) || store.CapacitySnapshot().Process().QuarantinedStableHandles() != 1 {
		t.Fatal("panicking cleanup did not quarantine uncertain stable ownership")
	}
	_ = store.Close()
}

type cancelledReadFile struct{}

func (cancelledReadFile) ExactSize() uint64                  { return 1 }
func (cancelledReadFile) ModifiedTime() catalog.ModifiedTime { return catalog.ModifiedTime{} }
func (cancelledReadFile) Verify(context.Context) error       { return nil }
func (cancelledReadFile) ReadAt(context.Context, []byte, uint64) (int, error) {
	return 0, context.Canceled
}
func (cancelledReadFile) Close() error { return nil }

func (*panicAfterPublicationFile) ExactSize() uint64                  { return 1 }
func (*panicAfterPublicationFile) ModifiedTime() catalog.ModifiedTime { return catalog.ModifiedTime{} }
func (f *panicAfterPublicationFile) Verify(context.Context) error {
	if f.verified.Add(1) > 1 {
		panic("verification panic")
	}
	return nil
}
func (*panicAfterPublicationFile) ReadAt(context.Context, []byte, uint64) (int, error) { return 1, nil }
func (f *panicAfterPublicationFile) Close() error                                      { f.closed.Add(1); return nil }

func TestStableReadPanicIsUnavailableAndDoesNotInvalidateRevision(t *testing.T) {
	file, record := fileRecord(t, 1)
	stable := &panicAfterPublicationFile{}
	store, process, share := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, revisionSourceFunc(func(context.Context, catalog.NodeRecord) (StableFile, error) {
		return stable, nil
	}), nil, &sequenceIDs{})
	session := generousSession(t, store, "session")
	lease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := NewBlockRef(file, lease.Descriptor().FileRevision(), 0, lease.Descriptor().Geometry())
	if _, err := store.ReadBlock(context.Background(), lease.ID(), ref); err == nil || errors.Is(err, ErrRevisionDrift) {
		t.Fatalf("panicking stable read = %v", err)
	}
	if stable.closed.Load() != 0 || process.Snapshot().Used == (QuotaUsage{}) || share.Snapshot().Used == (QuotaUsage{}) || session.Snapshot().Used() == (revisioncapacity.CapacityUsage{}) {
		t.Fatal("uncertain read invalidated an otherwise active revision")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if stable.closed.Load() != 1 || process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) || session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
		t.Fatal("store close leaked source or quota after uncertain read")
	}
}

func TestCancelledStableReadDoesNotDriftSharedRevision(t *testing.T) {
	comparison, readErr := readStableBlock(context.Background(), cancelledReadFile{}, make([]byte, 1), 0)
	if !errors.Is(readErr, context.Canceled) || comparison != RevisionComparisonUnavailable {
		t.Fatalf("cancelled read = %v, comparison=%v", readErr, comparison)
	}
}

func TestLeaseIdentityTombstoneRingRemainsBounded(t *testing.T) {
	store := &RevisionStore{
		leaseTombstones: make(map[LeaseID]leaseStatus),
	}
	for index := 0; index <= IdentityTombstoneLimit; index++ {
		var lease LeaseID
		lease[0] = byte(index >> 8)
		lease[1] = byte(index)
		store.rememberLeaseTombstoneLocked(lease, leaseEnded)
	}
	if len(store.leaseTombstones) != IdentityTombstoneLimit || len(store.leaseOrder) != IdentityTombstoneLimit {
		t.Fatalf("lease identity tombstones are unbounded: leases=%d", len(store.leaseTombstones))
	}
}
