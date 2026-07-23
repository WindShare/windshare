package contentflow

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestAssemblerAdmissionTombstoneCapacityAndExpiryEdges(t *testing.T) {
	hierarchy, accounts := reassemblyHierarchy(t, ReassemblyLimits{Bytes: 1 << 20, Records: 4})
	session := flowID[protocolsession.ProtocolSessionID](101)
	if _, err := NewAssembler(protocolsession.ProtocolSessionID{}, hierarchy, nil); err == nil {
		t.Fatal("zero session assembler accepted")
	}
	if _, err := NewAssembler(session, ReassemblyHierarchy{}, nil); err == nil {
		t.Fatal("nil hierarchy assembler accepted")
	}
	if _, err := NewAssembler(session, ReassemblyHierarchy{Process: accounts[0], Share: accounts[0], Session: accounts[2]}, nil); err == nil {
		t.Fatal("duplicate hierarchy assembler accepted")
	}
	now := time.Unix(3_000, 0)
	assembler, _ := NewAssembler(session, hierarchy, func() time.Time { return now })
	operation := flowID[protocolsession.OperationID](102)
	fragments, _ := FragmentRecord(operation, bytes.Repeat([]byte{1}, MaxFragmentPayloadBytes+1))
	if _, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, fragments[0])); err != nil || assembler.ActiveRecords() != 1 {
		t.Fatalf("active records=%d err=%v", assembler.ActiveRecords(), err)
	}
	now = now.Add(FragmentTimeout)
	if _, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, fragments[1])); !errors.Is(err, ErrFragmentTimeout) || assembler.ActiveRecords() != 0 {
		t.Fatalf("implicit timeout active=%d err=%v", assembler.ActiveRecords(), err)
	}
	late, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, fragments[1]))
	if err != nil || late.Status != FragmentTombstoned {
		t.Fatalf("timeout tombstone result=%+v err=%v", late, err)
	}
	now = now.Add(FragmentTombstone)
	accepted, err := assembler.AcceptAuthenticated(flowMessagePlaintext(t, fragments[0]))
	if err != nil || accepted.Status != FragmentAccepted {
		t.Fatalf("expired tombstone retained: %+v %v", accepted, err)
	}
	assembler.Close()

	full, _ := NewAssembler(session, hierarchy, func() time.Time { return now })
	for index := 0; index < MaxFragmentTombstones; index++ {
		full.recordTombstones[assemblyKey{
			operation: flowID[protocolsession.OperationID](byte(index%250 + 1)),
			record:    records.RecordID{byte(index / 250), byte(index%250 + 1)},
		}] = now.Add(time.Hour)
	}
	if err := full.CancelOperation(flowID[protocolsession.OperationID](110)); !errors.Is(err, ErrReassemblyBudget) {
		t.Fatalf("tombstone budget cancel error=%v", err)
	}
	newFragment, _ := FragmentRecord(flowID[protocolsession.OperationID](111), []byte("new"))
	if _, err := full.AcceptAuthenticated(flowMessagePlaintext(t, newFragment[0])); !errors.Is(err, ErrReassemblyBudget) {
		t.Fatalf("tombstone budget allocation error=%v", err)
	}

	if _, err := NewReassemblyAccount("", ReassemblyLimits{Bytes: 1, Records: 1}); err == nil {
		t.Fatal("unnamed account accepted")
	}
	var nilAccount *ReassemblyAccount
	if nilAccount.Usage() != (ReassemblyUsage{}) {
		t.Fatal("nil account reported usage")
	}
	if _, err := reserveReassembly(ReassemblyHierarchy{Process: accounts[0], Share: nil, Session: accounts[2]}, 1); err == nil {
		t.Fatal("nil reservation hierarchy accepted")
	}
	reservation, err := reserveReassembly(hierarchy, 1)
	if err != nil {
		t.Fatal(err)
	}
	reservation.Release()
	reservation.Release()
	var nilReservation *reassemblyReservation
	nilReservation.Release()
}

func canonicalRanges(count int) content.RangeSet {
	ranges := make([]content.Range, count)
	for index := range ranges {
		ranges[index] = content.Range{Offset: uint64(index * 2), End: uint64(index*2 + 1)}
	}
	result, _ := content.NewRangeSet(ranges)
	return result
}

func TestOpenAndResultTypesRejectEveryAmbiguousShape(t *testing.T) {
	file := flowID[catalog.FileID](121)
	if _, err := NewOpenRequest(nil); !errors.Is(err, ErrInvalidOpenRequest) {
		t.Fatalf("empty open error=%v", err)
	}
	tooMany := make([]OpenItem, content.MaxOpenRevisionBatch+1)
	if _, err := NewOpenRequest(tooMany); !errors.Is(err, ErrInvalidOpenRequest) {
		t.Fatalf("oversized open error=%v", err)
	}
	if _, err := NewOpenRequest([]OpenItem{{}}); !errors.Is(err, ErrInvalidOpenRequest) {
		t.Fatalf("zero file open error=%v", err)
	}
	if _, err := NewOpenRequest([]OpenItem{{FileID: file, InitialRanges: canonicalRanges(MaxInitialRangesPerFile + 1)}}); !errors.Is(err, ErrInvalidOpenRequest) {
		t.Fatalf("per-file range error=%v", err)
	}
	manyRanges := canonicalRanges(MaxInitialRangesPerFile)
	if _, err := NewOpenRequest([]OpenItem{
		{FileID: flowID[catalog.FileID](1), InitialRanges: manyRanges},
		{FileID: flowID[catalog.FileID](2), InitialRanges: manyRanges},
		{FileID: flowID[catalog.FileID](3), InitialRanges: manyRanges},
		{FileID: flowID[catalog.FileID](4), InitialRanges: manyRanges},
		{FileID: flowID[catalog.FileID](5), InitialRanges: manyRanges},
	}); !errors.Is(err, ErrInvalidOpenRequest) {
		t.Fatalf("request range total error=%v", err)
	}
	if _, err := SuccessfulOpen(file, content.RevisionLease{}, nil); !errors.Is(err, ErrInvalidOpenResults) {
		t.Fatalf("invalid success error=%v", err)
	}
	badFailure := RevisionFailure{Code: RevisionCodeStale - 1}
	if _, err := FailedOpen(file, badFailure); !errors.Is(err, ErrInvalidOpenResults) {
		t.Fatalf("invalid failed-open error=%v", err)
	}
	if _, err := NewOpenResults(nil); !errors.Is(err, ErrInvalidOpenResults) {
		t.Fatalf("empty results error=%v", err)
	}
	if _, err := NewOpenResults([]OpenResult{{FileID: file}}); !errors.Is(err, ErrInvalidOpenResults) {
		t.Fatalf("missing outcome error=%v", err)
	}
	if _, err := NewRevisionFailure(RevisionCodeStale, false, time.Second); err == nil {
		t.Fatal("permanent failure delay accepted")
	}
}

func TestControlDecodersRejectHostileCanonicalShapes(t *testing.T) {
	file := flowID[catalog.FileID](131)
	lease := flowID[content.LeaseID](132)
	malformedOpen := []any{
		nil,
		[]any{[]any{file.Bytes()}},
		[]any{[]any{make([]byte, 15), []any{}}},
		[]any{[]any{file.Bytes(), []any{[]any{uint64(2), uint64(1)}}}},
	}
	for index, value := range malformedOpen {
		encoded, _ := bodyEncMode.Marshal(value)
		if _, err := DecodeOpenRequest(encoded); err == nil {
			t.Fatalf("hostile open %d accepted", index)
		}
	}
	for index, value := range []any{
		[]any{lease.Bytes()},
		[]any{make([]byte, 15), []uint64{1}},
		[]any{lease.Bytes(), "indices"},
		[]any{lease.Bytes(), []uint64{}},
		[]any{lease.Bytes(), []uint64{2, 1}},
	} {
		encoded, _ := bodyEncMode.Marshal(value)
		if _, err := DecodeBlockRequest(encoded); err == nil {
			t.Fatalf("hostile block %d accepted", index)
		}
	}
	for index, value := range []any{[]any{}, []any{make([]byte, 15)}, []any{lease.Bytes(), lease.Bytes()}} {
		encoded, _ := bodyEncMode.Marshal(value)
		if _, err := DecodeLeaseRequest(encoded); err == nil {
			t.Fatalf("hostile lease %d accepted", index)
		}
	}
	if _, err := EncodeOpenRequest(OpenRequest{}); err == nil {
		t.Fatal("zero open encoded")
	}
	if _, err := EncodeBlockRequest(BlockRequest{}); err == nil {
		t.Fatal("zero block encoded")
	}
	if _, err := EncodeLeaseRequest(content.LeaseID{}); err == nil {
		t.Fatal("zero lease encoded")
	}
	if _, err := DecodeOpenResults([]byte{0xff}, []catalog.FileID{file}); err == nil {
		t.Fatal("noncanonical results accepted")
	}
	if _, err := DecodeOpenResults([]byte{0xa0}, nil); !errors.Is(err, ErrInvalidOpenResults) {
		t.Fatalf("empty expected results error=%v", err)
	}

	retryable := map[uint64]any{0: uint64(1), 1: []any{
		[]any{file.Bytes(), uint64(1), uint64(RevisionCodeQuota), true, uint64(250)},
	}}
	retryableBytes, _ := bodyEncMode.Marshal(retryable)
	decoded, err := DecodeOpenResults(retryableBytes, []catalog.FileID{file})
	if err != nil || decoded[0].Failure == nil || decoded[0].Failure.RetryAfter != 250*time.Millisecond {
		t.Fatalf("retryable results=%+v err=%v", decoded, err)
	}
	for index, mutate := range []func(map[uint64]any){
		func(fields map[uint64]any) { fields[0] = uint64(2) },
		func(fields map[uint64]any) { fields[1] = []any{} },
		func(fields map[uint64]any) { fields[2] = uint64(1) },
	} {
		fields := map[uint64]any{0: uint64(1), 1: retryable[1]}
		mutate(fields)
		encoded, _ := bodyEncMode.Marshal(fields)
		if _, err := DecodeOpenResults(encoded, []catalog.FileID{file}); err == nil {
			t.Fatalf("hostile result envelope %d accepted", index)
		}
	}

	badLeaseResults := []map[uint64]any{
		{0: uint64(2), 1: lease.Bytes(), 2: uint64(1), 3: uint64(0)},
		{0: uint64(1), 1: make([]byte, 15), 2: uint64(1), 3: uint64(0)},
		{0: uint64(1), 1: lease.Bytes(), 2: uint64(0), 3: uint64(0)},
		{0: uint64(1), 1: lease.Bytes(), 2: uint64(1), 3: uint64(2)},
	}
	for index, fields := range badLeaseResults {
		encoded, _ := bodyEncMode.Marshal(fields)
		if _, err := DecodeLeaseResult(encoded, lease); err == nil {
			t.Fatalf("hostile lease result %d accepted", index)
		}
	}
	if _, err := DecodeLeaseResult([]byte{0xff}, lease); err == nil {
		t.Fatal("malformed lease result accepted")
	}
	if _, err := DecodeLeaseResult(retryableBytes, content.LeaseID{}); !errors.Is(err, ErrInvalidLeaseRequest) {
		t.Fatalf("zero expected lease error=%v", err)
	}
	for _, fields := range []map[uint64]any{
		{0: uint64(2), 1: uint64(0)}, {0: uint64(1)}, {0: uint64(1), 1: "count"},
	} {
		encoded, _ := bodyEncMode.Marshal(fields)
		if _, err := DecodeOperationComplete(encoded); err == nil {
			t.Fatalf("hostile completion accepted: %+v", fields)
		}
	}
	if _, err := durationMilliseconds(0); err == nil {
		t.Fatal("zero duration accepted")
	}
	if _, err := durationMillisecondsAllowZero(time.Nanosecond); err == nil {
		t.Fatal("sub-millisecond duration accepted")
	}
}

func TestOpenResultItemDecoderRejectsHostileSuccessAndFailureFields(t *testing.T) {
	file := flowID[catalog.FileID](171)
	lease := flowID[content.LeaseID](172)
	validSuccess := []any{
		file.Bytes(), uint64(0), []byte{1}, lease.Bytes(),
		uint64(RevisionLeaseTTL / time.Millisecond), uint64(RevisionLeaseRenewAfter / time.Millisecond),
	}
	validFailure := []any{file.Bytes(), uint64(1), uint64(RevisionCodeStale), false, nil}
	validRetryableFailure := []any{
		file.Bytes(), uint64(1), uint64(RevisionCodeQuota), true,
		uint64(MaxRevisionFailureRetryAfter / time.Millisecond),
	}
	tests := [][]any{
		{file.Bytes(), uint64(2), []byte{1}, lease.Bytes(), uint64(120_000), uint64(60_000)},
		{file.Bytes(), uint64(0), []byte{}, lease.Bytes(), uint64(120_000), uint64(60_000)},
		{file.Bytes(), uint64(0), []byte{1}, make([]byte, 15), uint64(120_000), uint64(60_000)},
		{file.Bytes(), uint64(0), []byte{1}, lease.Bytes(), uint64(119_999), uint64(60_000)},
		{file.Bytes(), uint64(0), []byte{1}, lease.Bytes(), uint64(120_000), uint64(59_999)},
		{file.Bytes(), uint64(1), uint64(RevisionCodeStale - 1), false, nil},
		{file.Bytes(), uint64(1), uint64(RevisionCodeStale), true, uint64(0)},
		{
			file.Bytes(), uint64(1), uint64(RevisionCodeQuota), true,
			uint64(MaxRevisionFailureRetryAfter/time.Millisecond) + 1,
		},
		{file.Bytes(), uint64(1), uint64(RevisionCodeStale), false, uint64(1)},
		{file.Bytes(), uint64(1), uint64(RevisionCodeStale), "false", nil},
	}
	for index, item := range tests {
		encoded, _ := bodyEncMode.Marshal(map[uint64]any{0: uint64(1), 1: []any{item}})
		if _, err := DecodeOpenResults(encoded, []catalog.FileID{file}); err == nil {
			t.Fatalf("hostile open result item %d accepted", index)
		}
	}
	for _, item := range [][]any{validSuccess, validFailure, validRetryableFailure} {
		encoded, _ := bodyEncMode.Marshal(map[uint64]any{0: uint64(1), 1: []any{item}})
		if _, err := DecodeOpenResults(encoded, []catalog.FileID{file}); err != nil {
			t.Fatalf("valid result rejected: %v", err)
		}
	}
	if _, err := EncodeOpenResults(OpenResults{}); err == nil {
		t.Fatal("zero open results encoded")
	}
	if _, err := NewRevisionFailure(RevisionCodeQuota, true, time.Nanosecond); err == nil {
		t.Fatal("sub-millisecond retry delay was constructed")
	}
	if _, err := EncodeLeaseResult(content.RevisionLease{}); err == nil {
		t.Fatal("zero lease result encoded")
	}
}

type stubRevisionStore struct {
	mu          sync.Mutex
	results     []content.OpenRevisionResult
	openErr     error
	renew       content.RevisionLease
	renewErr    error
	read        []byte
	readErr     error
	validateErr error
	released    []content.LeaseID
}

func (store *stubRevisionStore) OpenRevisions(context.Context, []content.OpenRevisionRequest, *content.QuotaAccount) ([]content.OpenRevisionResult, error) {
	return slices.Clone(store.results), store.openErr
}
func (store *stubRevisionStore) RenewLease(content.LeaseID) (content.RevisionLease, error) {
	return store.renew, store.renewErr
}
func (store *stubRevisionStore) ReleaseLease(id content.LeaseID) error {
	store.mu.Lock()
	store.released = append(store.released, id)
	store.mu.Unlock()
	return nil
}
func (store *stubRevisionStore) ValidateLease(content.LeaseID, content.FileRevisionDescriptor) error {
	return store.validateErr
}
func (store *stubRevisionStore) ReadBlock(context.Context, content.LeaseID, content.BlockRef) ([]byte, error) {
	return slices.Clone(store.read), store.readErr
}

type stubRecordSealer struct {
	revision    []byte
	revisionErr error
	block       []byte
	blockErr    error
}

func (sealer stubRecordSealer) SealRevision(content.FileRevisionDescriptor) ([]byte, error) {
	return slices.Clone(sealer.revision), sealer.revisionErr
}
func (sealer stubRecordSealer) SealBlock(records.BlockRecord) (records.SealedBlock, error) {
	return records.SealedBlock{Object: slices.Clone(sealer.block)}, sealer.blockErr
}

func stubService(t *testing.T, descriptor content.FileRevisionDescriptor, store RevisionStore, sealer RecordSealer) (*SenderService, *SharedBlockCache) {
	t.Helper()
	process, _ := NewProcessCacheBudget(1 << 20)
	cache, _ := NewSharedBlockCache(descriptor.ShareInstance(), 1<<20, process)
	service, err := NewSenderService(SenderServiceConfig{Store: store, SessionQuota: quotaAccount(t, "stub-session"), Sealer: sealer, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	return service, cache
}

func TestSenderServiceRollsBackMalformedStoreAndSealOutcomes(t *testing.T) {
	base := newRuntimeFixture(t, 1)
	open, err := base.service.Open(context.Background(), mustOpenRequest(t, base.file))
	if err != nil {
		t.Fatal(err)
	}
	lease := open.Items()[0].Lease
	defer base.close(t)
	file := lease.Descriptor().FileID()
	other := flowID[catalog.FileID](141)
	requests, _ := NewOpenRequest([]OpenItem{{FileID: file}, {FileID: other}})

	tests := []struct {
		name    string
		results []content.OpenRevisionResult
		openErr error
	}{
		{"store error", []content.OpenRevisionResult{{FileID: file, Lease: lease}}, errors.New("store failed")},
		{"wrong count", []content.OpenRevisionResult{{FileID: file, Lease: lease}}, nil},
		{"wrong order", []content.OpenRevisionResult{{FileID: other, Lease: lease}, {FileID: file, Err: content.ErrRevisionNotFound}}, nil},
		{"lease and error", []content.OpenRevisionResult{{FileID: file, Lease: lease, Err: content.ErrRevisionNotFound}, {FileID: other, Err: content.ErrRevisionNotFound}}, nil},
		{"zero lease success", []content.OpenRevisionResult{{FileID: file}, {FileID: other, Err: content.ErrRevisionNotFound}}, nil},
		{"duplicate lease", []content.OpenRevisionResult{{FileID: file, Lease: lease}, {FileID: other, Lease: lease}}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &stubRevisionStore{results: test.results, openErr: test.openErr}
			service, cache := stubService(t, lease.Descriptor(), store, stubRecordSealer{revision: []byte{1}})
			defer cache.Close()
			defer service.Close()
			if _, err := service.Open(context.Background(), requests); err == nil {
				t.Fatal("malformed store result accepted")
			}
		})
	}

	sealStore := &stubRevisionStore{results: []content.OpenRevisionResult{{FileID: file, Lease: lease}}}
	service, cache := stubService(t, lease.Descriptor(), sealStore, stubRecordSealer{revisionErr: records.ErrSealLimit})
	defer cache.Close()
	defer service.Close()
	one, _ := NewOpenRequest([]OpenItem{{FileID: file}})
	result, err := service.Open(context.Background(), one)
	if err != nil || result.Items()[0].Failure == nil || result.Items()[0].Failure.Code != RevisionCodeQuota {
		t.Fatalf("seal failure result=%+v err=%v", result.Items(), err)
	}
	if len(sealStore.released) != 1 {
		t.Fatal("failed seal did not release its lease")
	}

	reuseStore := &stubRevisionStore{results: []content.OpenRevisionResult{{FileID: file, Lease: lease}}}
	reuseService, reuseCache := stubService(t, lease.Descriptor(), reuseStore, stubRecordSealer{revision: []byte{1}})
	defer reuseCache.Close()
	if _, err := reuseService.Open(context.Background(), one); err != nil {
		t.Fatal(err)
	}
	if _, err := reuseService.Open(context.Background(), one); err == nil {
		t.Fatal("session lease identity reuse accepted")
	}
	if err := reuseService.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reuseService.Open(context.Background(), one); !errors.Is(err, ErrServiceClosed) {
		t.Fatalf("closed service open error=%v", err)
	}
}

func mustOpenRequest(t *testing.T, file catalog.FileID) OpenRequest {
	t.Helper()
	request, err := NewOpenRequest([]OpenItem{{FileID: file}})
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestStubbedServiceCoversBlockErrorAndEmitterBoundaries(t *testing.T) {
	base := newRuntimeFixture(t, 1)
	open, _ := base.service.Open(context.Background(), mustOpenRequest(t, base.file))
	lease := open.Items()[0].Lease
	defer base.close(t)
	store := &stubRevisionStore{
		results: []content.OpenRevisionResult{{FileID: base.file, Lease: lease}},
		renew:   lease, read: make([]byte, catalog.MinChunkSize),
	}
	sealer := stubRecordSealer{revision: []byte{1}, block: []byte("sealed")}
	service, cache := stubService(t, lease.Descriptor(), store, sealer)
	defer cache.Close()
	defer service.Close()
	if _, err := service.Open(context.Background(), mustOpenRequest(t, base.file)); err != nil {
		t.Fatal(err)
	}
	request, _ := NewBlockRequest(lease.ID(), []uint64{0})
	operation := flowID[protocolsession.OperationID](151)
	emitErr := errors.New("writer stopped")
	if _, err := service.ServeBlocks(context.Background(), operation, request, func(context.Context, protocolsession.Message) error { return emitErr }); !errors.Is(err, emitErr) {
		t.Fatalf("emitter error=%v", err)
	}
	cache.mu.Lock()
	cache.evictOldestLocked()
	cache.mu.Unlock()
	store.readErr = errors.New("read failed")
	if _, err := service.ServeBlocks(context.Background(), operation, request, func(context.Context, protocolsession.Message) error { return nil }); err == nil {
		t.Fatal("read error hidden")
	}
	if _, err := service.ServeBlocks(context.Background(), protocolsession.OperationID{}, request, func(context.Context, protocolsession.Message) error { return nil }); !errors.Is(err, ErrOperationIdentity) {
		t.Fatalf("zero operation error=%v", err)
	}
	if _, err := service.ServeBlocks(context.Background(), operation, request, nil); !errors.Is(err, ErrOperationIdentity) {
		t.Fatalf("nil emitter error=%v", err)
	}
	outOfRange, _ := NewBlockRequest(lease.ID(), []uint64{1})
	if _, err := service.ServeBlocks(context.Background(), operation, outOfRange, func(context.Context, protocolsession.Message) error { return nil }); !errors.Is(err, content.ErrBlockOutOfRange) {
		t.Fatalf("out-of-range serve error=%v", err)
	}
}

func TestCacheAdmissionEvictionCancellationAndCloseEdges(t *testing.T) {
	descriptor := flowDescriptor(t, uint64(catalog.MinChunkSize)*2)
	if _, err := NewProcessCacheBudget(0); err == nil {
		t.Fatal("zero process cache accepted")
	}
	if _, err := NewSharedBlockCache(catalog.ShareInstance{}, 1, nil); err == nil {
		t.Fatal("invalid cache accepted")
	}
	if _, err := NewBlockCacheKey(content.FileRevisionDescriptor{}, 0); err == nil {
		t.Fatal("invalid cache key accepted")
	}
	if _, err := NewBlockCacheKey(descriptor, descriptor.Geometry().BlockCount()); err == nil {
		t.Fatal("out-of-range cache key accepted")
	}
	process, _ := NewProcessCacheBudget(8)
	cache, _ := NewSharedBlockCache(descriptor.ShareInstance(), 5, process)
	key0, _ := NewBlockCacheKey(descriptor, 0)
	key1, _ := NewBlockCacheKey(descriptor, 1)
	if _, err := cache.Get(context.Background(), BlockCacheKey{}, func(context.Context) ([]byte, error) { return []byte{1}, nil }); !errors.Is(err, ErrInvalidCacheKey) {
		t.Fatalf("invalid key get error=%v", err)
	}
	if _, err := cache.Get(context.Background(), key0, nil); !errors.Is(err, ErrInvalidCacheKey) {
		t.Fatalf("nil loader error=%v", err)
	}
	if _, err := cache.Get(context.Background(), key0, func(context.Context) ([]byte, error) { return nil, nil }); err == nil {
		t.Fatal("empty loader result accepted")
	}
	_, _ = cache.Get(context.Background(), key0, func(context.Context) ([]byte, error) { return []byte{1, 2, 3, 4}, nil })
	_, _ = cache.Get(context.Background(), key1, func(context.Context) ([]byte, error) { return []byte{5, 6, 7, 8}, nil })
	if cache.UsedBytes() != 4 || process.Used() != 4 {
		t.Fatalf("LRU usage cache=%d process=%d", cache.UsedBytes(), process.Used())
	}
	if _, err := cache.Get(context.Background(), key0, func(context.Context) ([]byte, error) { return bytes.Repeat([]byte{1}, 6), nil }); err != nil {
		t.Fatal(err)
	}
	if cache.UsedBytes() != 4 {
		t.Fatal("oversized cache object displaced bounded entry")
	}

	started := make(chan struct{})
	cancelled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cache.mu.Lock()
	for cache.evictOldestLocked() {
	}
	cache.mu.Unlock()
	go func() {
		_, err := cache.Get(ctx, key0, func(loadContext context.Context) ([]byte, error) {
			close(started)
			<-loadContext.Done()
			close(cancelled)
			return nil, loadContext.Err()
		})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("last waiter error=%v", err)
	}
	<-cancelled
	cache.Close()
	cache.Close()
	if _, err := cache.Get(context.Background(), key0, func(context.Context) ([]byte, error) { return []byte{1}, nil }); !errors.Is(err, ErrServiceClosed) {
		t.Fatalf("closed cache error=%v", err)
	}
}

func TestCacheCloseCancelsRegisteredRevisionConsumers(t *testing.T) {
	descriptor := flowDescriptor(t, 1)
	process, _ := NewProcessCacheBudget(1 << 20)
	cache, _ := NewSharedBlockCache(descriptor.ShareInstance(), 1<<20, process)
	watchContext, cancelWatch := context.WithCancelCause(context.Background())
	unwatch, err := cache.watchRevision(descriptor, cancelWatch)
	if err != nil {
		t.Fatal(err)
	}
	cache.Close()
	select {
	case <-watchContext.Done():
		if !errors.Is(context.Cause(watchContext), ErrServiceClosed) {
			t.Fatalf("cache-close cause=%v", context.Cause(watchContext))
		}
	case <-time.After(time.Second):
		t.Fatal("cache close did not cancel its registered consumer")
	}
	unwatch()
	unwatch()
	if _, err := cache.watchRevision(descriptor, cancelWatch); !errors.Is(err, ErrServiceClosed) {
		t.Fatalf("closed cache accepted a revision consumer: %v", err)
	}
}

func TestHandlerCancellationBookkeepingIsIdempotent(t *testing.T) {
	fixture := newRuntimeFixture(t, 1)
	defer fixture.close(t)
	handler, _ := NewSenderHandler(SenderHandlerConfig{Service: fixture.service, Outbound: newRecordingOutbound()})
	operation := flowID[protocolsession.OperationID](161)
	owner := handlerOperation{id: operation}
	ctx, cancel := context.WithCancel(context.Background())
	if !handler.register(owner, cancel) || handler.register(owner, func() {}) {
		t.Fatal("operation registration was not unique")
	}
	handler.cancel(owner)
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("operation cancel did not reach worker")
	}
	handler.unregister(owner)
	handler.cancel(owner)
	otherContext, otherCancel := context.WithCancel(context.Background())
	_ = handler.register(handlerOperation{id: flowID[protocolsession.OperationID](162)}, otherCancel)
	handler.cancelAll()
	if !errors.Is(otherContext.Err(), context.Canceled) {
		t.Fatal("cancelAll did not stop operation")
	}
}
