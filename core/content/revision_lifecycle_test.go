package content

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
)

func fileRecordWithModifiedTime(t *testing.T, fileByte byte, size uint64, modified catalog.ModifiedTime) (catalog.FileID, catalog.NodeRecord) {
	t.Helper()
	file := catalogID[catalog.FileID](fileByte)
	parent := catalogID[catalog.DirectoryID](8)
	locator, err := catalog.NewLocator(0, "file")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := catalog.NewSourceIdentity([]byte("identity"))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := catalog.NewVersionCandidate([]byte("candidate"))
	if err != nil {
		t.Fatal(err)
	}
	record, err := catalog.NewFileNodeRecord(file, parent, "file.bin", locator, identity, candidate, size, modified)
	if err != nil {
		t.Fatal(err)
	}
	return file, record
}

func TestModifiedTimeMismatchPermanentlyInvalidatesRestoredEvidence(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	frozenModified, _ := catalog.NewModifiedTime(50, 123_000_000, catalog.TimePrecisionMilliseconds)
	driftedModified, _ := catalog.NewModifiedTime(51, 123_000_000, catalog.TimePrecisionMilliseconds)
	file, record := fileRecordWithModifiedTime(t, 9, 1, frozenModified)
	first := &testStableFile{data: []byte{1}, modified: frozenModified}
	mismatch := &testStableFile{data: []byte{1}, modified: driftedModified}
	restored := &testStableFile{data: []byte{1}, modified: frozenModified}
	invalidator := &recordingInvalidator{}
	source := &testRevisionSource{files: []*testStableFile{first, mismatch, restored}}
	store, _, _ := newRevisionStore(t, source, clock, invalidator, file, record)
	session := generousSession(t, store, "session")

	initial, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EndLease(initial.ID(), LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenRevision(context.Background(), file, session); !errors.Is(err, ErrRevisionStale) {
		t.Fatalf("modified-time mismatch=%v", err)
	}
	if _, err := store.OpenRevision(context.Background(), file, session); !errors.Is(err, ErrRevisionDrift) {
		t.Fatalf("restored evidence reopened invalidated revision=%v", err)
	}
	if source.Calls() != 2 || len(invalidator.revisions) != 1 || invalidator.revisions[0] != initial.Descriptor().FileRevision() {
		t.Fatalf("invalidation boundary calls=%d invalidations=%v", source.Calls(), invalidator.revisions)
	}
	if got := store.metadataBudget.Snapshot().Used; got != 1 {
		t.Fatalf("lossless invalidation metadata=%d", got)
	}
}

func TestUnavailableReopenRetainsReleasedRevisionForExactRetry(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, 1)
	initial := &testStableFile{data: []byte{1}}
	retried := &testStableFile{data: []byte{1}}
	source := &testRevisionSource{files: []*testStableFile{initial, nil, retried}}
	invalidator := &recordingInvalidator{}
	store, _, _ := newRevisionStore(t, source, clock, invalidator, file, record)
	session := generousSession(t, store, "session")

	first, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.EndLease(first.ID(), LeaseRelinquished)
	if _, err := store.OpenRevision(context.Background(), file, session); err == nil || errors.Is(err, ErrRevisionDrift) {
		t.Fatalf("unavailable reopen=%v", err)
	}
	second, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if second.Descriptor().FileRevision() != first.Descriptor().FileRevision() || second.ID() == first.ID() {
		t.Fatalf("retry identity first=%+v second=%+v", first, second)
	}
	if len(invalidator.revisions) != 0 || store.metadataBudget.Snapshot().Used != 0 {
		t.Fatal("unavailable reopen mutated invalidation authority")
	}
}

func TestInvalidationMetadataExhaustionStopsFreshRevisionAdmission(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	firstFile, firstRecord := fileRecordWithModifiedTime(t, 9, 1, catalog.ModifiedTime{})
	secondFile, secondRecord := fileRecordWithModifiedTime(t, 10, 1, catalog.ModifiedTime{})
	thirdFile, thirdRecord := fileRecordWithModifiedTime(t, 11, 1, catalog.ModifiedTime{})
	source := &testRevisionSource{files: []*testStableFile{
		{data: []byte{1, 2}}, {data: []byte{1, 2}}, {data: []byte{1}},
	}}
	deriver := testRevisionDeriver(t)
	budget := testRevisionMetadataBudget(t, 1)
	config := RevisionStoreConfig{
		ShareInstance: catalogID[catalog.ShareInstance](1), ChunkSize: catalog.MinChunkSize,
		Catalog: testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{
			firstFile.NodeID(): firstRecord, secondFile.NodeID(): secondRecord, thirdFile.NodeID(): thirdRecord,
		}},
		Source: source, Clock: clock,
		LeaseIDs: &sequenceIDs{}, RevisionDeriver: deriver, MetadataBudget: budget,
	}
	attachTestCapacity(t, &config,
		revisioncapacity.CapacityLimits{StableHandles: 100, ActiveLeases: 100},
		revisioncapacity.CapacityLimits{StableHandles: 100, ActiveLeases: 100},
	)
	store, err := NewRevisionStore(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		deriver.Destroy()
	})
	session := generousSession(t, store, "session")
	if _, err := store.OpenRevision(context.Background(), firstFile, session); !errors.Is(err, ErrRevisionStale) || errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("first invalidation=%v", err)
	}
	if _, err := store.OpenRevision(context.Background(), secondFile, session); !errors.Is(err, ErrRevisionStale) || !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("budget-exhausting invalidation=%v", err)
	}
	if _, err := store.OpenRevision(context.Background(), thirdFile, session); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("fresh admission after metadata exhaustion=%v", err)
	}
	if source.Calls() != 2 || budget.Snapshot().Used != 1 {
		t.Fatalf("fail-closed admission calls=%d budget=%+v", source.Calls(), budget.Snapshot())
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if budget.Snapshot().Used != 0 {
		t.Fatalf("store close retained metadata budget=%+v", budget.Snapshot())
	}
}

type queuedStableSource struct {
	mu         sync.Mutex
	files      []StableFile
	calls      int
	blockIndex int
	started    chan struct{}
	release    chan struct{}
}

func (s *queuedStableSource) OpenStable(ctx context.Context, _ catalog.NodeRecord) (StableFile, error) {
	s.mu.Lock()
	index := s.calls
	s.calls++
	var file StableFile
	if index >= len(s.files) {
		s.mu.Unlock()
		return nil, errors.New("no queued stable source")
	}
	file = s.files[index]
	started, release, blocked := s.started, s.release, s.started != nil && index == s.blockIndex
	s.mu.Unlock()
	if blocked {
		close(started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
	}
	return file, nil
}

func (s *queuedStableSource) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestConcurrentReleasedReopenSingleflightsAndIssuesIndependentLeases(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, 1)
	source := &queuedStableSource{
		files:      []StableFile{&testStableFile{data: []byte{1}}, &testStableFile{data: []byte{1}}},
		blockIndex: 1, started: make(chan struct{}), release: make(chan struct{}),
	}
	store, _, _ := newRevisionStore(t, source, clock, nil, file, record)
	session := generousSession(t, store, "session")
	initial, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.EndLease(initial.ID(), LeaseRelinquished)
	leases := make(chan RevisionLease, 2)
	errorsOut := make(chan error, 2)
	for range 2 {
		go func() {
			lease, openErr := store.OpenRevision(context.Background(), file, session)
			leases <- lease
			errorsOut <- openErr
		}()
	}
	<-source.started
	close(source.release)
	first, second := <-leases, <-leases
	if err := <-errorsOut; err != nil {
		t.Fatal(err)
	}
	if err := <-errorsOut; err != nil {
		t.Fatal(err)
	}
	if source.Calls() != 2 || first.ID() == second.ID() ||
		first.Descriptor().FileRevision() != initial.Descriptor().FileRevision() ||
		second.Descriptor().FileRevision() != initial.Descriptor().FileRevision() {
		t.Fatalf("reopen singleflight calls=%d initial=%+v first=%+v second=%+v", source.Calls(), initial, first, second)
	}
}

type blockingShortStableFile struct {
	started chan struct{}
	release chan struct{}
	closed  int
	closeMu sync.Mutex
}

func (*blockingShortStableFile) ExactSize() uint64                  { return 1 }
func (*blockingShortStableFile) ModifiedTime() catalog.ModifiedTime { return catalog.ModifiedTime{} }
func (*blockingShortStableFile) Verify(context.Context) error       { return nil }
func (f *blockingShortStableFile) ReadAt(context.Context, []byte, uint64) (int, error) {
	close(f.started)
	<-f.release
	return 0, io.EOF
}
func (f *blockingShortStableFile) Close() error {
	f.closeMu.Lock()
	f.closed++
	f.closeMu.Unlock()
	return nil
}

func TestDetachedReaderInvalidatesSameTupleAfterConcurrentReopen(t *testing.T) {
	clock := &testClock{now: time.Unix(100, 0)}
	file, record := fileRecord(t, 1)
	oldSource := &blockingShortStableFile{started: make(chan struct{}), release: make(chan struct{})}
	newSource := &testStableFile{data: []byte{1}}
	source := &queuedStableSource{files: []StableFile{oldSource, newSource}}
	invalidator := &recordingInvalidator{}
	store, _, _ := newRevisionStore(t, source, clock, invalidator, file, record)
	session := generousSession(t, store, "session")
	oldLease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := NewBlockRef(file, oldLease.Descriptor().FileRevision(), 0, oldLease.Descriptor().Geometry())
	readResult := make(chan error, 1)
	go func() {
		_, readErr := store.ReadBlock(context.Background(), oldLease.ID(), ref)
		readResult <- readErr
	}()
	<-oldSource.started
	_ = store.EndLease(oldLease.ID(), LeaseDetached)
	clock.Advance(RevisionResumeGrace)
	newLease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	if newLease.Descriptor().FileRevision() != oldLease.Descriptor().FileRevision() {
		t.Fatal("concurrent reopen changed deterministic revision")
	}
	close(oldSource.release)
	if err := <-readResult; !errors.Is(err, ErrRevisionDrift) {
		t.Fatalf("detached reader drift=%v", err)
	}
	if err := store.ValidateLease(newLease.ID(), newLease.Descriptor()); !errors.Is(err, ErrRevisionDrift) {
		t.Fatalf("new active lease survived tuple-global drift=%v", err)
	}
	if len(invalidator.revisions) != 1 || newSource.closed.Load() != 1 {
		t.Fatalf("tuple invalidation count=%d new source closes=%d", len(invalidator.revisions), newSource.closed.Load())
	}
}

func TestRevisionTracerPanicCannotAffectStoreAuthority(t *testing.T) {
	file, record := fileRecord(t, 1)
	deriver := testRevisionDeriver(t)
	config := RevisionStoreConfig{
		ShareInstance: catalogID[catalog.ShareInstance](1), ChunkSize: catalog.MinChunkSize,
		Catalog:  testCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		Source:   &testRevisionSource{files: []*testStableFile{{data: []byte{1}}}},
		LeaseIDs: &sequenceIDs{}, RevisionDeriver: deriver,
		MetadataBudget: testRevisionMetadataBudget(t, 1),
		Tracer: RevisionTracerFunc(func(event RevisionTrace) {
			if event.ShareInstance().IsZero() || event.FileID().IsZero() || event.Stage() == RevisionTraceStageUnknown {
				t.Errorf("unsafe or incomplete trace=%+v", event)
			}
			panic("tracer failure")
		}),
	}
	attachTestCapacity(t, &config,
		revisioncapacity.CapacityLimits{StableHandles: 100, ActiveLeases: 100},
		revisioncapacity.CapacityLimits{StableHandles: 100, ActiveLeases: 100},
	)
	store, err := NewRevisionStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer deriver.Destroy()
	defer store.Close()
	session := generousSession(t, store, "session")
	if _, err := store.OpenRevision(context.Background(), file, session); err != nil {
		t.Fatalf("tracer panic changed open result=%v", err)
	}
}

type transientReadStableFile struct {
	err    error
	closed int
}

func (*transientReadStableFile) ExactSize() uint64                  { return 1 }
func (*transientReadStableFile) ModifiedTime() catalog.ModifiedTime { return catalog.ModifiedTime{} }
func (*transientReadStableFile) Verify(context.Context) error       { return nil }
func (f *transientReadStableFile) ReadAt(context.Context, []byte, uint64) (int, error) {
	return 0, f.err
}
func (f *transientReadStableFile) Close() error { f.closed++; return nil }

func TestTransientReadFailureDoesNotInvalidateActiveRevision(t *testing.T) {
	file, record := fileRecord(t, 1)
	sentinel := errors.New("transient provider read failure")
	stable := &transientReadStableFile{err: sentinel}
	invalidator := &recordingInvalidator{}
	store, _, _ := newRevisionStore(t, revisionSourceFunc(func(context.Context, catalog.NodeRecord) (StableFile, error) {
		return stable, nil
	}), &testClock{now: time.Unix(100, 0)}, invalidator, file, record)
	session := generousSession(t, store, "session")
	lease, err := store.OpenRevision(context.Background(), file, session)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := NewBlockRef(file, lease.Descriptor().FileRevision(), 0, lease.Descriptor().Geometry())
	if _, err := store.ReadBlock(context.Background(), lease.ID(), ref); !errors.Is(err, sentinel) || errors.Is(err, ErrRevisionDrift) {
		t.Fatalf("transient read=%v", err)
	}
	if err := store.ValidateLease(lease.ID(), lease.Descriptor()); err != nil {
		t.Fatalf("transient read revoked active lease=%v", err)
	}
	if len(invalidator.revisions) != 0 || store.metadataBudget.Snapshot().Used != 0 || stable.closed != 0 {
		t.Fatal("transient read mutated invalidation or handle authority")
	}
}
