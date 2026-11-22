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
	"github.com/windshare/windshare/core/content/revisioncapacity"
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

	capacityOwner, err := revisioncapacity.NewProcessOwner(revisioncapacity.DefaultProcessConfig())
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
		ShareInstance:       share,
		ChunkSize:           chunkSize,
		Catalog:             runtimeCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		Source:              source,
		CapacityCoordinator: capacityOwner.Coordinator(),
		CapacityStore: revisioncapacity.StoreConfig{
			StoreID: "resume-after-grace-store", ShareID: "resume-after-grace-share",
			Limits: revisioncapacity.DefaultShareLimits(),
		},
		Clock:            clock,
		LeaseIDs:         &runtimeIDs{},
		CacheInvalidator: cache,
	})
	if err != nil {
		_ = capacityOwner.Close()
		t.Fatal(err)
	}
	firstSessionCapacity, err := store.RegisterSession(revisioncapacity.SessionConfig{
		SessionID: "resume-after-grace-session-1", Limits: revisioncapacity.DefaultSessionLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondSessionCapacity, err := store.RegisterSession(revisioncapacity.SessionConfig{
		SessionID: "resume-after-grace-session-2", Limits: revisioncapacity.DefaultSessionLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sealer := records.Encoder{}
	firstService, err := NewSenderService(SenderServiceConfig{
		Store: store, SessionCapacity: firstSessionCapacity, Encoder: sealer, Cache: cache,
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
		_ = capacityOwner.Close()
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
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Process(), revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1})
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Share(), revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1})
	requireResumeCapacityUsage(t, firstSessionCapacity.Snapshot(), revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1})

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
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Process(), revisioncapacity.CapacityUsage{StableHandles: 1})
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Share(), revisioncapacity.CapacityUsage{StableHandles: 1})
	requireResumeCapacityUsage(t, firstSessionCapacity.Snapshot(), revisioncapacity.CapacityUsage{})

	secondService, err = NewSenderService(SenderServiceConfig{
		Store: store, SessionCapacity: secondSessionCapacity, Encoder: sealer, Cache: cache,
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
	// the old handle and assigns a fresh FileRevision whose SourceMetadata still
	// authorizes reuse.
	if err := secondService.Release(graceLease.ID()); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateLease(graceLease.ID(), graceLease.Descriptor()); !errors.Is(err, content.ErrLeaseExpired) {
		t.Fatalf("relinquished lease validation error=%v", err)
	}
	requireResumeCapacityUsage(t, secondSessionCapacity.Snapshot(), revisioncapacity.CapacityUsage{})
	clock.Advance(content.RevisionResumeGrace)

	reopened, err := secondService.Open(context.Background(), resumeRequest)
	if err != nil {
		t.Fatal(err)
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
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Process(), revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1})
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Share(), revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1})
	requireResumeCapacityUsage(t, secondSessionCapacity.Snapshot(), revisioncapacity.CapacityUsage{StableHandles: 1, ActiveLeases: 1})

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
	if err := secondService.Release(reopenedLease.ID()); err != nil {
		t.Fatal(err)
	}
	if opens, closes, reads := source.snapshot(); opens != 2 || closes != 2 || !slices.Equal(reads, []uint64{0, uint64(chunkSize)}) {
		t.Fatalf("post-relinquishment source lifecycle opens=%d closes=%d reads=%v", opens, closes, reads)
	}
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Process(), revisioncapacity.CapacityUsage{})
	requireResumeCapacityUsage(t, store.CapacitySnapshot().Share(), revisioncapacity.CapacityUsage{})
	requireResumeCapacityUsage(t, secondSessionCapacity.Snapshot(), revisioncapacity.CapacityUsage{})
}

func requireResumeCapacityUsage(t *testing.T, snapshot revisioncapacity.ScopeSnapshot, expected revisioncapacity.CapacityUsage) {
	t.Helper()
	if actual := snapshot.Used(); actual != expected {
		t.Fatalf("capacity %q usage=%+v, want %+v", snapshot.Identity(), actual, expected)
	}
}
