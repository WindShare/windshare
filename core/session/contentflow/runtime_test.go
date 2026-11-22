package contentflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type runtimeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *runtimeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *runtimeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

type runtimeCatalog struct {
	records map[catalog.NodeID]catalog.NodeRecord
}

func (c runtimeCatalog) Node(_ context.Context, id catalog.NodeID) (catalog.NodeRecord, bool, error) {
	record, ok := c.records[id]
	return record, ok, nil
}

type runtimeStableFile struct {
	mu        sync.Mutex
	data      []byte
	drifted   bool
	reads     []uint64
	readGate  chan struct{}
	readStart chan struct{}
	closed    atomic.Int32
}

func (f *runtimeStableFile) ExactSize() uint64                  { return uint64(len(f.data)) }
func (f *runtimeStableFile) ModifiedTime() catalog.ModifiedTime { return catalog.ModifiedTime{} }
func (f *runtimeStableFile) Close() error                       { f.closed.Add(1); return nil }
func (f *runtimeStableFile) Verify(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.drifted {
		return content.ErrSourceDrift
	}
	return nil
}
func (f *runtimeStableFile) ReadAt(ctx context.Context, destination []byte, offset uint64) (int, error) {
	f.mu.Lock()
	f.reads = append(f.reads, offset)
	start, gate := f.readStart, f.readGate
	f.mu.Unlock()
	if start != nil {
		select {
		case start <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-gate:
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

func (f *runtimeStableFile) setDrifted() {
	f.mu.Lock()
	f.drifted = true
	f.mu.Unlock()
}

func (f *runtimeStableFile) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reads)
}

type runtimeSource struct{ file *runtimeStableFile }

func (s runtimeSource) OpenStable(context.Context, catalog.NodeRecord) (content.StableFile, error) {
	return s.file, nil
}

type runtimeIDs struct {
	mu   sync.Mutex
	next byte
}

func (ids *runtimeIDs) value() byte {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.next++
	return ids.next
}

func (ids *runtimeIDs) NewLeaseID() (content.LeaseID, error) {
	return flowID[content.LeaseID](ids.value()), nil
}

type runtimeFixture struct {
	share      catalog.ShareInstance
	file       catalog.FileID
	stable     *runtimeStableFile
	clock      *runtimeClock
	store      *content.RevisionStore
	cache      *SharedBlockCache
	service    *SenderService
	opener     *records.Decoder
	handlePool *content.HandlePool
}

func newRuntimeFixture(t *testing.T, blocks int) runtimeFixture {
	t.Helper()
	share := flowID[catalog.ShareInstance](41)
	file := flowID[catalog.FileID](42)
	parent := flowID[catalog.DirectoryID](43)
	size := uint64(blocks * catalog.MinChunkSize)
	data := make([]byte, size)
	for index := range data {
		data[index] = byte(index / catalog.MinChunkSize)
	}
	locator, _ := catalog.NewSourceReference([]byte("object:" + "file.bin"))
	sourceIdentity, _ := catalog.NewSourceIdentity([]byte("runtime-source"))
	candidate, _ := catalog.NewVersionCandidate([]byte("runtime-candidate"))
	record, err := catalog.NewFileNodeRecord(file, parent, "file.bin", locator, sourceIdentity, candidate, size, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	processCache, _ := NewProcessCacheBudget(DefaultProcessBlockCacheBytes)
	cache, _ := NewSharedBlockCache(share, uint64(catalog.MinChunkSize)*4, processCache)
	clock := &runtimeClock{now: time.Unix(1_000, 0)}
	stable := &runtimeStableFile{data: data}
	handlePool, err := content.NewHandlePool(content.DefaultHandlePoolConfig())
	if err != nil {
		t.Fatal(err)
	}
	store, err := content.NewRevisionStore(content.RevisionStoreConfig{
		ShareInstance: share, ChunkSize: catalog.MinChunkSize,
		Catalog: runtimeCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		Source:  runtimeSource{file: stable}, HandlePool: handlePool,
		ShareLimits: content.DefaultShareCapacityLimits(),
		Clock:       clock, LeaseIDs: &runtimeIDs{}, Cache: cache,
	})
	if err != nil {
		_ = handlePool.Close()
		t.Fatal(err)
	}
	session, err := store.RegisterSession(content.SessionConfig{
		SessionID: "contentflow-runtime-session", Limits: content.DefaultSessionCapacityLimits(),
	})
	if err != nil {
		_ = store.Close()
		_ = handlePool.Close()
		t.Fatal(err)
	}
	sealer := records.Encoder{}
	opener := &records.Decoder{ShareInstance: share}
	service, err := NewSenderService(SenderServiceConfig{
		Store: session, Encoder: sealer, Cache: cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	return runtimeFixture{
		share: share, file: file, stable: stable, clock: clock, store: store,
		cache: cache, service: service, opener: opener, handlePool: handlePool,
	}
}

func (fixture runtimeFixture) close(t *testing.T) {
	t.Helper()
	if err := fixture.service.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.cache.Close()
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.handlePool.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSenderServicePreservesPerItemOpenLeaseAndBlockSemantics(t *testing.T) {
	fixture := newRuntimeFixture(t, 2)
	defer fixture.close(t)
	missing := flowID[catalog.FileID](99)
	ranges, _ := content.NewRangeSet([]content.Range{{Offset: 1, End: 2}})
	request, _ := NewOpenRequest([]OpenItem{{FileID: missing}, {FileID: fixture.file, InitialRanges: ranges}})
	results, err := fixture.service.Open(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	items := results.Items()
	if len(items) != 2 || items[0].Failure == nil || items[0].Failure.Code != RevisionCodeNotFound || items[1].Failure != nil {
		t.Fatalf("open results=%+v", items)
	}
	lease := items[1].Lease
	wireResults, err := EncodeOpenResults(results)
	if err != nil {
		t.Fatal(err)
	}
	receivedResults, err := DecodeOpenResults(wireResults, []catalog.FileID{missing, fixture.file})
	if err != nil || len(receivedResults) != 2 || receivedResults[0].Failure == nil || receivedResults[1].Lease.ID != lease.ID() {
		t.Fatalf("wire open results=%+v err=%v", receivedResults, err)
	}
	if _, err := DecodeOpenResults(wireResults, []catalog.FileID{fixture.file, missing}); !errors.Is(err, ErrInvalidOpenResults) {
		t.Fatalf("reordered open results error=%v", err)
	}
	descriptor, err := fixture.opener.DecodeRevision(fixture.file, catalog.MinChunkSize, items[1].RevisionObject)
	if err != nil || descriptor != lease.Descriptor() {
		t.Fatalf("revision object descriptor=%+v err=%v", descriptor, err)
	}

	if _, err := fixture.service.Renew(lease.ID()); !errors.Is(err, content.ErrRenewTooEarly) {
		t.Fatalf("early renewal error=%v", err)
	}
	fixture.clock.Advance(content.LeaseRenewWindow + time.Second)
	renewed, err := fixture.service.Renew(lease.ID())
	if err != nil || renewed.ID() != lease.ID() || renewed.TTL() <= 0 {
		t.Fatalf("renewed lease=%+v err=%v", renewed, err)
	}
	renewedBody, err := EncodeLeaseResult(renewed)
	if err != nil {
		t.Fatal(err)
	}
	remoteLease, err := DecodeLeaseResult(renewedBody, renewed.ID())
	if err != nil || remoteLease.ID != renewed.ID() || remoteLease.TTL != renewed.TTL() {
		t.Fatalf("wire renewal=%+v err=%v", remoteLease, err)
	}

	blockRequest, _ := NewBlockRequest(lease.ID(), []uint64{1})
	operation := flowID[protocolsession.OperationID](51)
	var emitted []protocolsession.Message
	count, err := fixture.service.ServeBlocks(context.Background(), operation, blockRequest, func(_ context.Context, message protocolsession.Message) error {
		emitted = append(emitted, message)
		return nil
	})
	if err != nil || count != 1 || len(emitted) == 0 {
		t.Fatalf("serve count=%d fragments=%d err=%v", count, len(emitted), err)
	}
	hierarchy, _ := reassemblyHierarchy(t, ReassemblyLimits{Bytes: records.MaxBlockRecordObjectBytes, Records: 2})
	assembler, _ := NewAssembler(flowID[protocolsession.ProtocolSessionID](52), hierarchy, nil)
	var object []byte
	for _, message := range emitted {
		assembled, acceptErr := assembler.AcceptAuthenticated(flowMessagePlaintext(t, message))
		if acceptErr != nil {
			t.Fatal(acceptErr)
		}
		if assembled.Status == RecordComplete {
			object = assembled.Object
		}
	}
	opened, err := fixture.opener.DecodeBlock(descriptor, 1, object)
	if err != nil || len(opened.Data()) != catalog.MinChunkSize || opened.Data()[0] != 1 {
		t.Fatalf("opened block bytes=%d err=%v", len(opened.Data()), err)
	}
	if fixture.stable.readCount() != 1 {
		t.Fatalf("source reads=%d, want 1", fixture.stable.readCount())
	}
	otherOperation := flowID[protocolsession.OperationID](53)
	if _, err := fixture.service.ServeBlocks(context.Background(), otherOperation, blockRequest, func(context.Context, protocolsession.Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if fixture.stable.readCount() != 1 {
		t.Fatal("share cache did not reuse the sealed sender object")
	}

	if err := fixture.service.Release(lease.ID()); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.Release(lease.ID()); err != nil {
		t.Fatalf("duplicate release was not idempotent: %v", err)
	}
	if _, err := fixture.service.ServeBlocks(context.Background(), operation, blockRequest, func(context.Context, protocolsession.Message) error { return nil }); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("released lease serve error=%v", err)
	}
}

func TestSenderServiceDriftRetiresEveryOwnedLeaseAndCache(t *testing.T) {
	fixture := newRuntimeFixture(t, 2)
	defer fixture.close(t)
	request, _ := NewOpenRequest([]OpenItem{{FileID: fixture.file}, {FileID: fixture.file}})
	results, err := fixture.service.Open(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	items := results.Items()
	first, second := items[0].Lease, items[1].Lease
	blockRequest, _ := NewBlockRequest(first.ID(), []uint64{0})
	if _, err := fixture.service.ServeBlocks(context.Background(), flowID[protocolsession.OperationID](61), blockRequest, func(context.Context, protocolsession.Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	fixture.stable.setDrifted()
	secondRequest, _ := NewBlockRequest(second.ID(), []uint64{1})
	if _, err := fixture.service.ServeBlocks(context.Background(), flowID[protocolsession.OperationID](62), secondRequest, func(context.Context, protocolsession.Message) error { return nil }); !errors.Is(err, content.ErrRevisionDrift) {
		t.Fatalf("drift error=%v", err)
	}
	for _, lease := range []content.RevisionLease{first, second} {
		if _, err := fixture.service.Renew(lease.ID()); !errors.Is(err, ErrLeaseNotOwned) {
			t.Fatalf("drifted lease remained owned: %v", err)
		}
	}
	if fixture.cache.UsedBytes() != 0 {
		t.Fatal("drifted revision remained cached")
	}
}

func TestCachedBlockCannotBypassLeaseExpiry(t *testing.T) {
	fixture := newRuntimeFixture(t, 1)
	defer fixture.close(t)
	results, err := fixture.service.Open(context.Background(), mustOpenRequest(t, fixture.file))
	if err != nil {
		t.Fatal(err)
	}
	lease := results.Items()[0].Lease
	request, _ := NewBlockRequest(lease.ID(), []uint64{0})
	emit := func(context.Context, protocolsession.Message) error { return nil }
	if _, err := fixture.service.ServeBlocks(context.Background(), flowID[protocolsession.OperationID](63), request, emit); err != nil {
		t.Fatal(err)
	}
	reads := fixture.stable.readCount()
	fixture.clock.Advance(content.LeaseTTL)
	if _, err := fixture.service.ServeBlocks(context.Background(), flowID[protocolsession.OperationID](64), request, emit); !errors.Is(err, content.ErrLeaseExpired) {
		t.Fatalf("expired cached block error=%v", err)
	}
	if fixture.stable.readCount() != reads {
		t.Fatal("expired cache hit reached the source")
	}
	if _, err := fixture.service.ownedDescriptor(lease.ID()); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("expired lease remained session-owned: %v", err)
	}
}

func TestRevisionInvalidationCancelsActiveCachedDelivery(t *testing.T) {
	fixture := newRuntimeFixture(t, 2)
	defer fixture.close(t)
	results, err := fixture.service.Open(context.Background(), mustOpenRequest(t, fixture.file))
	if err != nil {
		t.Fatal(err)
	}
	lease := results.Items()[0].Lease
	request, _ := NewBlockRequest(lease.ID(), []uint64{0})
	if _, err := fixture.service.ServeBlocks(
		context.Background(), flowID[protocolsession.OperationID](65), request,
		func(context.Context, protocolsession.Message) error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, serveErr := fixture.service.ServeBlocks(
			context.Background(), flowID[protocolsession.OperationID](66), request,
			func(sendContext context.Context, _ protocolsession.Message) error {
				select {
				case <-started:
				default:
					close(started)
				}
				<-sendContext.Done()
				return sendContext.Err()
			},
		)
		done <- serveErr
	}()
	<-started
	fixture.stable.setDrifted()
	driftRequest, _ := NewBlockRequest(lease.ID(), []uint64{1})
	if _, err := fixture.service.ServeBlocks(
		context.Background(), flowID[protocolsession.OperationID](67), driftRequest,
		func(context.Context, protocolsession.Message) error { return nil },
	); !errors.Is(err, content.ErrRevisionDrift) {
		t.Fatalf("drift trigger error=%v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, content.ErrRevisionDrift) {
			t.Fatalf("active cached delivery error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("revision invalidation did not cancel active cached delivery")
	}
	if _, err := fixture.service.ownedDescriptor(lease.ID()); !errors.Is(err, ErrLeaseNotOwned) {
		t.Fatalf("invalidated lease remained session-owned: %v", err)
	}
}

func TestSharedBlockCacheIsolatesConsumerCancellationAndInvalidation(t *testing.T) {
	synctest.Test(t, testSharedBlockCacheIsolatesConsumerCancellationAndInvalidation)
}

func testSharedBlockCacheIsolatesConsumerCancellationAndInvalidation(t *testing.T) {
	descriptor := flowDescriptor(t, uint64(catalog.MinChunkSize)*2)
	process, _ := NewProcessCacheBudget(uint64(catalog.MinChunkSize) * 4)
	cache, _ := NewSharedBlockCache(descriptor.ShareInstance(), uint64(catalog.MinChunkSize)*2, process)
	defer cache.Close()
	key, _ := NewBlockCacheKey(descriptor, 0)
	started := make(chan struct{})
	release := make(chan struct{})
	var loads atomic.Int32
	loader := func(ctx context.Context) ([]byte, error) {
		loads.Add(1)
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return []byte("payload"), nil
		}
	}
	firstContext, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() { _, err := cache.get(firstContext, key, loader); firstResult <- err }()
	<-started
	secondResult := make(chan string, 1)
	go func() { object, _ := cache.get(context.Background(), key, loader); secondResult <- object.payload }()
	synctest.Wait()
	cache.mu.Lock()
	joined := cache.inflight[key] != nil && cache.inflight[key].waiters == 2
	cache.mu.Unlock()
	if !joined {
		t.Fatal("second cache consumer did not join the shared load")
	}
	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("first consumer error=%v", err)
	}
	close(release)
	if object := <-secondResult; object != "payload" {
		t.Fatalf("second consumer object=%q", object)
	}
	if object, err := cache.get(context.Background(), key, loader); err != nil || object.payload != "payload" || loads.Load() != 1 {
		t.Fatalf("cache hit object=%q loads=%d err=%v", object, loads.Load(), err)
	}

	keyOne, _ := NewBlockCacheKey(descriptor, 1)
	ignoredCancelStarted := make(chan struct{})
	ignoredCancelRelease := make(chan struct{})
	staleDone := make(chan error, 1)
	go func() {
		_, err := cache.get(context.Background(), keyOne, func(context.Context) ([]byte, error) {
			close(ignoredCancelStarted)
			<-ignoredCancelRelease
			return []byte("stale"), nil
		})
		staleDone <- err
	}()
	<-ignoredCancelStarted
	cache.InvalidateRevision(descriptor.FileID(), descriptor.FileRevision())
	select {
	case err := <-staleDone:
		if !errors.Is(err, content.ErrRevisionDrift) {
			t.Fatalf("invalidated waiter error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cache invalidation waited for a loader that ignored cancellation")
	}
	close(ignoredCancelRelease)
	synctest.Wait()
	if cache.UsedBytes() != 0 {
		t.Fatalf("stale loader repopulated invalidated cache: used=%d", cache.UsedBytes())
	}
}

type recordingOutbound struct {
	controls  chan protocolsession.MessageKind
	fragments chan protocolsession.Message
	failures  chan OperationFailure
}

type failingSemanticOutbound struct {
	err             error
	operationErrors atomic.Int32
}

type stubOperationState struct {
	mu      sync.Mutex
	next    uint64
	active  map[protocolsession.OperationRef]protocolsession.MessageKind
	current map[protocolsession.OperationRef]protocolsession.MessageKind
}

func newStubOperationState() *stubOperationState {
	return &stubOperationState{
		active:  make(map[protocolsession.OperationRef]protocolsession.MessageKind),
		current: make(map[protocolsession.OperationRef]protocolsession.MessageKind),
	}
}

func (s *stubOperationState) admit(id protocolsession.OperationID, kind protocolsession.MessageKind) protocolsession.Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	ref := protocolsession.OperationRef{ID: id, Token: s.next}
	s.active[ref] = kind
	s.current[ref] = kind
	return protocolsession.NewOperation(ref, s)
}

func (s *stubOperationState) cancel(ref protocolsession.OperationRef) {
	s.mu.Lock()
	delete(s.active, ref)
	s.mu.Unlock()
}

func (s *stubOperationState) Active(ref protocolsession.OperationRef) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.active[ref]
	return ok
}

func (s *stubOperationState) Current(ref protocolsession.OperationRef) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.current[ref]
	return ok
}

func (s *stubOperationState) RequestKind(ref protocolsession.OperationRef) (protocolsession.MessageKind, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kind, ok := s.current[ref]
	return kind, ok
}

func (s *stubOperationState) PeerAttemptBinding(protocolsession.OperationRef) (protocolsession.PeerAttemptBinding, bool) {
	return protocolsession.PeerAttemptBinding{}, false
}

func testActiveOperation(id protocolsession.OperationID) protocolsession.Operation {
	return newStubOperationState().admit(id, protocolsession.MessageOpenRevisions)
}

type stubRegistrar struct {
	handlers map[protocolsession.MessageKind]protocolsession.MessageHandler
}

func (r *stubRegistrar) RegisterHandler(kind protocolsession.MessageKind, h protocolsession.MessageHandler) error {
	if r.handlers == nil {
		r.handlers = make(map[protocolsession.MessageKind]protocolsession.MessageHandler)
	}
	if _, exists := r.handlers[kind]; exists {
		return protocolsession.ErrHandlerRegistered
	}
	r.handlers[kind] = h
	return nil
}

func (outbound *failingSemanticOutbound) SendControl(
	context.Context,
	protocolsession.Operation,
	protocolsession.MessageKind,
	[]byte,
) error {
	return outbound.err
}

func (outbound *failingSemanticOutbound) SendFragment(context.Context, protocolsession.Operation, protocolsession.Message) error {
	return outbound.err
}

func (outbound *failingSemanticOutbound) SendOperationError(context.Context, protocolsession.Operation, OperationFailure) error {
	outbound.operationErrors.Add(1)
	return outbound.err
}

func newRecordingOutbound() *recordingOutbound {
	return &recordingOutbound{
		controls: make(chan protocolsession.MessageKind, 16), fragments: make(chan protocolsession.Message, 128),
		failures: make(chan OperationFailure, 16),
	}
}

func (outbound *recordingOutbound) SendControl(_ context.Context, _ protocolsession.Operation, kind protocolsession.MessageKind, _ []byte) error {
	outbound.controls <- kind
	return nil
}
func (outbound *recordingOutbound) SendFragment(_ context.Context, _ protocolsession.Operation, message protocolsession.Message) error {
	outbound.fragments <- message
	return nil
}
func (outbound *recordingOutbound) SendOperationError(_ context.Context, _ protocolsession.Operation, failure OperationFailure) error {
	outbound.failures <- failure
	return nil
}

func operationMessage(t *testing.T, kind protocolsession.MessageKind, operation protocolsession.OperationID, body []byte) protocolsession.Message {
	t.Helper()
	message, err := protocolsession.NewMessage(kind, &operation, body)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestSenderHandlerDispatchesWithoutOwningRouterOrWriter(t *testing.T) {
	fixture := newRuntimeFixture(t, 1)
	defer fixture.close(t)
	outbound := newRecordingOutbound()
	handler, err := NewSenderHandler(SenderHandlerConfig{Service: fixture.service, Outbound: outbound, QueueDepth: 8, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- handler.Run(ctx) }()
	state := newStubOperationState()
	request, _ := NewOpenRequest([]OpenItem{{FileID: fixture.file}})
	body, _ := EncodeOpenRequest(request)
	operation := flowID[protocolsession.OperationID](71)
	openMessage := operationMessage(t, protocolsession.MessageOpenRevisions, operation, body)
	if err := handler.HandleMessage(context.Background(), state.admit(operation, protocolsession.MessageOpenRevisions), openMessage); err != nil {
		t.Fatal(err)
	}
	select {
	case kind := <-outbound.controls:
		if kind != protocolsession.MessageOpenResults {
			t.Fatalf("control kind=%d", kind)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("open result was not dispatched")
	}

	invalidBody, _ := protocolsession.EncodeBody([]any{[]byte{1}})
	invalidID := flowID[protocolsession.OperationID](72)
	invalidRenew := operationMessage(t, protocolsession.MessageRenewLease, invalidID, invalidBody)
	if err := handler.HandleMessage(context.Background(), state.admit(invalidID, protocolsession.MessageRenewLease), invalidRenew); err != nil {
		t.Fatal(err)
	}
	select {
	case failure := <-outbound.failures:
		if failure.Scope != RevisionErrorScope {
			t.Fatalf("failure scope=%d", failure.Scope)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("operation failure was not dispatched")
	}
	directResults, err := fixture.service.Open(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	directLease := directResults.Items()[0].Lease
	leaseRequestBody, _ := EncodeLeaseRequest(directLease.ID())
	id173 := flowID[protocolsession.OperationID](173)
	handler.process(context.Background(), state.admit(id173, protocolsession.MessageRenewLease), operationMessage(t, protocolsession.MessageRenewLease, id173, leaseRequestBody))
	if failure := <-outbound.failures; failure.Scope != RevisionErrorScope {
		t.Fatalf("early renew failure scope=%d", failure.Scope)
	}
	fixture.clock.Advance(content.LeaseRenewWindow + time.Second)
	id74 := flowID[protocolsession.OperationID](74)
	handler.process(context.Background(), state.admit(id74, protocolsession.MessageRenewLease), operationMessage(t, protocolsession.MessageRenewLease, id74, leaseRequestBody))
	if kind := <-outbound.controls; kind != protocolsession.MessageLeaseResult {
		t.Fatalf("renew control kind=%d", kind)
	}
	blockRequest, _ := NewBlockRequest(directLease.ID(), []uint64{0})
	blockBody, _ := EncodeBlockRequest(blockRequest)
	id75 := flowID[protocolsession.OperationID](75)
	handler.process(context.Background(), state.admit(id75, protocolsession.MessageRequestBlocks), operationMessage(t, protocolsession.MessageRequestBlocks, id75, blockBody))
	if kind := <-outbound.controls; kind != protocolsession.MessageOperationComplete {
		t.Fatalf("block final kind=%d", kind)
	}
	if len(outbound.fragments) == 0 {
		t.Fatal("block handler emitted no fragments")
	}
	id76 := flowID[protocolsession.OperationID](76)
	handler.process(context.Background(), state.admit(id76, protocolsession.MessageReleaseLease), operationMessage(t, protocolsession.MessageReleaseLease, id76, leaseRequestBody))
	if kind := <-outbound.controls; kind != protocolsession.MessageOperationComplete {
		t.Fatalf("release final kind=%d", kind)
	}

	router := &stubRegistrar{}
	if err := RegisterSenderHandlers(router, handler); err != nil {
		t.Fatalf("register content handlers: %v", err)
	}
	if err := RegisterSenderHandlers(router, handler); err == nil {
		t.Fatal("duplicate content handler registration accepted")
	}
	id73 := flowID[protocolsession.OperationID](73)
	if err := handler.HandleMessage(context.Background(), state.admit(id73, protocolsession.MessageOpenRevisions), operationMessage(t, protocolsession.MessageCatalogResult, id73, completeBody(t))); !errors.Is(err, ErrUnexpectedMessage) {
		t.Fatalf("unexpected kind error=%v", err)
	}
	invalidCancel, _ := protocolsession.EncodeBody([]any{uint64(0)})
	id77 := flowID[protocolsession.OperationID](77)
	if err := handler.HandleMessage(context.Background(), state.admit(id77, protocolsession.MessageOpenRevisions), operationMessage(t, protocolsession.MessageCancel, id77, invalidCancel)); !errors.Is(err, ErrInvalidCancelRequest) {
		t.Fatalf("invalid cancel reason error=%v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("handler run error=%v", err)
	}
	if err := handler.Run(context.Background()); err == nil {
		t.Fatal("handler Run was reusable")
	}
}

func TestOutboundFailureStaysAtTheSessionBoundary(t *testing.T) {
	fixture := newRuntimeFixture(t, 1)
	defer fixture.close(t)
	results, err := fixture.service.Open(context.Background(), mustOpenRequest(t, fixture.file))
	if err != nil {
		t.Fatal(err)
	}
	lease := results.Items()[0].Lease
	body, _ := EncodeLeaseRequest(lease.ID())
	outbound := &failingSemanticOutbound{err: errors.New("transport stopped")}
	handler, err := NewSenderHandler(SenderHandlerConfig{Service: fixture.service, Outbound: outbound})
	if err != nil {
		t.Fatal(err)
	}
	id78 := flowID[protocolsession.OperationID](78)
	handler.process(context.Background(), testActiveOperation(id78), operationMessage(
		t, protocolsession.MessageReleaseLease, id78, body,
	))
	if outbound.operationErrors.Load() != 0 {
		t.Fatal("transport failure was reflected as a semantic operation error")
	}
}

func completeBody(t *testing.T) []byte {
	t.Helper()
	body, err := protocolsession.EncodeBody([]any{uint64(0)})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestSenderHandlerQueueAndServiceValidationFailuresAreBounded(t *testing.T) {
	fixture := newRuntimeFixture(t, 1)
	defer fixture.close(t)
	handler, _ := NewSenderHandler(SenderHandlerConfig{Service: fixture.service, Outbound: newRecordingOutbound(), QueueDepth: 1, Workers: 1})
	request, _ := NewOpenRequest([]OpenItem{{FileID: fixture.file}})
	body, _ := EncodeOpenRequest(request)
	state := newStubOperationState()
	id81 := flowID[protocolsession.OperationID](81)
	id82 := flowID[protocolsession.OperationID](82)
	first := operationMessage(t, protocolsession.MessageOpenRevisions, id81, body)
	second := operationMessage(t, protocolsession.MessageOpenRevisions, id82, body)
	if err := handler.HandleMessage(context.Background(), state.admit(id81, protocolsession.MessageOpenRevisions), first); err != nil {
		t.Fatal(err)
	}
	if err := handler.HandleMessage(context.Background(), state.admit(id82, protocolsession.MessageOpenRevisions), second); !errors.Is(err, ErrServiceQueueFull) {
		t.Fatalf("queue overflow error=%v", err)
	}
	if _, err := NewSenderHandler(SenderHandlerConfig{}); err == nil {
		t.Fatal("invalid handler accepted")
	}
	if err := RegisterSenderHandlers(nil, handler); err == nil {
		t.Fatal("nil registrar accepted")
	}
	if _, err := NewSenderService(SenderServiceConfig{}); err == nil {
		t.Fatal("invalid service accepted")
	}
	if err := fixture.service.Release(content.LeaseID{}); !errors.Is(err, ErrInvalidLeaseRequest) {
		t.Fatalf("zero release error=%v", err)
	}
}

func TestOperationFailureValidationRejectsAmbiguousValues(t *testing.T) {
	for _, invalid := range []OperationFailure{
		{Scope: 1, Code: BlockCodeTimeout, Message: "bad"},
		{Scope: BlockErrorScope, Code: RevisionCodeStale, Message: "bad"},
		{Scope: BlockErrorScope, Code: BlockCodeTimeout, Message: ""},
		{Scope: BlockErrorScope, Code: BlockCodeTimeout, RetryAfter: time.Second, Message: "bad"},
	} {
		if _, err := protocolsession.EncodeOperationFailure(invalid); err == nil {
			t.Fatalf("invalid failure accepted: %+v", invalid)
		}
	}
}

func TestErrorClassificationPreservesOperationScope(t *testing.T) {
	for err, expected := range map[error]uint16{
		content.ErrRevisionStale:        RevisionCodeStale,
		content.ErrRevisionNotFound:     RevisionCodeNotFound,
		content.ErrUnsupportedStability: RevisionCodeUnsupportedStability,
		content.ErrQuotaExceeded:        RevisionCodeUnreadable,
		content.ErrLeaseExpired:         RevisionCodeLeaseExpired,
		content.ErrRevisionDrift:        RevisionCodeDrift,
		content.ErrInvalidLease:         RevisionCodeInvalidLease,
		content.ErrRenewTooEarly:        RevisionCodeInvalidLease,
	} {
		if failure := classifyRevisionError(err); failure.Code != expected {
			t.Fatalf("revision error %v code=%#x", err, failure.Code)
		}
	}
	for err, expected := range map[error]uint16{
		content.ErrInvalidBlockRef:    BlockCodeInvalidRef,
		content.ErrBlockOutOfRange:    BlockCodeOutOfRange,
		records.ErrNonCanonicalObject: BlockCodeObjectAuth,
		ErrFragmentConflict:           BlockCodeFragmentConflict,
		ErrFragmentInactivity:         BlockCodeTimeout,
		context.Canceled:              BlockCodeCancelled,
	} {
		if code := classifyBlockError(err); code != expected {
			t.Fatalf("block error %v code=%#x", err, code)
		}
	}
}

func TestRevisionErrorClassificationGrantsRetryAuthorityOnlyToTypedCapacity(t *testing.T) {
	pool, err := content.NewHandlePool(content.HandlePoolConfig{
		Limits:     content.CapacityLimits{StableHandles: 1, ActiveLeases: 8},
		RetryAfter: MaxRevisionFailureRetryAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	share := flowID[catalog.ShareInstance](191)
	parent := flowID[catalog.DirectoryID](192)
	fileA := flowID[catalog.FileID](193)
	fileB := flowID[catalog.FileID](194)
	data := make([]byte, catalog.MinChunkSize)
	makeNode := func(id catalog.FileID, name string) catalog.NodeRecord {
		locator, _ := catalog.NewSourceReference([]byte("object:" + name))
		sourceID, _ := catalog.NewSourceIdentity([]byte("src:" + name))
		candidate, _ := catalog.NewVersionCandidate([]byte("ver:" + name))
		rec, recErr := catalog.NewFileNodeRecord(id, parent, name, locator, sourceID, candidate, uint64(len(data)), catalog.ModifiedTime{})
		if recErr != nil {
			t.Fatal(recErr)
		}
		return rec
	}
	store, err := content.NewRevisionStore(content.RevisionStoreConfig{
		ShareInstance: share, ChunkSize: catalog.MinChunkSize,
		Catalog: runtimeCatalog{records: map[catalog.NodeID]catalog.NodeRecord{
			fileA.NodeID(): makeNode(fileA, "a.bin"),
			fileB.NodeID(): makeNode(fileB, "b.bin"),
		}},
		Source:      runtimeSource{file: &runtimeStableFile{data: data}},
		HandlePool:  pool,
		ShareLimits: content.CapacityLimits{StableHandles: 8, ActiveLeases: 8},
		LeaseIDs:    &runtimeIDs{},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.RegisterSession(content.SessionConfig{
		SessionID: "contentflow-classification-session",
		Limits:    content.CapacityLimits{StableHandles: 8, ActiveLeases: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := session.OpenRevision(context.Background(), fileA)
	if err != nil {
		t.Fatal(err)
	}
	_, busyErr := session.OpenRevision(context.Background(), fileB)
	if _, ok := errors.AsType[*content.CapacityBusyError](busyErr); !ok {
		t.Fatalf("stable saturation error=%T %v", busyErr, busyErr)
	}
	failure := classifyRevisionError(fmt.Errorf("store open: %w", busyErr))
	if failure.Code != RevisionCodeQuota || !failure.Retryable || failure.RetryAfter != MaxRevisionFailureRetryAfter {
		t.Fatalf("typed capacity failure=%+v", failure)
	}
	descriptor := flowDescriptor(t, uint64(catalog.MinChunkSize))
	service, cache := stubService(t, descriptor, &stubRevisionStore{results: []content.OpenRevisionResult{{
		FileID: descriptor.FileID(), Err: fmt.Errorf("store open: %w", busyErr),
	}}}, stubRecordEncoder{revision: []byte{1}})
	defer cache.Close()
	defer service.Close()
	results, err := service.Open(context.Background(), mustOpenRequest(t, descriptor.FileID()))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeOpenResults(results)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeOpenResults(encoded, []catalog.FileID{descriptor.FileID()})
	if err != nil || len(decoded) != 1 || decoded[0].Failure == nil ||
		decoded[0].Failure.Code != RevisionCodeQuota || !decoded[0].Failure.Retryable ||
		decoded[0].Failure.RetryAfter != MaxRevisionFailureRetryAfter {
		t.Fatalf("authenticated capacity response=%+v err=%v", decoded, err)
	}
	for name, permanent := range map[string]error{
		"untyped quota":  fmt.Errorf("untyped quota error: %w", content.ErrQuotaExceeded),
		"seal limit":     records.ErrInvalidRecord,
		"missing source": content.ErrRevisionNotFound,
		"unreadable":     content.ErrRevisionUnreadable,
		"source drift":   content.ErrSourceDrift,
	} {
		classified := classifyRevisionError(permanent)
		if classified.Retryable || classified.RetryAfter != 0 || classified.Code == RevisionCodeQuota {
			t.Errorf("%s acquired capacity retry authority: %+v", name, classified)
		}
	}
	if err := session.EndLease(lease.ID(), content.LeaseRelinquished); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBlockHandlerKeepsRevisionFailuresOutOfTheBlockErrorDomain(t *testing.T) {
	base := newRuntimeFixture(t, 1)
	opened, err := base.service.Open(context.Background(), mustOpenRequest(t, base.file))
	if err != nil {
		t.Fatal(err)
	}
	lease := opened.Items()[0].Lease
	defer base.close(t)
	store := &stubRevisionStore{
		results:     []content.OpenRevisionResult{{FileID: base.file, Lease: lease}},
		validateErr: content.ErrRevisionDrift,
	}
	service, cache := stubService(t, lease.Descriptor(), store, stubRecordEncoder{revision: []byte("revision")})
	defer cache.Close()
	defer service.Close()
	if _, err := service.Open(context.Background(), mustOpenRequest(t, base.file)); err != nil {
		t.Fatal(err)
	}
	outbound := newRecordingOutbound()
	handler, err := NewSenderHandler(SenderHandlerConfig{Service: service, Outbound: outbound})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := NewBlockRequest(lease.ID(), []uint64{0})
	body, _ := EncodeBlockRequest(request)
	id184 := flowID[protocolsession.OperationID](184)
	handler.process(context.Background(), testActiveOperation(id184), operationMessage(t, protocolsession.MessageRequestBlocks, id184, body))
	select {
	case failure := <-outbound.failures:
		if failure.Scope != RevisionErrorScope || failure.Code != RevisionCodeDrift {
			t.Fatalf("drift failure=%+v", failure)
		}
	case <-time.After(time.Second):
		t.Fatal("revision drift produced no operation error")
	}
	unknownLease := flowID[content.LeaseID](185)
	unknownRequest, _ := NewBlockRequest(unknownLease, []uint64{0})
	unknownBody, _ := EncodeBlockRequest(unknownRequest)
	id186 := flowID[protocolsession.OperationID](186)
	handler.process(context.Background(), testActiveOperation(id186), operationMessage(t, protocolsession.MessageRequestBlocks, id186, unknownBody))
	select {
	case failure := <-outbound.failures:
		if failure.Scope != RevisionErrorScope || failure.Code != RevisionCodeInvalidLease {
			t.Fatalf("unknown lease failure=%+v", failure)
		}
	case <-time.After(time.Second):
		t.Fatal("unknown lease produced no operation error")
	}
}
