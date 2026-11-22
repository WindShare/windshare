package contentflow

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type resumeAfterGraceSource struct {
	mu       sync.Mutex
	data     []byte
	modified catalog.ModifiedTime
	opens    int
	closes   int
	reads    []uint64
}

func (s *resumeAfterGraceSource) OpenStable(context.Context, catalog.NodeRecord) (content.StableFile, error) {
	s.mu.Lock()
	s.opens++
	s.mu.Unlock()
	return &resumeAfterGraceHandle{source: s}, nil
}

func (s *resumeAfterGraceSource) recordClose() {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()
}

func (s *resumeAfterGraceSource) recordRead(offset uint64) {
	s.mu.Lock()
	s.reads = append(s.reads, offset)
	s.mu.Unlock()
}

func (s *resumeAfterGraceSource) snapshot() (int, int, []uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens, s.closes, slices.Clone(s.reads)
}

type resumeAfterGraceHandle struct {
	source *resumeAfterGraceSource
	closed atomic.Int32
}

func (h *resumeAfterGraceHandle) ExactSize() uint64 {
	return uint64(len(h.source.data))
}

func (h *resumeAfterGraceHandle) ModifiedTime() catalog.ModifiedTime {
	return h.source.modified
}

func (h *resumeAfterGraceHandle) Verify(context.Context) error {
	if h.closed.Load() != 0 {
		return content.ErrRevisionUnreadable
	}
	return nil
}

func (h *resumeAfterGraceHandle) ReadAt(_ context.Context, destination []byte, offset uint64) (int, error) {
	if h.closed.Load() != 0 {
		return 0, content.ErrRevisionUnreadable
	}
	h.source.recordRead(offset)
	if offset >= uint64(len(h.source.data)) {
		return 0, io.EOF
	}
	count := copy(destination, h.source.data[offset:])
	if count != len(destination) {
		return count, io.EOF
	}
	return count, nil
}

func (h *resumeAfterGraceHandle) Close() error {
	if h.closed.CompareAndSwap(0, 1) {
		h.source.recordClose()
	}
	return nil
}

func TestResumeWithinGraceReusesResidentCacheAndReopenMatchesMetadata(t *testing.T) {
	share := flowID[catalog.ShareInstance](201)
	file := flowID[catalog.FileID](202)
	parent := flowID[catalog.DirectoryID](203)
	chunkSize := uint32(catalog.MinChunkSize)
	exactSize := uint64(chunkSize) * 2
	data := make([]byte, exactSize)
	for index := range data {
		data[index] = byte(index / int(chunkSize))
	}
	locator, err := catalog.NewSourceReference([]byte("object:" + "resume.bin"))
	if err != nil {
		t.Fatal(err)
	}
	sourceIdentity, err := catalog.NewSourceIdentity([]byte("resume-after-grace-source"))
	if err != nil {
		t.Fatal(err)
	}
	versionCandidate, err := catalog.NewVersionCandidate([]byte("resume-after-grace-version"))
	if err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(1_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	record, err := catalog.NewFileNodeRecord(
		file,
		parent,
		"resume.bin",
		locator,
		sourceIdentity,
		versionCandidate,
		exactSize,
		modified,
	)
	if err != nil {
		t.Fatal(err)
	}

	pool, err := content.NewHandlePool(content.DefaultHandlePoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	processCache, err := NewProcessCacheBudget(uint64(chunkSize) * 4)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := NewSharedBlockCache(share, uint64(chunkSize)*4, processCache)
	if err != nil {
		t.Fatal(err)
	}
	clock := &runtimeClock{now: time.Unix(10_000, 0)}
	source := &resumeAfterGraceSource{data: data, modified: modified}
	store, err := content.NewRevisionStore(content.RevisionStoreConfig{
		ShareInstance: share,
		ChunkSize:     chunkSize,
		Catalog:       runtimeCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		Source:        source,
		HandlePool:    pool,
		ShareLimits:   content.DefaultShareCapacityLimits(),
		Clock:         clock,
		LeaseIDs:      &runtimeIDs{},
		Cache:         cache,
	})
	if err != nil {
		_ = pool.Close()
		t.Fatal(err)
	}
	firstSession, err := store.RegisterSession(content.SessionConfig{
		SessionID: "resume-after-grace-session-1", Limits: content.DefaultSessionCapacityLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondSession, err := store.RegisterSession(content.SessionConfig{
		SessionID: "resume-after-grace-session-2", Limits: content.DefaultSessionCapacityLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sealer := records.Encoder{}
	firstService, err := NewSenderService(SenderServiceConfig{
		Store: firstSession, Encoder: sealer, Cache: cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	var secondService *SenderService
	t.Cleanup(func() {
		if secondService != nil {
			_ = secondService.Close()
		}
		_ = firstService.Close()
		cache.Close()
		_ = store.Close()
		_ = pool.Close()
	})

	opened, err := firstService.Open(context.Background(), mustOpenRequest(t, file))
	if err != nil {
		t.Fatal(err)
	}
	openedItems := opened.Items()
	if len(openedItems) != 1 || openedItems[0].Failure != nil {
		t.Fatalf("initial open results=%+v", openedItems)
	}
	firstLease := openedItems[0].Lease
	firstBlock, err := NewBlockRequest(firstLease.ID(), []uint64{0})
	if err != nil {
		t.Fatal(err)
	}
	if delivered, serveErr := firstService.ServeBlocks(
		context.Background(),
		flowID[protocolsession.OperationID](204),
		firstBlock,
		func(context.Context, protocolsession.Message) error { return nil },
	); serveErr != nil || delivered != 1 {
		t.Fatalf("initial prefix delivery count=%d err=%v", delivered, serveErr)
	}
	if opens, closes, reads := source.snapshot(); opens != 1 || closes != 0 || !slices.Equal(reads, []uint64{0}) {
		t.Fatalf("initial source lifecycle opens=%d closes=%d reads=%v", opens, closes, reads)
	}
	if cache.UsedBytes() == 0 {
		t.Fatal("partial transfer did not populate the shared block cache")
	}
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Process(), content.CapacityUsage{StableHandles: 1, ActiveLeases: 1})
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Share(), content.CapacityUsage{StableHandles: 1, ActiveLeases: 1})
	requireResumeCapacityUsage(t, firstSession.Snapshot(), content.CapacityUsage{StableHandles: 1, ActiveLeases: 1})

	// Closing the session detaches its lease so the open handle stays resident
	// for RevisionResumeGrace and preserves both FileRevision and cache hits.
	if err := firstService.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateLease(firstLease.ID(), firstLease.Descriptor()); !errors.Is(err, content.ErrLeaseExpired) {
		t.Fatalf("detached lease validation error=%v", err)
	}
	if opens, closes, reads := source.snapshot(); opens != 1 || closes != 0 || !slices.Equal(reads, []uint64{0}) {
		t.Fatalf("detached grace lifecycle opens=%d closes=%d reads=%v", opens, closes, reads)
	}
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Process(), content.CapacityUsage{StableHandles: 1})
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Share(), content.CapacityUsage{StableHandles: 1})
	requireResumeCapacityUsage(t, firstSession.Snapshot(), content.CapacityUsage{})

	secondService, err = NewSenderService(SenderServiceConfig{
		Store: secondSession, Encoder: sealer, Cache: cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpointPrefix, err := content.NewRangeSet([]content.Range{{Offset: 0, End: uint64(chunkSize)}})
	if err != nil {
		t.Fatal(err)
	}
	resumeRequest, err := NewOpenRequest([]OpenItem{{FileID: file, InitialRanges: checkpointPrefix}})
	if err != nil {
		t.Fatal(err)
	}
	graceResumed, err := secondService.Open(context.Background(), resumeRequest)
	if err != nil {
		t.Fatal(err)
	}
	graceItems := graceResumed.Items()
	if len(graceItems) != 1 || graceItems[0].Failure != nil {
		t.Fatalf("grace resume open results=%+v", graceItems)
	}
	graceLease := graceItems[0].Lease
	if graceLease.ID() == firstLease.ID() {
		t.Error("grace resume reused the detached lease identity")
	}
	if graceLease.Descriptor().FileRevision() != firstLease.Descriptor().FileRevision() {
		t.Errorf(
			"resident grace handle changed revision: first=%x grace=%x",
			firstLease.Descriptor().FileRevision(),
			graceLease.Descriptor().FileRevision(),
		)
	}
	if opens, closes, reads := source.snapshot(); opens != 1 || closes != 0 || !slices.Equal(reads, []uint64{0}) {
		t.Errorf("grace resume source lifecycle opens=%d closes=%d reads=%v", opens, closes, reads)
	}
	cachedBytes := cache.UsedBytes()
	resumedPrefix, err := NewBlockRequest(graceLease.ID(), []uint64{0})
	if err != nil {
		t.Fatal(err)
	}
	if delivered, serveErr := secondService.ServeBlocks(
		context.Background(),
		flowID[protocolsession.OperationID](205),
		resumedPrefix,
		func(context.Context, protocolsession.Message) error { return nil },
	); serveErr != nil || delivered != 1 {
		t.Fatalf("cached prefix delivery count=%d err=%v", delivered, serveErr)
	}
	if _, _, reads := source.snapshot(); !slices.Equal(reads, []uint64{0}) {
		t.Errorf("grace resume could not reuse cached prefix; source reads=%v", reads)
	}
	if cache.UsedBytes() != cachedBytes {
		t.Errorf("cached prefix was replaced instead of reused: before=%d after=%d", cachedBytes, cache.UsedBytes())
	}

	// Releasing graceLease leaves the earlier detached session's recovery grace
	// intact until RevisionResumeGrace elapses; once expired, reopening retires
	// the old handle (and reclaims its cached blocks) and assigns a fresh
	// FileRevision whose SourceMetadata still authorizes reuse.
	if err := secondService.Release(graceLease.ID()); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateLease(graceLease.ID(), graceLease.Descriptor()); !errors.Is(err, content.ErrLeaseExpired) {
		t.Fatalf("relinquished lease validation error=%v", err)
	}
	if cache.UsedBytes() != cachedBytes || processCache.Used() != cachedBytes {
		t.Fatalf("grace-period cache was prematurely evicted: cache=%d process=%d want=%d", cache.UsedBytes(), processCache.Used(), cachedBytes)
	}
	requireResumeCapacityUsage(t, secondSession.Snapshot(), content.CapacityUsage{})
	clock.Advance(content.RevisionResumeGrace)

	reopened, err := secondService.Open(context.Background(), resumeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if cache.UsedBytes() != 0 || processCache.Used() != 0 {
		t.Fatalf("expired grace revision retained cached blocks after reopen: cache=%d process=%d", cache.UsedBytes(), processCache.Used())
	}
	reopenedItems := reopened.Items()
	if len(reopenedItems) != 1 || reopenedItems[0].Failure != nil {
		t.Fatalf("reopen results=%+v", reopenedItems)
	}
	reopenedLease := reopenedItems[0].Lease
	if reopenedLease.ID() == graceLease.ID() {
		t.Error("reopen reused the released lease identity")
	}
	if reopenedLease.Descriptor().FileRevision() == firstLease.Descriptor().FileRevision() {
		t.Errorf("reopened handle reused FileRevision %x", reopenedLease.Descriptor().FileRevision())
	}
	comparison := content.CompareRevisions(firstLease.Descriptor(), reopenedLease.Descriptor())
	if !comparison.AllowsReuse() || comparison.Reason != content.ComparisonMetadataAgreement {
		t.Fatalf("reopened comparison = %+v, want metadata agreement reuse", comparison)
	}
	if opens, closes, reads := source.snapshot(); opens != 2 || closes != 1 || !slices.Equal(reads, []uint64{0}) {
		t.Errorf("reopen source lifecycle opens=%d closes=%d reads=%v", opens, closes, reads)
	}
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Process(), content.CapacityUsage{StableHandles: 1, ActiveLeases: 1})
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Share(), content.CapacityUsage{StableHandles: 1, ActiveLeases: 1})
	requireResumeCapacityUsage(t, secondSession.Snapshot(), content.CapacityUsage{StableHandles: 1, ActiveLeases: 1})

	continuation, err := NewBlockRequest(reopenedLease.ID(), []uint64{1})
	if err != nil {
		t.Fatal(err)
	}
	if delivered, serveErr := secondService.ServeBlocks(
		context.Background(),
		flowID[protocolsession.OperationID](206),
		continuation,
		func(context.Context, protocolsession.Message) error { return nil },
	); serveErr != nil || delivered != 1 {
		t.Fatalf("checkpoint continuation count=%d err=%v", delivered, serveErr)
	}
	if _, _, reads := source.snapshot(); !slices.Equal(reads, []uint64{0, uint64(chunkSize)}) {
		t.Errorf("checkpoint resume re-requested verified progress; source reads=%v", reads)
	}
	if cache.UsedBytes() == 0 || processCache.Used() == 0 {
		t.Fatalf("reopened continuation did not populate cache: cache=%d process=%d", cache.UsedBytes(), processCache.Used())
	}
	if err := secondService.Release(reopenedLease.ID()); err != nil {
		t.Fatal(err)
	}
	if opens, closes, reads := source.snapshot(); opens != 2 || closes != 2 || !slices.Equal(reads, []uint64{0, uint64(chunkSize)}) {
		t.Fatalf("post-relinquishment source lifecycle opens=%d closes=%d reads=%v", opens, closes, reads)
	}
	if cache.UsedBytes() != 0 || processCache.Used() != 0 {
		t.Fatalf("cleanly retired revision retained cache budget: cache=%d process=%d", cache.UsedBytes(), processCache.Used())
	}
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Process(), content.CapacityUsage{})
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Share(), content.CapacityUsage{})
	requireResumeCapacityUsage(t, secondSession.Snapshot(), content.CapacityUsage{})
}

func TestCrossShareReclaimFreesRetiredCacheForPeerShareBudget(t *testing.T) {
	chunkSize := uint32(catalog.MinChunkSize)
	data := make([]byte, chunkSize)
	for index := range data {
		data[index] = byte(index)
	}
	modified, err := catalog.NewModifiedTime(2_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	makeRecord := func(file catalog.FileID, parent catalog.DirectoryID, name string) catalog.NodeRecord {
		locator, locErr := catalog.NewSourceReference([]byte("object:" + name))
		if locErr != nil {
			t.Fatal(locErr)
		}
		identity, idErr := catalog.NewSourceIdentity([]byte("src:" + name))
		if idErr != nil {
			t.Fatal(idErr)
		}
		candidate, candErr := catalog.NewVersionCandidate([]byte("ver:" + name))
		if candErr != nil {
			t.Fatal(candErr)
		}
		record, recErr := catalog.NewFileNodeRecord(file, parent, name, locator, identity, candidate, uint64(chunkSize), modified)
		if recErr != nil {
			t.Fatal(recErr)
		}
		return record
	}

	pool, err := content.NewHandlePool(content.HandlePoolConfig{
		Limits:     content.CapacityLimits{StableHandles: 1, ActiveLeases: 4},
		RetryAfter: content.DefaultCapacityRetryAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Budget accommodates one encoded block (~1 KiB + CBOR envelope) but not two,
	// proving cross-share reclaim frees both the physical handle and cache budget.
	processCache, err := NewProcessCacheBudget(uint64(chunkSize) + 256)
	if err != nil {
		t.Fatal(err)
	}
	clock := &runtimeClock{now: time.Unix(20_000, 0)}

	shareA := flowID[catalog.ShareInstance](211)
	fileA := flowID[catalog.FileID](212)
	recordA := makeRecord(fileA, flowID[catalog.DirectoryID](213), "a.bin")
	cacheA, err := NewSharedBlockCache(shareA, uint64(chunkSize)*4, processCache)
	if err != nil {
		t.Fatal(err)
	}
	storeA, err := content.NewRevisionStore(content.RevisionStoreConfig{
		ShareInstance: shareA,
		ChunkSize:     chunkSize,
		Catalog:       runtimeCatalog{records: map[catalog.NodeID]catalog.NodeRecord{fileA.NodeID(): recordA}},
		Source:        &resumeAfterGraceSource{data: data, modified: modified},
		HandlePool:    pool,
		ShareLimits:   content.CapacityLimits{StableHandles: 1, ActiveLeases: 4},
		Clock:         clock,
		LeaseIDs:      &runtimeIDs{},
		Cache:         cacheA,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionA, err := storeA.RegisterSession(content.SessionConfig{
		SessionID: "reclaim-cache-session-a", Limits: content.CapacityLimits{StableHandles: 1, ActiveLeases: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	serviceA, err := NewSenderService(SenderServiceConfig{
		Store: sessionA, Encoder: records.Encoder{}, Cache: cacheA,
	})
	if err != nil {
		t.Fatal(err)
	}

	shareB := flowID[catalog.ShareInstance](221)
	fileB := flowID[catalog.FileID](222)
	recordB := makeRecord(fileB, flowID[catalog.DirectoryID](223), "b.bin")
	cacheB, err := NewSharedBlockCache(shareB, uint64(chunkSize)*4, processCache)
	if err != nil {
		t.Fatal(err)
	}
	storeB, err := content.NewRevisionStore(content.RevisionStoreConfig{
		ShareInstance: shareB,
		ChunkSize:     chunkSize,
		Catalog:       runtimeCatalog{records: map[catalog.NodeID]catalog.NodeRecord{fileB.NodeID(): recordB}},
		Source:        &resumeAfterGraceSource{data: data, modified: modified},
		HandlePool:    pool,
		ShareLimits:   content.CapacityLimits{StableHandles: 1, ActiveLeases: 4},
		Clock:         clock,
		LeaseIDs:      &runtimeIDs{},
		Cache:         cacheB,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionB, err := storeB.RegisterSession(content.SessionConfig{
		SessionID: "reclaim-cache-session-b", Limits: content.CapacityLimits{StableHandles: 1, ActiveLeases: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	serviceB, err := NewSenderService(SenderServiceConfig{
		Store: sessionB, Encoder: records.Encoder{}, Cache: cacheB,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = serviceB.Close()
		_ = serviceA.Close()
		cacheB.Close()
		cacheA.Close()
		_ = storeB.Close()
		_ = storeA.Close()
		_ = pool.Close()
	})

	openedA, err := serviceA.Open(context.Background(), mustOpenRequest(t, fileA))
	if err != nil {
		t.Fatal(err)
	}
	reqA, err := NewBlockRequest(openedA.Items()[0].Lease.ID(), []uint64{0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serviceA.ServeBlocks(context.Background(), flowID[protocolsession.OperationID](230), reqA, func(context.Context, protocolsession.Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	bytesA := cacheA.UsedBytes()
	if bytesA == 0 || processCache.Used() != bytesA {
		t.Fatalf("share A cache=%d process=%d", bytesA, processCache.Used())
	}

	// Closing serviceA detaches its lease into grace; cache remains warm until reclaimed.
	if err := serviceA.Close(); err != nil {
		t.Fatal(err)
	}
	if cacheA.UsedBytes() != bytesA || processCache.Used() != bytesA {
		t.Fatalf("detached grace evicted share A cache: cache=%d process=%d", cacheA.UsedBytes(), processCache.Used())
	}

	// Opening on shareB reclaims shareA's idle handle and must retire shareA's cache.
	openedB, err := serviceB.Open(context.Background(), mustOpenRequest(t, fileB))
	if err != nil {
		t.Fatal(err)
	}
	if cacheA.UsedBytes() != 0 || processCache.Used() != 0 {
		t.Fatalf("reclaimed share A retained cache budget: cacheA=%d process=%d", cacheA.UsedBytes(), processCache.Used())
	}
	reqB, err := NewBlockRequest(openedB.Items()[0].Lease.ID(), []uint64{0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serviceB.ServeBlocks(context.Background(), flowID[protocolsession.OperationID](231), reqB, func(context.Context, protocolsession.Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if cacheB.UsedBytes() == 0 || processCache.Used() != cacheB.UsedBytes() {
		t.Fatalf("share B could not use reclaimed process budget: cacheB=%d process=%d", cacheB.UsedBytes(), processCache.Used())
	}
	if err := serviceB.Release(openedB.Items()[0].Lease.ID()); err != nil {
		t.Fatal(err)
	}
	if cacheB.UsedBytes() != 0 || processCache.Used() != 0 {
		t.Fatalf("share B release retained cache budget: cacheB=%d process=%d", cacheB.UsedBytes(), processCache.Used())
	}
}

func requireResumeCapacityUsage(t *testing.T, snapshot content.ScopeSnapshot, expected content.CapacityUsage) {
	t.Helper()
	if actual := snapshot.Used(); actual != expected {
		t.Fatalf("capacity %q usage=%+v, want %+v", snapshot.Identity(), actual, expected)
	}
}
