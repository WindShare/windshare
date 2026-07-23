package content

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
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
	revision FileRevision
	lease    LeaseID
}

func (g fixedIDs) NewFileRevision() (FileRevision, error) { return g.revision, nil }
func (g fixedIDs) NewLeaseID() (LeaseID, error)           { return g.lease, nil }

func customRevisionStore(t *testing.T, nodeSource CatalogNodeSource, source RevisionSource, clock Clock, ids IdentityGenerator) (*RevisionStore, *QuotaAccount, *QuotaAccount) {
	t.Helper()
	process := generousQuota(t, "process")
	share := generousQuota(t, "share")
	store, err := NewRevisionStore(RevisionStoreConfig{
		ShareInstance: catalogID[catalog.ShareInstance](1), ChunkSize: catalog.MinChunkSize,
		Catalog: nodeSource, Source: source, ProcessQuota: process, ShareQuota: share, Clock: clock, IDs: ids,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, process, share
}

func TestRevisionStoreRejectsInvalidConfigurationAndInputs(t *testing.T) {
	if _, err := NewRevisionStore(RevisionStoreConfig{}); err == nil {
		t.Fatal("empty revision store configuration was accepted")
	}
	quota := generousQuota(t, "quota")
	if _, err := NewRevisionStore(RevisionStoreConfig{
		ShareInstance: catalogID[catalog.ShareInstance](1), ChunkSize: catalog.MinChunkSize,
		Catalog: testCatalog{}, Source: &testRevisionSource{}, ProcessQuota: quota, ShareQuota: quota,
	}); err == nil {
		t.Fatal("aliased process/share quota was accepted")
	}
	if _, err := NewRevisionStore(RevisionStoreConfig{
		ShareInstance: catalogID[catalog.ShareInstance](1), ChunkSize: 1000,
		Catalog: testCatalog{}, Source: &testRevisionSource{}, ProcessQuota: generousQuota(t, "process"), ShareQuota: generousQuota(t, "share"),
	}); err == nil {
		t.Fatal("invalid chunk geometry was accepted")
	}
	file, record := fileRecord(t, 1)
	store, _, _ := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, &testRevisionSource{files: []*testStableFile{{data: []byte{1}}}}, nil, &sequenceIDs{})
	session := generousQuota(t, "session")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.OpenRevision(cancelled, file, session); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled open = %v", err)
	}
	if _, err := store.OpenRevision(context.Background(), catalog.FileID{}, session); err == nil {
		t.Fatal("zero file open was accepted")
	}
	if _, err := store.OpenRevision(context.Background(), file, nil); err == nil {
		t.Fatal("nil session quota was accepted")
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
		name   string
		limits QuotaLimits
	}{
		{name: "stable handle", limits: QuotaLimits{StableHandles: 1, ActiveLeases: 2}},
		{name: "active lease", limits: QuotaLimits{StableHandles: 2, ActiveLeases: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &testRevisionSource{files: []*testStableFile{{data: []byte{1}}, {data: []byte{2}}}}
			store, process, share := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{
				firstFile.NodeID(): firstRecord, secondFile.NodeID(): secondRecord,
			}}, source, nil, &sequenceIDs{})
			session, err := NewQuotaAccount("limited-session", test.limits)
			if err != nil {
				t.Fatal(err)
			}
			first, err := store.OpenRevision(context.Background(), firstFile, session)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.OpenRevision(context.Background(), secondFile, session); !errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("second open admission = %v", err)
			}
			if source.Calls() != 1 {
				t.Fatalf("rejected session admission opened %d stable sources", source.Calls())
			}
			if got := session.Snapshot().Used; got != (QuotaUsage{StableHandles: 1, ActiveLeases: 1}) {
				t.Fatalf("failed pre-admission changed session usage: %+v", got)
			}
			if err := store.ReleaseLease(first.ID()); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) || session.Snapshot().Used != (QuotaUsage{}) {
				t.Fatal("pre-admission test leaked quota")
			}
		})
	}
}

func TestOpenRevisionRollsBackEveryPrepublicationFailure(t *testing.T) {
	file, record := fileRecord(t, 1)
	session := generousQuota(t, "session")
	tests := []struct {
		name    string
		catalog CatalogNodeSource
		source  RevisionSource
		ids     IdentityGenerator
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
			name: "zero revision identity", catalog: testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
			source: &testRevisionSource{files: []*testStableFile{{data: []byte{1}}}}, ids: fixedIDs{lease: contentID[LeaseID](1)},
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
			_, openErr := store.OpenRevision(context.Background(), file, session)
			if openErr == nil {
				t.Fatal("prepublication failure was accepted")
			}
			if test.name == "initial verification failure" && !errors.Is(openErr, ErrRevisionStale) {
				t.Fatalf("initial candidate drift = %v", openErr)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			if process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) || session.Snapshot().Used != (QuotaUsage{}) {
				t.Fatalf("prepublication failure leaked quota: process=%+v share=%+v session=%+v", process.Snapshot().Used, share.Snapshot().Used, session.Snapshot().Used)
			}
		})
	}
}

func TestOpenRevisionCancellationCancelsUnpublishedStableOpen(t *testing.T) {
	file, record := fileRecord(t, 1)
	source := &testRevisionSource{files: []*testStableFile{{data: []byte{1}}}, started: make(chan struct{}), release: make(chan struct{})}
	store, process, share := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, source, nil, &sequenceIDs{})
	session := generousQuota(t, "session")
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
	if process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) || session.Snapshot().Used != (QuotaUsage{}) {
		t.Fatal("cancelled open leaked quota")
	}
}

func TestStoreCloseReleasesPendingOpenAdmissionBeforeWaiterReturns(t *testing.T) {
	file, record := fileRecord(t, 1)
	source := &testRevisionSource{files: []*testStableFile{{data: []byte{1}}}, started: make(chan struct{}), release: make(chan struct{})}
	store, process, share := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, source, nil, &sequenceIDs{})
	session := generousQuota(t, "session")
	result := make(chan error, 1)
	go func() {
		_, err := store.OpenRevision(context.Background(), file, session)
		result <- err
	}()
	<-source.started
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) || session.Snapshot().Used != (QuotaUsage{}) {
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
	session := generousQuota(t, "session")
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
	if stable.closed.Load() != 1 || session.Snapshot().Used != (QuotaUsage{}) {
		t.Fatalf("close cleanup: source=%d quota=%+v", stable.closed.Load(), session.Snapshot().Used)
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
	ids := fixedIDs{revision: contentID[FileRevision](1), lease: contentID[LeaseID](2)}
	store, _, _ := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, &testRevisionSource{files: []*testStableFile{stable}}, nil, ids)
	session := generousQuota(t, "session")
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
	if err := store.ReleaseLease(first.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenRevision(context.Background(), file, session); err == nil {
		t.Fatal("recently released lease identity was reused")
	}
	_ = store.Close()
}

func TestExpiredLeaseGraceUsesActualExpiryAndForcesFreshRevision(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, 1)
	firstSource := &testStableFile{data: []byte{1}}
	secondSource := &testStableFile{data: []byte{2}}
	source := &testRevisionSource{files: []*testStableFile{firstSource, secondSource}}
	store, _, _ := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, source, clock, &sequenceIDs{})
	defer store.Close()
	session := generousQuota(t, "session")
	first, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(LeaseTTL + RevisionResumeGrace + time.Second)
	second, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if first.Descriptor().FileRevision() == second.Descriptor().FileRevision() || source.Calls() != 2 || firstSource.closed.Load() != 1 {
		t.Fatalf("expired revision reuse: first=%x second=%x calls=%d closes=%d", first.Descriptor().FileRevision(), second.Descriptor().FileRevision(), source.Calls(), firstSource.closed.Load())
	}
	if _, err := store.RenewLease(first.ID()); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expired lease tombstone = %v", err)
	}
}

func TestDelayedReleaseCannotRestartGraceAfterLeaseExpiry(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store, process, share := customRevisionStore(t,
		testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		&testRevisionSource{files: []*testStableFile{stable}}, clock, &sequenceIDs{})
	defer store.Close()
	session := generousQuota(t, "session")
	lease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(LeaseTTL + RevisionResumeGrace + time.Second)
	if err := store.ReleaseLease(lease.ID()); err != nil {
		t.Fatal(err)
	}
	if stable.closed.Load() != 1 {
		t.Fatal("late release retained the source for a new grace interval")
	}
	if process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) || session.Snapshot().Used != (QuotaUsage{}) {
		t.Fatal("late release retained expired revision quota")
	}
}

func TestGeneratedIdentitiesAreNonzeroAndNeverReusedAfterGrace(t *testing.T) {
	file, record := fileRecord(t, 1)
	process := generousQuota(t, "process")
	share := generousQuota(t, "share")
	stable := &testStableFile{data: []byte{1}}
	store, err := NewRevisionStore(RevisionStoreConfig{
		ShareInstance: catalogID[catalog.ShareInstance](1), ChunkSize: catalog.MinChunkSize,
		Catalog: testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		Source:  &testRevisionSource{files: []*testStableFile{stable}}, ProcessQuota: process, ShareQuota: share,
	})
	if err != nil {
		t.Fatal(err)
	}
	session := generousQuota(t, "session")
	lease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if lease.ID().IsZero() || lease.Descriptor().FileRevision().IsZero() {
		t.Fatal("default identity generator returned zero")
	}
	_ = store.Close()

	clock := &testClock{now: time.Unix(100, 0)}
	firstStable := &testStableFile{data: []byte{1}}
	secondStable := &testStableFile{data: []byte{1}}
	fixed := fixedIDs{revision: contentID[FileRevision](7), lease: contentID[LeaseID](8)}
	reuseStore, _, _ := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, &testRevisionSource{files: []*testStableFile{firstStable, secondStable}}, clock, fixed)
	first, err := reuseStore.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	_ = reuseStore.ReleaseLease(first.ID())
	clock.Advance(RevisionResumeGrace)
	if _, err := reuseStore.OpenRevision(context.Background(), file, session); err == nil {
		t.Fatal("reused file revision identity was accepted")
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

func TestFailedOpenReleasesQuotaWhenStableClosePanics(t *testing.T) {
	file, record := fileRecord(t, 1)
	stable := &panicCloseFile{}
	store, process, share := customRevisionStore(t,
		testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		revisionSourceFunc(func(context.Context, catalog.NodeRecord) (StableFile, error) { return stable, nil }),
		nil, &sequenceIDs{})
	session := generousQuota(t, "session")
	if _, err := store.OpenRevision(context.Background(), file, session); !errors.Is(err, ErrRevisionStale) {
		t.Fatalf("size-mismatched stable source = %v", err)
	}
	if stable.closed.Load() != 1 || process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) || session.Snapshot().Used != (QuotaUsage{}) {
		t.Fatal("panicking cleanup leaked stable-handle admission")
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

func TestStableReadPanicDriftsRevisionWithoutLeakingResources(t *testing.T) {
	file, record := fileRecord(t, 1)
	stable := &panicAfterPublicationFile{}
	store, process, share := customRevisionStore(t, testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}}, revisionSourceFunc(func(context.Context, catalog.NodeRecord) (StableFile, error) {
		return stable, nil
	}), nil, &sequenceIDs{})
	session := generousQuota(t, "session")
	lease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := NewBlockRef(file, lease.Descriptor().FileRevision(), 0, lease.Descriptor().Geometry())
	if _, err := store.ReadBlock(context.Background(), lease.ID(), ref); !errors.Is(err, ErrRevisionDrift) {
		t.Fatalf("panicking stable read = %v", err)
	}
	if stable.closed.Load() != 1 || process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) || session.Snapshot().Used != (QuotaUsage{}) {
		t.Fatal("panicking stable read leaked source or quota")
	}
}

func TestCancelledStableReadDoesNotDriftSharedRevision(t *testing.T) {
	readErr, drift := readStableBlock(context.Background(), cancelledReadFile{}, make([]byte, 1), 0)
	if !errors.Is(readErr, context.Canceled) || drift {
		t.Fatalf("cancelled read = %v, drift=%v", readErr, drift)
	}
}

func TestIdentityTombstoneRingsRemainBounded(t *testing.T) {
	store := &RevisionStore{
		usedRevisions:   make(map[FileRevision]struct{}),
		leaseTombstones: make(map[LeaseID]leaseStatus),
	}
	for index := 0; index <= IdentityTombstoneLimit; index++ {
		var lease LeaseID
		lease[0] = byte(index >> 8)
		lease[1] = byte(index)
		store.rememberLeaseTombstoneLocked(lease, leaseExpired)
		var revision FileRevision
		revision[0] = byte(index >> 8)
		revision[1] = byte(index)
		store.rememberRevisionIDLocked(revision)
	}
	if len(store.leaseTombstones) != IdentityTombstoneLimit || len(store.usedRevisions) != IdentityTombstoneLimit ||
		len(store.leaseOrder) != IdentityTombstoneLimit || len(store.revisionOrder) != IdentityTombstoneLimit {
		t.Fatalf("identity tombstones are unbounded: leases=%d revisions=%d", len(store.leaseTombstones), len(store.usedRevisions))
	}
}
