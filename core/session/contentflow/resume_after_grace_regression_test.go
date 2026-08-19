package contentflow

import (
	"bytes"
	"context"
	"crypto/ed25519"
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

func TestResumeAfterGraceReusesRevisionAndCachedPrefixAcrossSenderServices(t *testing.T) {
	share := flowID[catalog.ShareInstance](201)
	file := flowID[catalog.FileID](202)
	parent := flowID[catalog.DirectoryID](203)
	chunkSize := uint32(catalog.MinChunkSize)
	exactSize := uint64(chunkSize) * 2
	data := make([]byte, exactSize)
	for index := range data {
		data[index] = byte(index / int(chunkSize))
	}
	locator, err := catalog.NewLocator(0, "resume.bin")
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
	modified := catalog.ModifiedTime{}
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

	processQuota := quotaAccount(t, "resume-after-grace-process")
	shareQuota := quotaAccount(t, "resume-after-grace-share")
	firstSessionQuota := quotaAccount(t, "resume-after-grace-session-1")
	secondSessionQuota := quotaAccount(t, "resume-after-grace-session-2")
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
	revisionDeriver, err := content.NewHMACRevisionIdentityDeriver(content.RevisionIdentityKey{0x91})
	if err != nil {
		t.Fatal(err)
	}
	metadataBudget, err := content.NewRevisionMetadataBudget(content.DefaultRevisionInvalidationEntries)
	if err != nil {
		t.Fatal(err)
	}
	store, err := content.NewRevisionStore(content.RevisionStoreConfig{
		ShareInstance:    share,
		ChunkSize:        chunkSize,
		Catalog:          runtimeCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		Source:           source,
		ProcessQuota:     processQuota,
		ShareQuota:       shareQuota,
		Clock:            clock,
		LeaseIDs:         &runtimeIDs{},
		RevisionDeriver:  revisionDeriver,
		MetadataBudget:   metadataBudget,
		CacheInvalidator: cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	readSecret := bytes.Repeat([]byte{0x6a}, content.ReadSecretBytes)
	keys, err := content.NewKeyTree(readSecret, share)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x7b}, ed25519.SeedSize))
	sealer, err := records.NewSealer(records.SealerConfig{
		ShareInstance: share,
		Keys:          keys,
		SigningKey:    privateKey,
		NonceSource:   bytes.NewReader(bytes.Repeat([]byte{0x8c}, records.ObjectNonceBytes*16)),
	})
	if err != nil {
		t.Fatal(err)
	}
	firstService, err := NewSenderService(SenderServiceConfig{
		Store: store, SessionQuota: firstSessionQuota, Sealer: sealer, Cache: cache,
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
		revisionDeriver.Destroy()
		keys.Destroy()
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
	requireResumeQuotaUsage(t, processQuota, content.QuotaUsage{StableHandles: 1, ActiveLeases: 1})
	requireResumeQuotaUsage(t, shareQuota, content.QuotaUsage{StableHandles: 1, ActiveLeases: 1})
	requireResumeQuotaUsage(t, firstSessionQuota, content.QuotaUsage{StableHandles: 1, ActiveLeases: 1})

	checkpointPrefix, err := content.NewRangeSet([]content.Range{{Offset: 0, End: uint64(chunkSize)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := firstService.Release(firstLease.ID()); err != nil {
		t.Fatal(err)
	}
	requireResumeQuotaUsage(t, processQuota, content.QuotaUsage{StableHandles: 1})
	requireResumeQuotaUsage(t, shareQuota, content.QuotaUsage{StableHandles: 1})
	requireResumeQuotaUsage(t, firstSessionQuota, content.QuotaUsage{})

	clock.Advance(content.RevisionResumeGrace + time.Nanosecond)
	// A validation of the released lease is only a deterministic maintenance
	// trigger: it proves grace cleanup independently, before a replacement open
	// can reserve the same quota slots and obscure whether the old handle drained.
	if err := store.ValidateLease(firstLease.ID(), firstLease.Descriptor()); !errors.Is(err, content.ErrLeaseExpired) {
		t.Fatalf("released lease after grace error=%v", err)
	}
	if opens, closes, reads := source.snapshot(); opens != 1 || closes != 1 || !slices.Equal(reads, []uint64{0}) {
		t.Fatalf("post-grace source lifecycle opens=%d closes=%d reads=%v", opens, closes, reads)
	}
	requireResumeQuotaUsage(t, processQuota, content.QuotaUsage{})
	requireResumeQuotaUsage(t, shareQuota, content.QuotaUsage{})
	if cache.UsedBytes() == 0 {
		t.Fatal("clean grace release revoked cached progress")
	}

	secondService, err = NewSenderService(SenderServiceConfig{
		Store: store, SessionQuota: secondSessionQuota, Sealer: sealer, Cache: cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	resumeRequest, err := NewOpenRequest([]OpenItem{{FileID: file, InitialRanges: checkpointPrefix}})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := secondService.Open(context.Background(), resumeRequest)
	if err != nil {
		t.Fatal(err)
	}
	resumedItems := resumed.Items()
	if len(resumedItems) != 1 || resumedItems[0].Failure != nil {
		t.Fatalf("resume open results=%+v", resumedItems)
	}
	secondLease := resumedItems[0].Lease
	if secondLease.ID() == firstLease.ID() {
		t.Error("resume reused the released lease identity")
	}
	if secondLease.Descriptor().FileRevision() != firstLease.Descriptor().FileRevision() {
		t.Errorf(
			"identical frozen evidence changed revision after grace: first=%x second=%x",
			firstLease.Descriptor().FileRevision(),
			secondLease.Descriptor().FileRevision(),
		)
	}
	if opens, closes, reads := source.snapshot(); opens != 2 || closes != 1 || !slices.Equal(reads, []uint64{0}) {
		t.Errorf("reopen source lifecycle opens=%d closes=%d reads=%v", opens, closes, reads)
	}
	requireResumeQuotaUsage(t, processQuota, content.QuotaUsage{StableHandles: 1, ActiveLeases: 1})
	requireResumeQuotaUsage(t, shareQuota, content.QuotaUsage{StableHandles: 1, ActiveLeases: 1})
	requireResumeQuotaUsage(t, secondSessionQuota, content.QuotaUsage{StableHandles: 1, ActiveLeases: 1})

	cachedBytes := cache.UsedBytes()
	resumedPrefix, err := NewBlockRequest(secondLease.ID(), []uint64{0})
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
		t.Errorf("new lease could not reuse cached prefix authority; source reads=%v", reads)
	}
	if cache.UsedBytes() != cachedBytes {
		t.Errorf("cached prefix was replaced instead of reused: before=%d after=%d", cachedBytes, cache.UsedBytes())
	}

	continuation, err := NewBlockRequest(secondLease.ID(), []uint64{1})
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
}

func requireResumeQuotaUsage(t *testing.T, account *content.QuotaAccount, expected content.QuotaUsage) {
	t.Helper()
	if actual := account.Snapshot().Used; actual != expected {
		t.Fatalf("quota %q usage=%+v, want %+v", account.Name(), actual, expected)
	}
}
