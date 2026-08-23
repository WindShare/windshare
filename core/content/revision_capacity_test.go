package content

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
)

type capacityTraceRecorder struct {
	mu     sync.Mutex
	events []revisioncapacity.TraceEvent
}

type claimReactivationTrace struct {
	claimed      chan struct{}
	allowClaim   chan struct{}
	declined     chan struct{}
	allowDecline chan struct{}
	claimedOnce  sync.Once
	declinedOnce sync.Once
}

func (r *claimReactivationTrace) TraceCapacity(event revisioncapacity.TraceEvent) {
	switch event.Stage() {
	case revisioncapacity.TraceReclaimClaimed:
		r.claimedOnce.Do(func() {
			close(r.claimed)
			<-r.allowClaim
		})
	case revisioncapacity.TraceReclaimDeclined:
		r.declinedOnce.Do(func() {
			close(r.declined)
			<-r.allowDecline
		})
	}
}

func (r *capacityTraceRecorder) TraceCapacity(event revisioncapacity.TraceEvent) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *capacityTraceRecorder) snapshot() []revisioncapacity.TraceEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]revisioncapacity.TraceEvent(nil), r.events...)
}

type mappedRevisionSource struct {
	mu    sync.Mutex
	files map[catalog.FileID]StableFile
	calls map[catalog.FileID]int
}

type firstNodeBlockingCatalog struct {
	records map[catalog.NodeID]catalog.NodeRecord
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (c *firstNodeBlockingCatalog) Node(ctx context.Context, id catalog.NodeID) (catalog.NodeRecord, bool, error) {
	if c.calls.Add(1) == 1 {
		close(c.started)
		select {
		case <-ctx.Done():
			return catalog.NodeRecord{}, false, ctx.Err()
		case <-c.release:
		}
	}
	record, exists := c.records[id]
	return record, exists, nil
}

func (s *mappedRevisionSource) OpenStable(_ context.Context, record catalog.NodeRecord) (StableFile, error) {
	file, ok := record.FileID()
	if !ok {
		return nil, errors.New("capacity test record is not a file")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[file]++
	stable := s.files[file]
	if stable == nil {
		return nil, errors.New("capacity test has no stable source")
	}
	return stable, nil
}

func (s *mappedRevisionSource) callCount(file catalog.FileID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[file]
}

type capacityStableFile struct {
	data            []byte
	modified        catalog.ModifiedTime
	readStarted     chan struct{}
	readRelease     chan struct{}
	closeStarted    chan struct{}
	closeRelease    chan struct{}
	closeErr        error
	closePanic      bool
	readStartOnce   sync.Once
	closeStartOnce  sync.Once
	readers         atomic.Int32
	closed          atomic.Int32
	closeDuringRead atomic.Bool
}

func (f *capacityStableFile) ExactSize() uint64                  { return uint64(len(f.data)) }
func (f *capacityStableFile) ModifiedTime() catalog.ModifiedTime { return f.modified }
func (*capacityStableFile) Verify(context.Context) error         { return nil }

func (f *capacityStableFile) ReadAt(ctx context.Context, destination []byte, offset uint64) (int, error) {
	f.readers.Add(1)
	defer f.readers.Add(-1)
	if f.readStarted != nil {
		f.readStartOnce.Do(func() { close(f.readStarted) })
	}
	if f.readRelease != nil {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-f.readRelease:
		}
	}
	if offset >= uint64(len(f.data)) {
		return 0, io.EOF
	}
	count := copy(destination, f.data[offset:])
	if count != len(destination) {
		return count, io.EOF
	}
	return count, nil
}

func (f *capacityStableFile) Close() error {
	f.closed.Add(1)
	if f.readers.Load() != 0 {
		f.closeDuringRead.Store(true)
	}
	if f.closeStarted != nil {
		f.closeStartOnce.Do(func() { close(f.closeStarted) })
	}
	if f.closeRelease != nil {
		<-f.closeRelease
	}
	if f.closePanic {
		panic("capacity close panic")
	}
	return f.closeErr
}

func newCapacityTestOwner(
	t *testing.T,
	limits revisioncapacity.CapacityLimits,
	tracer revisioncapacity.Tracer,
) *revisioncapacity.ProcessOwner {
	t.Helper()
	owner, err := revisioncapacity.NewProcessOwner(revisioncapacity.ProcessConfig{
		Limits: limits, RetryAfter: revisioncapacity.DefaultCapacityRetryAfter, Tracer: tracer,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.Close(); err != nil {
			t.Errorf("close capacity owner: %v", err)
		}
	})
	return owner
}

func newCapacityTestStore(
	t *testing.T,
	owner *revisioncapacity.ProcessOwner,
	storeID revisioncapacity.StoreID,
	shareID revisioncapacity.ShareID,
	shareByte byte,
	limits revisioncapacity.CapacityLimits,
	clock Clock,
	records map[catalog.NodeID]catalog.NodeRecord,
	source RevisionSource,
) *RevisionStore {
	t.Helper()
	deriver := testRevisionDeriver(t)
	store, err := NewRevisionStore(RevisionStoreConfig{
		ShareInstance: catalogID[catalog.ShareInstance](shareByte), ChunkSize: catalog.MinChunkSize,
		Catalog: testCatalog{records: records}, Source: source, Clock: clock,
		LeaseIDs: &sequenceIDs{}, RevisionDeriver: deriver,
		MetadataBudget:      testRevisionMetadataBudget(t, DefaultRevisionInvalidationEntries),
		CapacityCoordinator: owner.Coordinator(),
		CapacityStore:       revisioncapacity.StoreConfig{StoreID: storeID, ShareID: shareID, Limits: limits},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close capacity store %s: %v", storeID, err)
		}
		deriver.Destroy()
	})
	return store
}

func capacityRecord(t *testing.T, fileByte byte) (catalog.FileID, catalog.NodeRecord) {
	t.Helper()
	return fileRecordWithModifiedTime(t, fileByte, 1, catalog.ModifiedTime{})
}

func capacitySession(
	t *testing.T,
	store *RevisionStore,
	id string,
	limits revisioncapacity.CapacityLimits,
) *revisioncapacity.SessionRegistration {
	t.Helper()
	session, err := store.RegisterSession(revisioncapacity.SessionConfig{
		SessionID: revisioncapacity.SessionID(id), Limits: limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestRevisionStoreReclaimsOldestIdleAboveShareLimit(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	traces := &capacityTraceRecorder{}
	owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 3, ActiveLeases: 10}, traces)
	limits := revisioncapacity.CapacityLimits{StableHandles: 2, ActiveLeases: 10}
	firstID, firstRecord := capacityRecord(t, 11)
	secondID, secondRecord := capacityRecord(t, 12)
	thirdID, thirdRecord := capacityRecord(t, 13)
	first := &capacityStableFile{data: []byte{1}}
	second := &capacityStableFile{data: []byte{2}}
	third := &capacityStableFile{data: []byte{3}}
	source := &mappedRevisionSource{files: map[catalog.FileID]StableFile{
		firstID: first, secondID: second, thirdID: third,
	}, calls: make(map[catalog.FileID]int)}
	store := newCapacityTestStore(t, owner, "store", "share", 1, limits, clock, map[catalog.NodeID]catalog.NodeRecord{
		firstID.NodeID(): firstRecord, secondID.NodeID(): secondRecord, thirdID.NodeID(): thirdRecord,
	}, source)
	session := capacitySession(t, store, "session", revisioncapacity.CapacityLimits{StableHandles: 3, ActiveLeases: 10})

	firstLease, err := store.OpenRevision(context.Background(), firstID, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EndLease(firstLease.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	secondLease, err := store.OpenRevision(context.Background(), secondID, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EndLease(secondLease.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}
	thirdLease, err := store.OpenRevision(context.Background(), thirdID, session)
	if err != nil {
		t.Fatalf("open above stable limit: %v", err)
	}
	if first.closed.Load() != 1 || second.closed.Load() != 0 || third.closed.Load() != 0 {
		t.Fatalf("deterministic oldest reclaim closes=(%d,%d,%d)", first.closed.Load(), second.closed.Load(), third.closed.Load())
	}
	snapshot := store.CapacitySnapshot()
	want := revisioncapacity.CapacityUsage{StableHandles: 2, ActiveLeases: 1}
	if snapshot.Process().Used() != want || snapshot.Share().Used() != want ||
		snapshot.Process().ReclaimableStableHandles() != 1 || snapshot.Share().ReclaimableStableHandles() != 1 ||
		snapshot.Process().PendingAdmissions() != 0 || snapshot.Process().ActiveReclaims() != 0 {
		t.Fatalf("post-handoff snapshot=%+v", snapshot)
	}
	stages := make(map[revisioncapacity.TraceStage]int)
	for _, event := range traces.snapshot() {
		stages[event.Stage()]++
		if event.Snapshot().Process().Used().StableHandles > 3 || event.Snapshot().Share().Used().StableHandles > 2 {
			t.Fatalf("trace observed overbooking: %+v", event.Snapshot())
		}
	}
	if stages[revisioncapacity.TraceIdlePublished] < 2 || stages[revisioncapacity.TraceReclaimClaimed] != 1 ||
		stages[revisioncapacity.TraceReclaimCompleted] != 1 || stages[revisioncapacity.TraceAdmissionGranted] < 3 {
		t.Fatalf("capacity trace stages=%v", stages)
	}
	if err := store.EndLease(thirdLease.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
}

func TestRevisionStoreCrossShareHandoffWaitsForTerminalClose(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 10}, nil)
	limits := revisioncapacity.CapacityLimits{StableHandles: 2, ActiveLeases: 10}
	victimID, victimRecord := capacityRecord(t, 21)
	requestID, requestRecord := capacityRecord(t, 22)
	victim := &capacityStableFile{
		data: []byte{1}, closeStarted: make(chan struct{}), closeRelease: make(chan struct{}),
	}
	requesterFile := &capacityStableFile{data: []byte{2}}
	victimSource := &mappedRevisionSource{files: map[catalog.FileID]StableFile{victimID: victim}, calls: make(map[catalog.FileID]int)}
	requestSource := &mappedRevisionSource{files: map[catalog.FileID]StableFile{requestID: requesterFile}, calls: make(map[catalog.FileID]int)}
	victimStore := newCapacityTestStore(t, owner, "victim", "share-a", 1, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{victimID.NodeID(): victimRecord}, victimSource)
	requestStore := newCapacityTestStore(t, owner, "requester", "share-b", 2, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord}, requestSource)
	victimSession := capacitySession(t, victimStore, "victim-session", limits)
	requestSession := capacitySession(t, requestStore, "request-session", limits)
	lease, err := victimStore.OpenRevision(context.Background(), victimID, victimSession)
	if err != nil {
		t.Fatal(err)
	}
	if err := victimStore.EndLease(lease.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}

	result := make(chan struct {
		lease RevisionLease
		err   error
	}, 1)
	go func() {
		opened, openErr := requestStore.OpenRevision(context.Background(), requestID, requestSession)
		result <- struct {
			lease RevisionLease
			err   error
		}{opened, openErr}
	}()
	<-victim.closeStarted
	if requestSource.callCount(requestID) != 0 {
		t.Fatal("requester source opened before victim Close returned")
	}
	select {
	case premature := <-result:
		t.Fatalf("capacity granted before terminal Close: %+v", premature)
	default:
	}
	close(victim.closeRelease)
	opened := <-result
	if opened.err != nil || opened.lease.ID().IsZero() || requestSource.callCount(requestID) != 1 {
		t.Fatalf("cross-share handoff=(%x,%v) requester opens=%d", opened.lease.ID(), opened.err, requestSource.callCount(requestID))
	}
	snapshot := requestStore.CapacitySnapshot()
	if snapshot.Process().Used() != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) ||
		snapshot.Share().Used() != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) {
		t.Fatalf("cross-share accounting=%+v", snapshot)
	}
}

func TestRevisionStoreCrossShareReclaimsExpiredIdleWithoutVictimActivity(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 4}, nil)
	limits := revisioncapacity.CapacityLimits{StableHandles: 2, ActiveLeases: 4}
	victimID, victimRecord := capacityRecord(t, 28)
	requestID, requestRecord := capacityRecord(t, 29)
	victim := &capacityStableFile{data: []byte{1}}
	requester := &capacityStableFile{data: []byte{2}}
	victimStore := newCapacityTestStore(t, owner, "expired-victim", "expired-share-a", 1, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{victimID.NodeID(): victimRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{victimID: victim}, calls: make(map[catalog.FileID]int)})
	requestStore := newCapacityTestStore(t, owner, "expired-requester", "expired-share-b", 2, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{requestID: requester}, calls: make(map[catalog.FileID]int)})
	victimSession := capacitySession(t, victimStore, "expired-victim-session", limits)
	requestSession := capacitySession(t, requestStore, "expired-request-session", limits)
	lease, err := victimStore.OpenRevision(context.Background(), victimID, victimSession)
	if err != nil {
		t.Fatal(err)
	}
	if err := victimStore.EndLease(lease.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}
	clock.Advance(RevisionResumeGrace + time.Second)

	opened, err := requestStore.OpenRevision(context.Background(), requestID, requestSession)
	if err != nil {
		t.Fatalf("cross-share admission after inactive victim grace: %v", err)
	}
	if victim.closed.Load() != 1 {
		t.Fatalf("expired idle victim closes=%d, want 1", victim.closed.Load())
	}
	snapshot := requestStore.CapacitySnapshot()
	if snapshot.Process().Used() != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) ||
		snapshot.Process().ReclaimableStableHandles() != 0 || snapshot.Process().ActiveReclaims() != 0 {
		t.Fatalf("expired cross-share handoff accounting=%+v", snapshot)
	}
	if err := requestStore.EndLease(opened.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
}

func TestRevisionStoreClaimedReactivationFailureRepublishesOwnedHandle(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	tracer := &claimReactivationTrace{
		claimed: make(chan struct{}), allowClaim: make(chan struct{}),
		declined: make(chan struct{}), allowDecline: make(chan struct{}),
	}
	defer func() {
		select {
		case <-tracer.allowClaim:
		default:
			close(tracer.allowClaim)
		}
		select {
		case <-tracer.allowDecline:
		default:
			close(tracer.allowDecline)
		}
	}()
	owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 1}, tracer)
	limits := revisioncapacity.CapacityLimits{StableHandles: 2, ActiveLeases: 2}
	victimID, victimRecord := capacityRecord(t, 33)
	requestID, requestRecord := capacityRecord(t, 34)
	victim := &capacityStableFile{data: []byte{1}}
	requester := &capacityStableFile{data: []byte{2}}
	victimStore := newCapacityTestStore(t, owner, "reactivation-victim", "reactivation-share-a", 1, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{victimID.NodeID(): victimRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{victimID: victim}, calls: make(map[catalog.FileID]int)})
	requestStore := newCapacityTestStore(t, owner, "reactivation-requester", "reactivation-share-b", 2, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{requestID: requester}, calls: make(map[catalog.FileID]int)})
	victimSession := capacitySession(t, victimStore, "reactivation-victim-session", limits)
	requestSession := capacitySession(t, requestStore, "reactivation-request-session", limits)
	lease, err := victimStore.OpenRevision(context.Background(), victimID, victimSession)
	if err != nil {
		t.Fatal(err)
	}
	if err := victimStore.EndLease(lease.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}

	type openResult struct {
		lease RevisionLease
		err   error
	}
	requestResult := make(chan openResult, 1)
	go func() {
		opened, openErr := requestStore.OpenRevision(context.Background(), requestID, requestSession)
		requestResult <- openResult{lease: opened, err: openErr}
	}()
	select {
	case <-tracer.claimed:
	case <-time.After(2 * time.Second):
		t.Fatal("requester did not claim the idle victim")
	}
	victimResult := make(chan error, 1)
	go func() {
		_, openErr := victimStore.OpenRevision(context.Background(), victimID, victimSession)
		victimResult <- openErr
	}()
	deadline := time.After(2 * time.Second)
	for {
		victimStore.mu.Lock()
		revision := victimStore.revisions[victimID]
		waitingForClaim := revision != nil && revision.admissionDone != nil && revision.idleToken == ""
		victimStore.mu.Unlock()
		if waitingForClaim {
			break
		}
		select {
		case <-deadline:
			t.Fatal("victim reactivation did not withdraw the claimed generation")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(tracer.allowClaim)
	select {
	case <-tracer.declined:
	case <-time.After(2 * time.Second):
		t.Fatal("reactivated victim did not decline its stale claim")
	}
	select {
	case reactivateErr := <-victimResult:
		var busy *revisioncapacity.CapacityBusyError
		if !errors.As(reactivateErr, &busy) || busy.Resource() != revisioncapacity.CapacityResourceActiveLease {
			t.Fatalf("reactivation under reserved active capacity=%v", reactivateErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reactivation did not finish after stale claim resolution")
	}
	close(tracer.allowDecline)
	var opened openResult
	select {
	case opened = <-requestResult:
	case <-time.After(2 * time.Second):
		t.Fatal("requester did not retry the republished victim")
	}
	if opened.err != nil || opened.lease.ID().IsZero() {
		t.Fatalf("requester after victim republication=(%x,%v)", opened.lease.ID(), opened.err)
	}
	if victim.closed.Load() != 1 {
		t.Fatalf("victim terminal closes=%d, want exactly 1", victim.closed.Load())
	}
	snapshot := requestStore.CapacitySnapshot()
	if snapshot.Process().Used() != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) ||
		snapshot.Process().ReclaimableStableHandles() != 0 || snapshot.Process().ActiveReclaims() != 0 ||
		snapshot.Process().PendingAdmissions() != 0 {
		t.Fatalf("post-reactivation ownership accounting=%+v", snapshot)
	}
	if err := requestStore.EndLease(opened.lease.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	if snapshot := requestStore.CapacitySnapshot(); snapshot.Process().Used() != (revisioncapacity.CapacityUsage{}) ||
		snapshot.Process().ReclaimableStableHandles() != 0 || snapshot.Process().ActiveReclaims() != 0 ||
		snapshot.Process().PendingAdmissions() != 0 {
		t.Fatalf("terminal ownership accounting=%+v", snapshot)
	}
	if err := victimStore.Close(); err != nil {
		t.Fatalf("close victim store: %v", err)
	}
	if err := requestStore.Close(); err != nil {
		t.Fatalf("close requester store: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("close process owner: %v", err)
	}
}

func TestRevisionStoreCoalescedWaiterRetriesAfterOwnerSessionCloses(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 2, ActiveLeases: 2}, nil)
	limits := revisioncapacity.CapacityLimits{StableHandles: 2, ActiveLeases: 2}
	fileID, record := capacityRecord(t, 35)
	stable := &capacityStableFile{data: []byte{1}}
	source := &mappedRevisionSource{files: map[catalog.FileID]StableFile{fileID: stable}, calls: make(map[catalog.FileID]int)}
	store := newCapacityTestStore(t, owner, "coalesced-store", "coalesced-share", 1, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{fileID.NodeID(): record}, source)
	blockedCatalog := &firstNodeBlockingCatalog{
		records: map[catalog.NodeID]catalog.NodeRecord{fileID.NodeID(): record},
		started: make(chan struct{}), release: make(chan struct{}),
	}
	store.catalog = blockedCatalog
	defer func() {
		select {
		case <-blockedCatalog.release:
		default:
			close(blockedCatalog.release)
		}
	}()
	sessionA := capacitySession(t, store, "coalesced-a", limits)
	sessionB := capacitySession(t, store, "coalesced-b", limits)
	type openResult struct {
		lease RevisionLease
		err   error
	}
	ownerResult := make(chan openResult, 1)
	waiterResult := make(chan openResult, 1)
	go func() {
		lease, openErr := store.OpenRevision(context.Background(), fileID, sessionA)
		ownerResult <- openResult{lease: lease, err: openErr}
	}()
	select {
	case <-blockedCatalog.started:
	case <-time.After(2 * time.Second):
		t.Fatal("owner open did not reach catalog lookup")
	}
	go func() {
		lease, openErr := store.OpenRevision(context.Background(), fileID, sessionB)
		waiterResult <- openResult{lease: lease, err: openErr}
	}()
	deadline := time.After(2 * time.Second)
	joined := false
	for !joined {
		store.mu.Lock()
		attempt := store.opening[fileID]
		joined = attempt != nil && attempt.waiters == 2
		store.mu.Unlock()
		if joined {
			break
		}
		select {
		case <-deadline:
			t.Fatal("second session did not join the store-scoped open attempt")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if err := sessionA.Close(); err != nil {
		t.Fatalf("close initiating session before admission: %v", err)
	}
	close(blockedCatalog.release)
	ownerOpen := <-ownerResult
	if !errors.Is(ownerOpen.err, revisioncapacity.ErrRegistrationClosing) {
		t.Fatalf("initiating closed session result=%v", ownerOpen.err)
	}
	waiterOpen := <-waiterResult
	if waiterOpen.err != nil || waiterOpen.lease.ID().IsZero() {
		t.Fatalf("healthy coalesced waiter=(%x,%v)", waiterOpen.lease.ID(), waiterOpen.err)
	}
	if blockedCatalog.calls.Load() != 2 || source.callCount(fileID) != 1 {
		t.Fatalf("coalesced retry catalog calls=%d source opens=%d", blockedCatalog.calls.Load(), source.callCount(fileID))
	}
	want := revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}
	if sessionA.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) || sessionB.Snapshot().Used() != want ||
		store.CapacitySnapshot().Process().Used() != want {
		t.Fatalf("coalesced retry charges owner=%+v waiter=%+v process=%+v",
			sessionA.Snapshot(), sessionB.Snapshot(), store.CapacitySnapshot().Process())
	}
	if err := store.EndLease(waiterOpen.lease.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
}

func TestRevisionStoreCrossShareReclaimSelectionIsDeterministic(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 2, ActiveLeases: 10}, nil)
	limits := revisioncapacity.CapacityLimits{StableHandles: 2, ActiveLeases: 10}
	bID, bRecord := capacityRecord(t, 23)
	aID, aRecord := capacityRecord(t, 24)
	requestID, requestRecord := capacityRecord(t, 25)
	bFile := &capacityStableFile{data: []byte{1}}
	aFile := &capacityStableFile{data: []byte{2}}
	requestFile := &capacityStableFile{data: []byte{3}}
	storeB := newCapacityTestStore(t, owner, "victim-b", "share-b", 1, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{bID.NodeID(): bRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{bID: bFile}, calls: make(map[catalog.FileID]int)})
	storeA := newCapacityTestStore(t, owner, "victim-a", "share-a", 2, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{aID.NodeID(): aRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{aID: aFile}, calls: make(map[catalog.FileID]int)})
	requestStore := newCapacityTestStore(t, owner, "requester", "share-request", 3, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{requestID: requestFile}, calls: make(map[catalog.FileID]int)})
	sessionB := capacitySession(t, storeB, "session-b", limits)
	sessionA := capacitySession(t, storeA, "session-a", limits)
	requestSession := capacitySession(t, requestStore, "session-request", limits)
	leaseB, err := storeB.OpenRevision(context.Background(), bID, sessionB)
	if err != nil {
		t.Fatal(err)
	}
	if err := storeB.EndLease(leaseB.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}
	leaseA, err := storeA.OpenRevision(context.Background(), aID, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	if err := storeA.EndLease(leaseA.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}
	if _, err := requestStore.OpenRevision(context.Background(), requestID, requestSession); err != nil {
		t.Fatal(err)
	}
	if aFile.closed.Load() != 1 || bFile.closed.Load() != 0 {
		t.Fatalf("cross-share tie broke by registration order instead of stable identity: a=%d b=%d", aFile.closed.Load(), bFile.closed.Load())
	}
}

func TestRevisionStoreCloseJoinsInFlightReclaimCallback(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 4}, nil)
	limits := revisioncapacity.CapacityLimits{StableHandles: 2, ActiveLeases: 4}
	victimID, victimRecord := capacityRecord(t, 26)
	requestID, requestRecord := capacityRecord(t, 27)
	victim := &capacityStableFile{data: []byte{1}, closeStarted: make(chan struct{}), closeRelease: make(chan struct{})}
	victimStore := newCapacityTestStore(t, owner, "closing-victim", "closing-a", 1, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{victimID.NodeID(): victimRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{victimID: victim}, calls: make(map[catalog.FileID]int)})
	requestStore := newCapacityTestStore(t, owner, "closing-request", "closing-b", 2, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord},
		&mappedRevisionSource{files: map[catalog.FileID]StableFile{requestID: &capacityStableFile{data: []byte{2}}}, calls: make(map[catalog.FileID]int)})
	victimSession := capacitySession(t, victimStore, "closing-victim-session", limits)
	requestSession := capacitySession(t, requestStore, "closing-request-session", limits)
	lease, err := victimStore.OpenRevision(context.Background(), victimID, victimSession)
	if err != nil {
		t.Fatal(err)
	}
	if err := victimStore.EndLease(lease.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}
	openResult := make(chan error, 1)
	go func() {
		_, openErr := requestStore.OpenRevision(context.Background(), requestID, requestSession)
		openResult <- openErr
	}()
	<-victim.closeStarted
	closeResult := make(chan error, 1)
	go func() { closeResult <- victimStore.Close() }()
	select {
	case err := <-closeResult:
		t.Fatalf("store close escaped its reclaim callback: %v", err)
	default:
	}
	close(victim.closeRelease)
	if err := <-openResult; err != nil {
		t.Fatalf("requester failed after victim close race: %v", err)
	}
	if err := <-closeResult; err != nil {
		t.Fatalf("victim store close race: %v", err)
	}
	if snapshot := requestStore.CapacitySnapshot(); snapshot.Process().Used() != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) ||
		snapshot.Process().ActiveReclaims() != 0 || snapshot.Process().PendingAdmissions() != 0 {
		t.Fatalf("close/reclaim race accounting=%+v", snapshot)
	}
}

func TestRevisionStoreNeverReclaimsActiveReader(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 10}, nil)
	limits := revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 10}
	readID, readRecord := capacityRecord(t, 31)
	requestID, requestRecord := capacityRecord(t, 32)
	reading := &capacityStableFile{
		data: []byte{1}, readStarted: make(chan struct{}), readRelease: make(chan struct{}),
	}
	requester := &capacityStableFile{data: []byte{2}}
	source := &mappedRevisionSource{files: map[catalog.FileID]StableFile{readID: reading, requestID: requester}, calls: make(map[catalog.FileID]int)}
	store := newCapacityTestStore(t, owner, "reader-store", "reader-share", 1, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{readID.NodeID(): readRecord, requestID.NodeID(): requestRecord}, source)
	session := capacitySession(t, store, "reader-session", limits)
	lease, err := store.OpenRevision(context.Background(), readID, session)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := NewBlockRef(readID, lease.Descriptor().FileRevision(), 0, lease.Descriptor().Geometry())
	readResult := make(chan error, 1)
	go func() {
		_, readErr := store.ReadBlock(context.Background(), lease.ID(), ref)
		readResult <- readErr
	}()
	<-reading.readStarted
	if err := store.EndLease(lease.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}
	if store.CapacitySnapshot().Share().ReclaimableStableHandles() != 0 {
		t.Fatal("active reader was published as reclaimable")
	}
	var busy *revisioncapacity.CapacityBusyError
	if _, err := store.OpenRevision(context.Background(), requestID, session); !errors.As(err, &busy) || busy.Resource() != revisioncapacity.CapacityResourceStableHandle {
		t.Fatalf("reader pressure error=%v", err)
	}
	if reading.closed.Load() != 0 || reading.closeDuringRead.Load() {
		t.Fatal("pressure closed an active reader")
	}
	close(reading.readRelease)
	if err := <-readResult; err != nil {
		t.Fatal(err)
	}
	if store.CapacitySnapshot().Share().ReclaimableStableHandles() != 1 {
		t.Fatal("completed detached reader was not registered for recovery")
	}
	opened, err := store.OpenRevision(context.Background(), requestID, session)
	if err != nil || opened.ID().IsZero() || reading.closed.Load() != 1 || reading.closeDuringRead.Load() {
		t.Fatalf("post-reader reclaim=(%x,%v) closes=%d during=%v", opened.ID(), err, reading.closed.Load(), reading.closeDuringRead.Load())
	}
}

func TestRevisionStoreMixedLeasesPublishOnlyAfterLastActiveLease(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 4}, nil)
	limits := revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 4}
	firstID, firstRecord := capacityRecord(t, 41)
	secondID, secondRecord := capacityRecord(t, 42)
	first := &capacityStableFile{data: []byte{1}}
	second := &capacityStableFile{data: []byte{2}}
	source := &mappedRevisionSource{files: map[catalog.FileID]StableFile{firstID: first, secondID: second}, calls: make(map[catalog.FileID]int)}
	store := newCapacityTestStore(t, owner, "mixed-store", "mixed-share", 1, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{firstID.NodeID(): firstRecord, secondID.NodeID(): secondRecord}, source)
	sessionA := capacitySession(t, store, "mixed-a", limits)
	sessionB := capacitySession(t, store, "mixed-b", limits)
	leaseA, err := store.OpenRevision(context.Background(), firstID, sessionA)
	if err != nil {
		t.Fatal(err)
	}
	leaseB, err := store.OpenRevision(context.Background(), firstID, sessionB)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EndLease(leaseA.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}
	if snapshot := store.CapacitySnapshot(); snapshot.Share().ReclaimableStableHandles() != 0 ||
		snapshot.Share().Used() != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) ||
		sessionA.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) ||
		sessionB.Snapshot().Used() != (revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1}) {
		t.Fatalf("mixed active/detached accounting=%+v", snapshot)
	}
	if err := store.EndLease(leaseB.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	if store.CapacitySnapshot().Share().ReclaimableStableHandles() != 1 {
		t.Fatal("last explicit ending erased earlier detached recovery")
	}
	if _, err := store.OpenRevision(context.Background(), secondID, sessionB); err != nil {
		t.Fatal(err)
	}
	if first.closed.Load() != 1 {
		t.Fatal("mixed-lease idle revision was not reclaimed")
	}
}

func TestRevisionStoreCancellationDuringCloseReleasesVictimWithoutGrant(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 4}, nil)
	limits := revisioncapacity.CapacityLimits{StableHandles: 2, ActiveLeases: 4}
	victimID, victimRecord := capacityRecord(t, 51)
	requestID, requestRecord := capacityRecord(t, 52)
	victim := &capacityStableFile{data: []byte{1}, closeStarted: make(chan struct{}), closeRelease: make(chan struct{})}
	requester := &capacityStableFile{data: []byte{2}}
	victimSource := &mappedRevisionSource{files: map[catalog.FileID]StableFile{victimID: victim}, calls: make(map[catalog.FileID]int)}
	requestSource := &mappedRevisionSource{files: map[catalog.FileID]StableFile{requestID: requester}, calls: make(map[catalog.FileID]int)}
	victimStore := newCapacityTestStore(t, owner, "cancel-victim", "cancel-a", 1, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{victimID.NodeID(): victimRecord}, victimSource)
	requestStore := newCapacityTestStore(t, owner, "cancel-requester", "cancel-b", 2, limits, clock,
		map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord}, requestSource)
	victimSession := capacitySession(t, victimStore, "cancel-victim-session", limits)
	requestSession := capacitySession(t, requestStore, "cancel-request-session", limits)
	lease, err := victimStore.OpenRevision(context.Background(), victimID, victimSession)
	if err != nil {
		t.Fatal(err)
	}
	if err := victimStore.EndLease(lease.ID(), LeaseDetached); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, openErr := requestStore.OpenRevision(ctx, requestID, requestSession)
		result <- openErr
	}()
	<-victim.closeStarted
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled handoff=%v", err)
	}
	if victim.closed.Load() != 1 {
		t.Fatal("detached victim did not continue terminal Close after requester cancellation")
	}
	close(victim.closeRelease)
	requestStore.openWG.Wait()
	if requestSource.callCount(requestID) != 0 || victim.closed.Load() != 1 ||
		requestStore.CapacitySnapshot().Process().Used() != (revisioncapacity.CapacityUsage{}) {
		t.Fatalf("cancelled handoff requester opens=%d closes=%d snapshot=%+v",
			requestSource.callCount(requestID), victim.closed.Load(), requestStore.CapacitySnapshot())
	}
}

func TestRevisionStoreCloseFailureOwnershipOutcomes(t *testing.T) {
	t.Run("ordinary diagnostic error releases after return", func(t *testing.T) {
		clock := &testClock{now: time.Unix(100, 0)}
		sentinel := errors.New("ordinary diagnostic close failure")
		owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 2}, nil)
		limits := revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 2}
		fileID, record := capacityRecord(t, 60)
		stable := &capacityStableFile{data: []byte{1}, closeErr: sentinel}
		store := newCapacityTestStore(t, owner, "ordinary-error", "ordinary-error-share", 1, limits, clock,
			map[catalog.NodeID]catalog.NodeRecord{fileID.NodeID(): record},
			&mappedRevisionSource{files: map[catalog.FileID]StableFile{fileID: stable}, calls: make(map[catalog.FileID]int)})
		session := capacitySession(t, store, "ordinary-error-session", limits)
		lease, err := store.OpenRevision(context.Background(), fileID, session)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.EndLease(lease.ID(), LeaseRelinquished); !errors.Is(err, sentinel) {
			t.Fatalf("ordinary close diagnostic=%v", err)
		}
		if stable.closed.Load() != 1 || store.CapacitySnapshot().Process().Used() != (revisioncapacity.CapacityUsage{}) ||
			store.CapacitySnapshot().Process().QuarantinedStableHandles() != 0 {
			t.Fatalf("ordinary terminal close accounting=%+v closes=%d", store.CapacitySnapshot(), stable.closed.Load())
		}
	})

	t.Run("ordinary panic quarantines uncertain ownership", func(t *testing.T) {
		clock := &testClock{now: time.Unix(100, 0)}
		owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 2}, nil)
		limits := revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 2}
		fileID, record := capacityRecord(t, 59)
		stable := &capacityStableFile{data: []byte{1}, closePanic: true}
		store := newCapacityTestStore(t, owner, "ordinary-panic", "ordinary-panic-share", 1, limits, clock,
			map[catalog.NodeID]catalog.NodeRecord{fileID.NodeID(): record},
			&mappedRevisionSource{files: map[catalog.FileID]StableFile{fileID: stable}, calls: make(map[catalog.FileID]int)})
		session := capacitySession(t, store, "ordinary-panic-session", limits)
		lease, err := store.OpenRevision(context.Background(), fileID, session)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.EndLease(lease.ID(), LeaseRelinquished); err == nil {
			t.Fatal("ordinary close panic was not reported")
		}
		snapshot := store.CapacitySnapshot()
		if stable.closed.Load() != 1 || snapshot.Process().Used() != (revisioncapacity.CapacityUsage{StableHandles: 1}) ||
			snapshot.Process().QuarantinedStableHandles() != 1 || snapshot.Share().QuarantinedStableHandles() != 1 ||
			session.Snapshot().Used() != (revisioncapacity.CapacityUsage{}) {
			t.Fatalf("ordinary panic ownership snapshot=%+v closes=%d session=%+v", snapshot, stable.closed.Load(), session.Snapshot())
		}
	})

	t.Run("diagnostic error is terminal", func(t *testing.T) {
		clock := &testClock{now: time.Unix(100, 0)}
		sentinel := errors.New("diagnostic close failure")
		traces := &capacityTraceRecorder{}
		owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 4}, traces)
		limits := revisioncapacity.CapacityLimits{StableHandles: 2, ActiveLeases: 4}
		victimID, victimRecord := capacityRecord(t, 61)
		requestID, requestRecord := capacityRecord(t, 62)
		victim := &capacityStableFile{data: []byte{1}, closeErr: sentinel}
		requester := &capacityStableFile{data: []byte{2}}
		victimStore := newCapacityTestStore(t, owner, "error-victim", "error-a", 1, limits, clock,
			map[catalog.NodeID]catalog.NodeRecord{victimID.NodeID(): victimRecord},
			&mappedRevisionSource{files: map[catalog.FileID]StableFile{victimID: victim}, calls: make(map[catalog.FileID]int)})
		requestStore := newCapacityTestStore(t, owner, "error-request", "error-b", 2, limits, clock,
			map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord},
			&mappedRevisionSource{files: map[catalog.FileID]StableFile{requestID: requester}, calls: make(map[catalog.FileID]int)})
		victimSession := capacitySession(t, victimStore, "error-victim-session", limits)
		requestSession := capacitySession(t, requestStore, "error-request-session", limits)
		lease, err := victimStore.OpenRevision(context.Background(), victimID, victimSession)
		if err != nil {
			t.Fatal(err)
		}
		if err := victimStore.EndLease(lease.ID(), LeaseDetached); err != nil {
			t.Fatal(err)
		}
		if _, err := requestStore.OpenRevision(context.Background(), requestID, requestSession); err != nil {
			t.Fatalf("diagnostic Close prevented handoff: %v", err)
		}
		foundDiagnostic := false
		for _, event := range traces.snapshot() {
			if event.Stage() == revisioncapacity.TraceReclaimCompleted && errors.Is(event.Diagnostic(), sentinel) {
				foundDiagnostic = true
			}
		}
		if !foundDiagnostic || requestStore.CapacitySnapshot().Process().QuarantinedStableHandles() != 0 {
			t.Fatalf("diagnostic completion trace=%v snapshot=%+v", foundDiagnostic, requestStore.CapacitySnapshot())
		}
	})

	t.Run("panic quarantines and denies", func(t *testing.T) {
		clock := &testClock{now: time.Unix(100, 0)}
		traces := &capacityTraceRecorder{}
		owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 4}, traces)
		limits := revisioncapacity.CapacityLimits{StableHandles: 2, ActiveLeases: 4}
		victimID, victimRecord := capacityRecord(t, 71)
		requestID, requestRecord := capacityRecord(t, 72)
		victim := &capacityStableFile{data: []byte{1}, closePanic: true}
		requestSource := &mappedRevisionSource{
			files: map[catalog.FileID]StableFile{requestID: &capacityStableFile{data: []byte{2}}}, calls: make(map[catalog.FileID]int),
		}
		victimStore := newCapacityTestStore(t, owner, "panic-victim", "panic-a", 1, limits, clock,
			map[catalog.NodeID]catalog.NodeRecord{victimID.NodeID(): victimRecord},
			&mappedRevisionSource{files: map[catalog.FileID]StableFile{victimID: victim}, calls: make(map[catalog.FileID]int)})
		requestStore := newCapacityTestStore(t, owner, "panic-request", "panic-b", 2, limits, clock,
			map[catalog.NodeID]catalog.NodeRecord{requestID.NodeID(): requestRecord}, requestSource)
		victimSession := capacitySession(t, victimStore, "panic-victim-session", limits)
		requestSession := capacitySession(t, requestStore, "panic-request-session", limits)
		lease, err := victimStore.OpenRevision(context.Background(), victimID, victimSession)
		if err != nil {
			t.Fatal(err)
		}
		if err := victimStore.EndLease(lease.ID(), LeaseDetached); err != nil {
			t.Fatal(err)
		}
		var ownership *revisioncapacity.ReclaimOwnershipError
		if _, err := requestStore.OpenRevision(context.Background(), requestID, requestSession); !errors.As(err, &ownership) {
			t.Fatalf("panic reclaim error=%v", err)
		}
		snapshot := requestStore.CapacitySnapshot()
		if snapshot.Process().Used() != (revisioncapacity.CapacityUsage{StableHandles: 1}) ||
			snapshot.Process().QuarantinedStableHandles() != 1 || requestSource.callCount(requestID) != 0 {
			t.Fatalf("panic quarantine snapshot=%+v requester opens=%d", snapshot, requestSource.callCount(requestID))
		}
		found := false
		for _, event := range traces.snapshot() {
			if event.Stage() == revisioncapacity.TraceReclaimQuarantined && event.DecisionID() != "" && event.ClaimID() != "" {
				found = true
			}
		}
		if !found {
			t.Fatal("panic quarantine lacked structured decision/claim trace")
		}
	})
}

func TestRevisionStoreRegistrationAndCloseRace(t *testing.T) {
	owner := newCapacityTestOwner(t, revisioncapacity.CapacityLimits{StableHandles: 100, ActiveLeases: 100}, nil)
	store := newCapacityTestStore(t, owner, "race-store", "race-share", 1,
		revisioncapacity.CapacityLimits{StableHandles: 100, ActiveLeases: 100}, nil,
		map[catalog.NodeID]catalog.NodeRecord{}, &mappedRevisionSource{files: map[catalog.FileID]StableFile{}, calls: make(map[catalog.FileID]int)})
	start := make(chan struct{})
	var wait sync.WaitGroup
	var unexpected atomic.Value
	for index := range 64 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, err := store.RegisterSession(revisioncapacity.SessionConfig{
				SessionID: revisioncapacity.SessionID(fmt.Sprintf("race-%d", index)),
				Limits:    revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 1},
			})
			if err != nil && !errors.Is(err, ErrRevisionStoreClosed) &&
				!errors.Is(err, revisioncapacity.ErrRegistrationClosing) {
				unexpected.Store(err)
			}
		}(index)
	}
	close(start)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	if value := unexpected.Load(); value != nil {
		t.Fatalf("registration/close race=%v", value)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("owner retained raced registration: %v", err)
	}
}
