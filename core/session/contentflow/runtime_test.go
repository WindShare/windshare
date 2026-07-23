package contentflow

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
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

func (ids *runtimeIDs) NewFileRevision() (content.FileRevision, error) {
	return flowID[content.FileRevision](ids.value()), nil
}
func (ids *runtimeIDs) NewLeaseID() (content.LeaseID, error) {
	return flowID[content.LeaseID](ids.value()), nil
}

type runtimeFixture struct {
	share   catalog.ShareInstance
	file    catalog.FileID
	stable  *runtimeStableFile
	clock   *runtimeClock
	store   *content.RevisionStore
	cache   *SharedBlockCache
	service *SenderService
	opener  *records.Opener
	quota   *content.QuotaAccount
}

func quotaAccount(t *testing.T, name string) *content.QuotaAccount {
	t.Helper()
	account, err := content.NewQuotaAccount(name, content.QuotaLimits{StableHandles: 100, ActiveLeases: 100})
	if err != nil {
		t.Fatal(err)
	}
	return account
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
	locator, _ := catalog.NewLocator(0, "file.bin")
	sourceIdentity, _ := catalog.NewSourceIdentity([]byte("runtime-source"))
	candidate, _ := catalog.NewVersionCandidate([]byte("runtime-candidate"))
	record, err := catalog.NewFileNodeRecord(file, parent, "file.bin", locator, sourceIdentity, candidate, size, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	processCache, _ := NewProcessCacheBudget(DefaultProcessSealedCacheBytes)
	cache, _ := NewSharedBlockCache(share, uint64(catalog.MinChunkSize)*4, processCache)
	clock := &runtimeClock{now: time.Unix(1_000, 0)}
	stable := &runtimeStableFile{data: data}
	store, err := content.NewRevisionStore(content.RevisionStoreConfig{
		ShareInstance: share, ChunkSize: catalog.MinChunkSize,
		Catalog: runtimeCatalog{records: map[catalog.NodeID]catalog.NodeRecord{file.NodeID(): record}},
		Source:  runtimeSource{file: stable}, ProcessQuota: quotaAccount(t, "process"), ShareQuota: quotaAccount(t, "share"),
		Clock: clock, IDs: &runtimeIDs{}, CacheInvalidator: cache,
	})
	if err != nil {
		t.Fatal(err)
	}
	readSecret := bytes.Repeat([]byte{0x66}, content.ReadSecretBytes)
	keys, _ := content.NewKeyTree(readSecret, share)
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x77}, ed25519.SeedSize))
	sealer, err := records.NewSealer(records.SealerConfig{
		ShareInstance: share, Keys: keys, SigningKey: privateKey, NonceSource: rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	opener, _ := records.NewOpener(records.OpenerConfig{
		ShareInstance: share, Keys: keys, VerificationKey: privateKey.Public().(ed25519.PublicKey),
	})
	sessionQuota := quotaAccount(t, "session")
	service, err := NewSenderService(SenderServiceConfig{Store: store, SessionQuota: sessionQuota, Sealer: sealer, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	return runtimeFixture{
		share: share, file: file, stable: stable, clock: clock, store: store,
		cache: cache, service: service, opener: opener, quota: sessionQuota,
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
	descriptor, err := fixture.opener.OpenRevision(fixture.file, catalog.MinChunkSize, items[1].RevisionObject)
	if err != nil || descriptor != lease.Descriptor() {
		t.Fatalf("revision object descriptor=%+v err=%v", descriptor, err)
	}
	if bytes.Contains(items[1].RevisionObject, fixture.file.Bytes()) {
		t.Fatal("OPEN_RESULTS exposed FileID outside the encrypted revision object body")
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
	opened, err := fixture.opener.OpenBlock(descriptor, 1, object)
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
	fixture := newRuntimeFixture(t, 1)
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
	fixture.cache.InvalidateRevision(first.Descriptor().FileID(), first.Descriptor().FileRevision())
	fixture.stable.setDrifted()
	secondRequest, _ := NewBlockRequest(second.ID(), []uint64{0})
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
	fixture := newRuntimeFixture(t, 1)
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
	fixture.cache.InvalidateRevision(lease.Descriptor().FileID(), lease.Descriptor().FileRevision())
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
			return []byte("sealed"), nil
		}
	}
	firstContext, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() { _, err := cache.Get(firstContext, key, loader); firstResult <- err }()
	<-started
	secondResult := make(chan []byte, 1)
	go func() { object, _ := cache.Get(context.Background(), key, loader); secondResult <- object }()
	deadline := time.Now().Add(time.Second)
	for {
		cache.mu.Lock()
		joined := cache.inflight[key] != nil && cache.inflight[key].waiters == 2
		cache.mu.Unlock()
		if joined {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second cache consumer did not join the shared load")
		}
		time.Sleep(time.Millisecond)
	}
	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("first consumer error=%v", err)
	}
	close(release)
	if object := <-secondResult; !bytes.Equal(object, []byte("sealed")) {
		t.Fatalf("second consumer object=%q", object)
	}
	if object, err := cache.Get(context.Background(), key, loader); err != nil || !bytes.Equal(object, []byte("sealed")) || loads.Load() != 1 {
		t.Fatalf("cache hit object=%q loads=%d err=%v", object, loads.Load(), err)
	}

	keyOne, _ := NewBlockCacheKey(descriptor, 1)
	ignoredCancelStarted := make(chan struct{})
	ignoredCancelRelease := make(chan struct{})
	staleDone := make(chan error, 1)
	go func() {
		_, err := cache.Get(context.Background(), keyOne, func(context.Context) ([]byte, error) {
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
	loadsBefore := loads.Load()
	object, err := cache.Get(context.Background(), keyOne, func(context.Context) ([]byte, error) {
		loads.Add(1)
		return []byte("fresh"), nil
	})
	if !errors.Is(err, content.ErrRevisionDrift) || object != nil || loads.Load() != loadsBefore {
		t.Fatalf("permanently invalidated revision object=%q loads=%d err=%v", object, loads.Load(), err)
	}
	close(ignoredCancelRelease)
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

func (outbound *failingSemanticOutbound) SendControl(
	context.Context,
	protocolsession.MessageKind,
	protocolsession.OperationID,
	[]byte,
) (protocolsession.SendOutcome, error) {
	return protocolsession.SendOutcomeUnknown, outbound.err
}

func (outbound *failingSemanticOutbound) SendFragment(context.Context, protocolsession.Message) error {
	return outbound.err
}

func (outbound *failingSemanticOutbound) SendOperationError(context.Context, protocolsession.OperationID, OperationFailure) error {
	outbound.operationErrors.Add(1)
	return outbound.err
}

func newRecordingOutbound() *recordingOutbound {
	return &recordingOutbound{
		controls: make(chan protocolsession.MessageKind, 16), fragments: make(chan protocolsession.Message, 128),
		failures: make(chan OperationFailure, 16),
	}
}

func (outbound *recordingOutbound) SendControl(_ context.Context, kind protocolsession.MessageKind, _ protocolsession.OperationID, _ []byte) (protocolsession.SendOutcome, error) {
	outbound.controls <- kind
	return protocolsession.SendOutcomeDelivered, nil
}
func (outbound *recordingOutbound) SendFragment(_ context.Context, message protocolsession.Message) error {
	outbound.fragments <- message
	return nil
}
func (outbound *recordingOutbound) SendOperationError(_ context.Context, _ protocolsession.OperationID, failure OperationFailure) error {
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

func senderMessageContext(t *testing.T, message protocolsession.Message) context.Context {
	t.Helper()
	operations, err := protocolsession.NewOperationTable(
		protocolsession.OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := operations.ObserveInbound(protocolsession.DirectionReceiverToSender, message)
	if err != nil || admission.Disposition != protocolsession.OperationDeliver {
		t.Fatalf("admit sender handler message: disposition=%d error=%v", admission.Disposition, err)
	}
	return protocolsession.WithOperationGeneration(context.Background(), admission.Generation)
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
	request, _ := NewOpenRequest([]OpenItem{{FileID: fixture.file}})
	body, _ := EncodeOpenRequest(request)
	operation := flowID[protocolsession.OperationID](71)
	openMessage := operationMessage(t, protocolsession.MessageOpenRevisions, operation, body)
	if err := handler.HandleMessage(senderMessageContext(t, openMessage), openMessage); err != nil {
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
	invalidRenew := operationMessage(t, protocolsession.MessageRenewLease, flowID[protocolsession.OperationID](72), invalidBody)
	if err := handler.HandleMessage(senderMessageContext(t, invalidRenew), invalidRenew); err != nil {
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
	handler.process(context.Background(), operationMessage(t, protocolsession.MessageRenewLease, flowID[protocolsession.OperationID](173), leaseRequestBody))
	if failure := <-outbound.failures; failure.Scope != RevisionErrorScope {
		t.Fatalf("early renew failure scope=%d", failure.Scope)
	}
	fixture.clock.Advance(content.LeaseRenewWindow + time.Second)
	handler.process(context.Background(), operationMessage(t, protocolsession.MessageRenewLease, flowID[protocolsession.OperationID](74), leaseRequestBody))
	if kind := <-outbound.controls; kind != protocolsession.MessageLeaseResult {
		t.Fatalf("renew control kind=%d", kind)
	}
	blockRequest, _ := NewBlockRequest(directLease.ID(), []uint64{0})
	blockBody, _ := EncodeBlockRequest(blockRequest)
	handler.process(context.Background(), operationMessage(t, protocolsession.MessageRequestBlocks, flowID[protocolsession.OperationID](75), blockBody))
	if kind := <-outbound.controls; kind != protocolsession.MessageOperationComplete {
		t.Fatalf("block final kind=%d", kind)
	}
	if len(outbound.fragments) == 0 {
		t.Fatal("block handler emitted no fragments")
	}
	handler.process(context.Background(), operationMessage(t, protocolsession.MessageReleaseLease, flowID[protocolsession.OperationID](76), leaseRequestBody))
	if kind := <-outbound.controls; kind != protocolsession.MessageOperationComplete {
		t.Fatalf("release final kind=%d", kind)
	}

	operations, _ := protocolsession.NewOperationTable(protocolsession.OperationLimits{MaxActive: 16, MaxTombstones: 32}, nil)
	router, _ := protocolsession.NewRoleRouter(protocolsession.RoleSender, operations)
	if err := RegisterSenderHandlers(router, handler); err != nil {
		t.Fatalf("register content handlers: %v", err)
	}
	if err := RegisterSenderHandlers(router, handler); err == nil {
		t.Fatal("duplicate content handler registration accepted")
	}
	if err := handler.HandleMessage(context.Background(), operationMessage(t, protocolsession.MessageCatalogResult, flowID[protocolsession.OperationID](73), completeBody(t))); !errors.Is(err, ErrUnexpectedMessage) {
		t.Fatalf("unexpected kind error=%v", err)
	}
	invalidCancel, _ := protocolsession.EncodeBody([]any{uint64(0)})
	if err := handler.HandleMessage(context.Background(), operationMessage(t, protocolsession.MessageCancel, flowID[protocolsession.OperationID](77), invalidCancel)); !errors.Is(err, ErrInvalidCancelRequest) {
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
	handler.process(context.Background(), operationMessage(
		t, protocolsession.MessageReleaseLease, flowID[protocolsession.OperationID](78), body,
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
	first := operationMessage(t, protocolsession.MessageOpenRevisions, flowID[protocolsession.OperationID](81), body)
	second := operationMessage(t, protocolsession.MessageOpenRevisions, flowID[protocolsession.OperationID](82), body)
	if err := handler.HandleMessage(senderMessageContext(t, first), first); err != nil {
		t.Fatal(err)
	}
	if err := handler.HandleMessage(senderMessageContext(t, second), second); !errors.Is(err, ErrServiceQueueFull) {
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
		content.ErrQuotaExceeded:        RevisionCodeQuota,
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
		content.ErrInvalidBlockRef: BlockCodeInvalidRef,
		content.ErrBlockOutOfRange: BlockCodeOutOfRange,
		records.ErrObjectAuth:      BlockCodeObjectAuth,
		ErrFragmentConflict:        BlockCodeFragmentConflict,
		ErrFragmentTimeout:         BlockCodeTimeout,
		context.Canceled:           BlockCodeCancelled,
	} {
		if code := classifyBlockError(err); code != expected {
			t.Fatalf("block error %v code=%#x", err, code)
		}
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
	service, cache := stubService(t, lease.Descriptor(), store, stubRecordSealer{revision: []byte("revision")})
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
	handler.process(context.Background(), operationMessage(t, protocolsession.MessageRequestBlocks, flowID[protocolsession.OperationID](184), body))
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
	handler.process(context.Background(), operationMessage(t, protocolsession.MessageRequestBlocks, flowID[protocolsession.OperationID](186), unknownBody))
	select {
	case failure := <-outbound.failures:
		if failure.Scope != RevisionErrorScope || failure.Code != RevisionCodeInvalidLease {
			t.Fatalf("unknown lease failure=%+v", failure)
		}
	case <-time.After(time.Second):
		t.Fatal("unknown lease produced no operation error")
	}
}
