package content

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

type testCatalog struct {
	records map[catalog.NodeID]catalog.NodeRecord
}

func (c testCatalog) Node(_ context.Context, id catalog.NodeID) (catalog.NodeRecord, bool, error) {
	record, exists := c.records[id]
	return record, exists, nil
}

type testStableFile struct {
	data     []byte
	drifted  atomic.Bool
	closed   atomic.Int32
	modified catalog.ModifiedTime
}

func (f *testStableFile) ExactSize() uint64                  { return uint64(len(f.data)) }
func (f *testStableFile) ModifiedTime() catalog.ModifiedTime { return f.modified }
func (f *testStableFile) Verify(context.Context) error {
	if f.drifted.Load() {
		return ErrSourceDrift
	}
	return nil
}
func (f *testStableFile) ReadAt(_ context.Context, destination []byte, offset uint64) (int, error) {
	if offset >= uint64(len(f.data)) {
		return 0, io.EOF
	}
	count := copy(destination, f.data[offset:])
	if count != len(destination) {
		return count, io.EOF
	}
	return count, nil
}
func (f *testStableFile) Close() error { f.closed.Add(1); return nil }

type testRevisionSource struct {
	mu      sync.Mutex
	files   []*testStableFile
	calls   int
	started chan struct{}
	release chan struct{}
}

func (s *testRevisionSource) OpenStable(ctx context.Context, _ catalog.NodeRecord) (StableFile, error) {
	s.mu.Lock()
	index := s.calls
	s.calls++
	var file *testStableFile
	if index < len(s.files) {
		file = s.files[index]
	}
	started, release := s.started, s.release
	s.mu.Unlock()
	if started != nil && index == 0 {
		close(started)
	}
	if release != nil && index == 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
	}
	if file == nil {
		return nil, errors.New("no test stable file")
	}
	return file, nil
}

func (s *testRevisionSource) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type sequenceIDs struct {
	mu   sync.Mutex
	next byte
}

func (g *sequenceIDs) nextValue() byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	return g.next
}

func (g *sequenceIDs) NewLeaseID() (LeaseID, error) {
	return contentID[LeaseID](g.nextValue()), nil
}

func testRevisionDeriver(t *testing.T) *HMACRevisionIdentityDeriver {
	t.Helper()
	var key RevisionIdentityKey
	key[0] = 1
	deriver, err := NewHMACRevisionIdentityDeriver(key)
	if err != nil {
		t.Fatal(err)
	}
	return deriver
}

func testRevisionMetadataBudget(t *testing.T, capacity uint64) *RevisionMetadataBudget {
	t.Helper()
	budget, err := NewRevisionMetadataBudget(capacity)
	if err != nil {
		t.Fatal(err)
	}
	return budget
}

type recordingInvalidator struct {
	mu        sync.Mutex
	revisions []FileRevision
}

func (i *recordingInvalidator) InvalidateRevision(_ catalog.FileID, revision FileRevision) {
	i.mu.Lock()
	i.revisions = append(i.revisions, revision)
	i.mu.Unlock()
}

func generousQuota(t *testing.T, name string) *QuotaAccount {
	t.Helper()
	account, err := NewQuotaAccount(name, QuotaLimits{StableHandles: 100, ActiveLeases: 100})
	if err != nil {
		t.Fatal(err)
	}
	return account
}

type capacityTestAccount struct {
	name     string
	snapshot func() revisioncapacity.ScopeSnapshot
}

func (a *capacityTestAccount) Snapshot() QuotaSnapshot {
	snapshot := a.snapshot()
	limits := snapshot.Limits()
	used := snapshot.Used()
	return QuotaSnapshot{
		Name:   a.name,
		Limits: QuotaLimits{StableHandles: limits.StableHandles, ActiveLeases: limits.ActiveLeases},
		Used:   QuotaUsage{StableHandles: used.StableHandles, ActiveLeases: used.ActiveLeases},
	}
}

func generousSession(t *testing.T, store *RevisionStore, name string) *revisioncapacity.SessionRegistration {
	t.Helper()
	return limitedSession(t, store, name, revisioncapacity.CapacityLimits{StableHandles: 100, ActiveLeases: 100})
}

func limitedSession(
	t *testing.T,
	store *RevisionStore,
	name string,
	limits revisioncapacity.CapacityLimits,
) *revisioncapacity.SessionRegistration {
	t.Helper()
	session, err := store.RegisterSession(revisioncapacity.SessionConfig{
		SessionID: revisioncapacity.SessionID(name), Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func attachTestCapacity(
	t *testing.T,
	config *RevisionStoreConfig,
	processLimits revisioncapacity.CapacityLimits,
	shareLimits revisioncapacity.CapacityLimits,
) *revisioncapacity.ProcessOwner {
	t.Helper()
	owner, err := revisioncapacity.NewProcessOwner(revisioncapacity.ProcessConfig{
		Limits: processLimits, RetryAfter: revisioncapacity.DefaultCapacityRetryAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	config.CapacityCoordinator = owner.Coordinator()
	config.CapacityStore = revisioncapacity.StoreConfig{
		StoreID: "attached-test-store", ShareID: "attached-test-share", Limits: shareLimits,
	}
	t.Cleanup(func() { _ = owner.Close() })
	return owner
}

func fileRecord(t *testing.T, size uint64) (catalog.FileID, catalog.NodeRecord) {
	t.Helper()
	file := catalogID[catalog.FileID](9)
	parent := catalogID[catalog.DirectoryID](8)
	locator, _ := catalog.NewLocator(0, "file")
	identity, _ := catalog.NewSourceIdentity([]byte("identity"))
	candidate, _ := catalog.NewVersionCandidate([]byte("candidate"))
	record, err := catalog.NewFileNodeRecord(file, parent, "file.bin", locator, identity, candidate, size, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	return file, record
}

func newRevisionStore(t *testing.T, source RevisionSource, clock Clock, invalidator CacheInvalidator, file catalog.FileID, record catalog.NodeRecord) (*RevisionStore, *capacityTestAccount, *capacityTestAccount) {
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
		Catalog: testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		Source:  source, Clock: clock,
		CapacityCoordinator: owner.Coordinator(),
		CapacityStore: revisioncapacity.StoreConfig{
			StoreID: "test-store", ShareID: "test-share",
			Limits: revisioncapacity.CapacityLimits{StableHandles: 100, ActiveLeases: 100},
		},
		LeaseIDs: &sequenceIDs{}, RevisionDeriver: deriver,
		MetadataBudget: testRevisionMetadataBudget(t, DefaultRevisionInvalidationEntries), CacheInvalidator: invalidator,
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

func TestRevisionStoreSingleflightsOpenAndCreatesIndependentLeases(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, uint64(catalog.MinChunkSize)+1)
	data := append(make([]byte, catalog.MinChunkSize), 'i')
	stable := &testStableFile{data: data}
	reopened := &testStableFile{data: append([]byte(nil), data...)}
	source := &testRevisionSource{files: []*testStableFile{stable, reopened}, started: make(chan struct{}), release: make(chan struct{})}
	store, process, share := newRevisionStore(t, source, clock, nil, file, record)
	session := generousSession(t, store, "session")

	results := make(chan RevisionLease, 2)
	errorsOut := make(chan error, 2)
	for range 2 {
		go func() {
			lease, err := store.OpenRevision(context.Background(), file, session)
			results <- lease
			errorsOut <- err
		}()
	}
	<-source.started
	close(source.release)
	first, second := <-results, <-results
	if err := <-errorsOut; err != nil {
		t.Fatal(err)
	}
	if err := <-errorsOut; err != nil {
		t.Fatal(err)
	}
	if source.Calls() != 1 || first.Descriptor().FileRevision() != second.Descriptor().FileRevision() || first.ID() == second.ID() {
		t.Fatalf("open result: calls=%d first=%+v second=%+v", source.Calls(), first, second)
	}
	if first.Descriptor().BlockCountFieldPresent() {
		t.Fatal("revision descriptor exposed a derived block count")
	}
	if first.TTL() != LeaseTTL || first.RenewAfter() != LeaseTTL-LeaseRenewWindow {
		t.Fatalf("relative lease = ttl %v renew after %v", first.TTL(), first.RenewAfter())
	}

	ref, _ := NewBlockRef(file, first.Descriptor().FileRevision(), 1, first.Descriptor().Geometry())
	data, err := store.ReadBlock(context.Background(), second.ID(), ref)
	if err != nil || string(data) != "i" {
		t.Fatalf("tail block = %q, %v", data, err)
	}
	if _, err := store.RenewLease(second.ID()); !errors.Is(err, ErrRenewTooEarly) {
		t.Fatalf("early renew error = %v", err)
	}
	clock.Advance(LeaseRenewWindow + time.Second)
	renewed, err := store.RenewLease(second.ID())
	if err != nil || renewed.TTL() != LeaseTTL || renewed.RenewAfter() != LeaseTTL-LeaseRenewWindow {
		t.Fatalf("renewed lease = %+v, %v", renewed, err)
	}
	if err := store.EndLease(first.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	if err := store.EndLease(first.ID(), LeaseRelinquished); err != nil {
		t.Fatal("release must be idempotent")
	}
	if err := store.EndLease(second.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	third, err := store.OpenRevision(context.Background(), file, session)
	if err != nil || third.ID().IsZero() || third.ID() == second.ID() ||
		third.Descriptor().FileRevision() != second.Descriptor().FileRevision() || source.Calls() != 2 {
		t.Fatalf("post-relinquishment reopen = %+v, %v, calls=%d", third, err, source.Calls())
	}
	if stable.closed.Load() != 1 {
		t.Fatalf("stable source closes = %d", stable.closed.Load())
	}
	if err := store.EndLease(third.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) ||
		session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
		t.Fatal("capacity leaked after store close")
	}
}

func TestSharedRevisionChargesStableHandleToCurrentLeaseSessions(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store, process, share := newRevisionStore(t, &testRevisionSource{files: []*testStableFile{stable}}, clock, nil, file, record)
	sessionA := generousSession(t, store, "session-a")
	sessionB := generousSession(t, store, "session-b")

	first, err := store.OpenRevision(context.Background(), file, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.OpenRevision(context.Background(), file, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	third, err := store.OpenRevision(context.Background(), file, sessionB)
	if err != nil {
		t.Fatal(err)
	}
	if got := process.Snapshot().Used; got != (QuotaUsage{StableHandles: 1, ActiveLeases: 3}) {
		t.Fatalf("process quota counted a shared handle per session: %+v", got)
	}
	if got := share.Snapshot().Used; got != (QuotaUsage{StableHandles: 1, ActiveLeases: 3}) {
		t.Fatalf("share quota counted a shared handle per session: %+v", got)
	}
	if got := sessionA.Snapshot().Used(); got != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 2}) {
		t.Fatalf("session A quota = %+v", got)
	}
	if got := sessionB.Snapshot().Used(); got != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) {
		t.Fatalf("session B quota = %+v", got)
	}

	_ = store.EndLease(first.ID(), LeaseRelinquished)
	if got := sessionA.Snapshot().Used(); got != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) {
		t.Fatalf("session A released its handle before its last lease: %+v", got)
	}
	_ = store.EndLease(second.ID(), LeaseRelinquished)
	if got := sessionA.Snapshot().Used(); got != (revisioncapacity.CapacityUsage{}) {
		t.Fatalf("session A retained a shared handle after its last lease: %+v", got)
	}
	if got := sessionB.Snapshot().Used(); got != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) {
		t.Fatalf("session A release changed session B accounting: %+v", got)
	}
	_ = store.EndLease(third.ID(), LeaseRelinquished)
	if got := sessionB.Snapshot().Used(); got != (revisioncapacity.CapacityUsage{}) {
		t.Fatalf("session B retained a handle after its last lease: %+v", got)
	}
	if got := process.Snapshot().Used; got != (QuotaUsage{}) {
		t.Fatalf("final relinquishment retained physical capacity: %+v", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateLeaseGuardsCachedDeliveryAcrossIdentityExpiryAndClose(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, 1)
	stable := &testStableFile{data: []byte{1}}
	store, _, _ := newRevisionStore(t, &testRevisionSource{files: []*testStableFile{stable}}, clock, nil, file, record)
	session := generousSession(t, store, "session")
	lease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateLease(lease.ID(), lease.Descriptor()); err != nil {
		t.Fatalf("active lease validation failed: %v", err)
	}
	if err := store.ValidateLease(LeaseID{}, lease.Descriptor()); !errors.Is(err, ErrInvalidLease) {
		t.Fatalf("zero lease validation error=%v", err)
	}
	otherRevision := catalogID[FileRevision](99)
	wrongDescriptor, err := NewFileRevisionDescriptor(
		lease.Descriptor().ShareInstance(), file, otherRevision,
		lease.Descriptor().Geometry(), lease.Descriptor().ModifiedTime(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateLease(lease.ID(), wrongDescriptor); !errors.Is(err, ErrInvalidLease) {
		t.Fatalf("cross-revision lease validation error=%v", err)
	}
	clock.Advance(LeaseTTL)
	if err := store.ValidateLease(lease.ID(), lease.Descriptor()); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expired lease validation error=%v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateLease(lease.ID(), lease.Descriptor()); !errors.Is(err, ErrRevisionStoreClosed) {
		t.Fatalf("closed store validation error=%v", err)
	}
}

func TestRevisionDriftInvalidatesEveryLeaseAndCache(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, uint64(catalog.MinChunkSize))
	stable := &testStableFile{data: make([]byte, catalog.MinChunkSize)}
	source := &testRevisionSource{files: []*testStableFile{stable}}
	invalidator := &recordingInvalidator{}
	store, process, share := newRevisionStore(t, source, clock, invalidator, file, record)
	session := generousSession(t, store, "session")
	first, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	stable.drifted.Store(true)
	ref, _ := NewBlockRef(file, first.Descriptor().FileRevision(), 0, first.Descriptor().Geometry())
	if _, err := store.ReadBlock(context.Background(), first.ID(), ref); !errors.Is(err, ErrRevisionDrift) {
		t.Fatalf("drift read error = %v", err)
	}
	if _, err := store.ReadBlock(context.Background(), second.ID(), ref); !errors.Is(err, ErrRevisionDrift) {
		t.Fatalf("sibling lease error = %v", err)
	}
	if stable.closed.Load() != 1 || len(invalidator.revisions) != 1 {
		t.Fatalf("drift cleanup: closes=%d invalidations=%d", stable.closed.Load(), len(invalidator.revisions))
	}
	if process.Snapshot().Used != (QuotaUsage{}) || share.Snapshot().Used != (QuotaUsage{}) ||
		session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
		t.Fatal("drift leaked capacity")
	}
}

func TestOpenRevisionsPreservesOrderAndPerItemFailure(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, uint64(catalog.MinChunkSize))
	stable := &testStableFile{data: make([]byte, catalog.MinChunkSize)}
	store, _, _ := newRevisionStore(t, &testRevisionSource{files: []*testStableFile{stable}}, clock, nil, file, record)
	session := generousSession(t, store, "session")
	missing := catalogID[catalog.FileID](77)
	results, err := store.OpenRevisions(context.Background(), []OpenRevisionRequest{{FileID: missing}, {FileID: file}}, session)
	if err != nil || len(results) != 2 || results[0].FileID != missing || results[0].Err == nil || results[1].FileID != file || results[1].Err != nil {
		t.Fatalf("batch result = %+v, %v", results, err)
	}
	tooMany := make([]OpenRevisionRequest, MaxOpenRevisionBatch+1)
	if _, err := store.OpenRevisions(context.Background(), tooMany, session); !errors.Is(err, ErrOpenBatchLimit) {
		t.Fatalf("batch limit error = %v", err)
	}
	manyRanges := make([]Range, MaxInitialRangesPerFile+1)
	for index := range manyRanges {
		manyRanges[index] = Range{Offset: uint64(index * 2), End: uint64(index*2 + 1)}
	}
	rangeSet, err := NewRangeSet(manyRanges)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenRevisions(context.Background(), []OpenRevisionRequest{{FileID: file, InitialRanges: rangeSet}}, session); !errors.Is(err, ErrInitialRangeLimit) {
		t.Fatalf("per-file initial-range limit = %v", err)
	}
	validRanges, err := NewRangeSet(manyRanges[:MaxInitialRangesPerFile])
	if err != nil {
		t.Fatal(err)
	}
	totalOverflow := make([]OpenRevisionRequest, MaxInitialRangesPerRequest/MaxInitialRangesPerFile+1)
	for index := range totalOverflow {
		totalOverflow[index] = OpenRevisionRequest{FileID: file, InitialRanges: validRanges}
	}
	if _, err := store.OpenRevisions(context.Background(), totalOverflow, session); !errors.Is(err, ErrInitialRangeLimit) {
		t.Fatalf("request initial-range limit = %v", err)
	}
}
