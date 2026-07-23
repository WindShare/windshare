package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const localFailureTestReadSecretByte = byte(5)

type blockFailureOpener struct {
	RecordOpener
	err error
}

func (opener blockFailureOpener) OpenBlock(
	content.FileRevisionDescriptor,
	uint64,
	[]byte,
) (records.BlockRecord, error) {
	return records.BlockRecord{}, opener.err
}

type contextCapturingContentStore struct {
	*verticalContentStore
	started chan context.Context
}

func (store *contextCapturingContentStore) ReadBlock(
	ctx context.Context,
	lease content.LeaseID,
	reference content.BlockRef,
) ([]byte, error) {
	select {
	case store.started <- ctx:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return store.verticalContentStore.ReadBlock(ctx, lease, reference)
}

type auditedVerticalPair struct {
	sender          *SenderRuntime
	receiver        *ReceiverRuntime
	receiverChannel *memoryChannel
	receiverFrames  atomic.Int32
}

func connectAuditedVerticalPair(
	t *testing.T,
	senderFactory *SenderFactory,
	receiverFactory *ReceiverFactory,
) *auditedVerticalPair {
	t.Helper()
	senderChannel, receiverChannel := newMemoryChannelPair()
	accepted := make(chan struct {
		runtime *SenderRuntime
		err     error
	}, 1)
	go func() {
		runtime, err := senderFactory.Accept(context.Background(), senderChannel)
		accepted <- struct {
			runtime *SenderRuntime
			err     error
		}{runtime: runtime, err: err}
	}()
	receiver, err := receiverFactory.Connect(context.Background(), receiverChannel)
	if err != nil {
		t.Fatalf("connect receiver: %v", err)
	}
	result := <-accepted
	if result.err != nil {
		receiver.Close()
		t.Fatalf("accept sender: %v", result.err)
	}
	pair := &auditedVerticalPair{
		sender: result.runtime, receiver: receiver, receiverChannel: receiverChannel,
	}
	t.Cleanup(func() {
		pair.receiver.Close()
		pair.sender.Close()
	})
	return pair
}

func (pair *auditedVerticalPair) beginReceiverFrameAudit() {
	pair.receiverFrames.Store(0)
	pair.receiverChannel.pipe.mu.Lock()
	pair.receiverChannel.onSend = func(framechannel.Frame) { pair.receiverFrames.Add(1) }
	pair.receiverChannel.pipe.mu.Unlock()
}

type exactOperationLifecycle struct {
	call                *operationCall
	receiverGeneration  protocolsession.OperationGeneration
	senderGeneration    protocolsession.OperationGeneration
	receiverTombstones  int
	senderTombstones    int
	expectedRequestKind protocolsession.MessageKind
}

type operationHandlerLifecycle struct {
	active  int
	running int
	queued  int
}

func contentHandlerLifecycle(sender *SenderRuntime) operationHandlerLifecycle {
	snapshot := sender.contentHandler.LifecycleSnapshot()
	return operationHandlerLifecycle{
		active: snapshot.ActiveOperations, running: snapshot.RunningWorkers, queued: snapshot.QueuedOperations,
	}
}

func catalogOperationLifecycle(sender *SenderRuntime) operationHandlerLifecycle {
	snapshot := sender.catalogHandler.lifecycleSnapshot()
	return operationHandlerLifecycle{
		active: snapshot.activeOperations, running: snapshot.runningWorkers, queued: snapshot.queuedOperations,
	}
}

func captureExactOperationLifecycle(
	t *testing.T,
	pair *auditedVerticalPair,
	senderContext context.Context,
	receiverTombstones int,
	senderTombstones int,
	expectedKind protocolsession.MessageKind,
	handlerLifecycle func(*SenderRuntime) operationHandlerLifecycle,
) exactOperationLifecycle {
	t.Helper()
	call := onlyActiveCall(t, pair.receiver.rpc)
	receiverGeneration, authority := call.operationAuthority()
	senderGeneration, ok := protocolsession.OperationGenerationFromContext(senderContext, call.id)
	if receiverGeneration.IsZero() || authority.IsZero() || !ok || senderGeneration.IsZero() {
		t.Fatal("operation did not retain exact receiver and sender generation authority")
	}
	for role, generation := range map[string]protocolsession.OperationGeneration{
		"receiver": receiverGeneration,
		"sender":   senderGeneration,
	} {
		requestKind, present := generation.RequestKind()
		if !generation.IsCurrent() || !generation.IsActive() || !present || requestKind != expectedKind {
			t.Fatalf("%s generation current=%v active=%v kind=%d present=%v", role,
				generation.IsCurrent(), generation.IsActive(), requestKind, present)
		}
	}
	if pair.receiver.operations.ActiveCount() != 1 ||
		pair.receiver.operations.TombstoneCount() != receiverTombstones || pair.receiver.routes.len() != 0 {
		t.Fatalf("receiver start active=%d tombstones=%d routes=%d",
			pair.receiver.operations.ActiveCount(), pair.receiver.operations.TombstoneCount(), pair.receiver.routes.len())
	}
	if pair.sender.operations.ActiveCount() != 1 ||
		pair.sender.operations.TombstoneCount() != senderTombstones || pair.sender.routes.len() != 1 ||
		pair.sender.routes.current(call.id) == nil {
		t.Fatalf("sender start active=%d tombstones=%d routes=%d exact-route=%v",
			pair.sender.operations.ActiveCount(), pair.sender.operations.TombstoneCount(), pair.sender.routes.len(),
			pair.sender.routes.current(call.id) != nil)
	}
	lifecycle := handlerLifecycle(pair.sender)
	if lifecycle.active != 1 || lifecycle.running != 1 || lifecycle.queued != 0 {
		t.Fatalf("sender handler start active=%d running=%d queued=%d",
			lifecycle.active, lifecycle.running, lifecycle.queued)
	}
	return exactOperationLifecycle{
		call: call, receiverGeneration: receiverGeneration, senderGeneration: senderGeneration,
		receiverTombstones: receiverTombstones, senderTombstones: senderTombstones,
		expectedRequestKind: expectedKind,
	}
}

func assertExactOperationDrained(
	t *testing.T,
	pair *auditedVerticalPair,
	operation exactOperationLifecycle,
	handlerLifecycle func(*SenderRuntime) operationHandlerLifecycle,
) {
	t.Helper()
	// The remote callback barrier proves cancellation reached the service. Waiting
	// for quiescence then distinguishes map cleanup from a still-running worker.
	synctest.Wait()
	for role, generation := range map[string]protocolsession.OperationGeneration{
		"receiver": operation.receiverGeneration,
		"sender":   operation.senderGeneration,
	} {
		requestKind, present := generation.RequestKind()
		if !generation.IsCurrent() || generation.IsActive() || !present || requestKind != operation.expectedRequestKind {
			t.Fatalf("%s cancelled generation current=%v active=%v kind=%d present=%v", role,
				generation.IsCurrent(), generation.IsActive(), requestKind, present)
		}
	}
	if pair.receiverFrames.Load() != 1 {
		t.Fatalf("local failure emitted %d receiver wire frames, want one exact-generation CANCEL",
			pair.receiverFrames.Load())
	}
	if pair.receiver.operations.ActiveCount() != 0 ||
		pair.receiver.operations.TombstoneCount() != operation.receiverTombstones+1 || pair.receiver.routes.len() != 0 {
		t.Fatalf("receiver drain active=%d tombstones=%d routes=%d",
			pair.receiver.operations.ActiveCount(), pair.receiver.operations.TombstoneCount(), pair.receiver.routes.len())
	}
	if pair.sender.operations.ActiveCount() != 0 ||
		pair.sender.operations.TombstoneCount() != operation.senderTombstones+1 || pair.sender.routes.len() != 0 {
		t.Fatalf("sender drain active=%d tombstones=%d routes=%d",
			pair.sender.operations.ActiveCount(), pair.sender.operations.TombstoneCount(), pair.sender.routes.len())
	}
	pair.receiver.rpc.mu.Lock()
	callCount := len(pair.receiver.rpc.calls)
	pair.receiver.rpc.mu.Unlock()
	operation.call.stateMu.Lock()
	callClosed, queuedResponses := operation.call.closed, len(operation.call.messages)
	clearedGeneration, clearedAuthority := operation.call.generation, operation.call.authority
	operation.call.stateMu.Unlock()
	if callCount != 0 || !callClosed || queuedResponses != 0 || !clearedGeneration.IsZero() || !clearedAuthority.IsZero() {
		t.Fatalf("receiver waiter drain calls=%d closed=%v queued=%d generation-cleared=%v authority-cleared=%v",
			callCount, callClosed, queuedResponses, clearedGeneration.IsZero(), clearedAuthority.IsZero())
	}
	lifecycle := handlerLifecycle(pair.sender)
	if lifecycle.active != 0 || lifecycle.running != 0 || lifecycle.queued != 0 {
		t.Fatalf("sender handler drain active=%d running=%d queued=%d",
			lifecycle.active, lifecycle.running, lifecycle.queued)
	}
}

func newLocalFailureSenderFactory(
	t *testing.T,
	fixture *verticalFixture,
	catalogFactory SenderCatalogFactory,
	contentFactory SenderContentFactory,
) *SenderFactory {
	t.Helper()
	base := fixture.senderFactory
	if catalogFactory == nil {
		catalogFactory = base.catalog
	}
	if contentFactory == nil {
		contentFactory = base.content
	}
	factory, err := NewSenderFactory(SenderFactoryConfig{
		ShareInstance: fixture.share, SessionAuthKey: base.authKey, SenderPrivateKey: base.privateKey,
		Catalog: catalogFactory, Content: contentFactory, Peers: base.peers,
		Random: &deterministicReader{next: 31}, TerminalConnectivity: &verticalTerminalConnectivity{},
		TerminalTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := factory.Stop(context.Background(), "Test cleanup"); err != nil {
			t.Errorf("stop local-failure sender factory: %v", err)
		}
	})
	return factory
}

func newLocalFailureReceiverFactory(
	t *testing.T,
	fixture *verticalFixture,
	configure func(*ReceiverFactoryConfig),
) *ReceiverFactory {
	t.Helper()
	config := fixture.receiverConfig
	config.Random = &deterministicReader{next: 41}
	if configure != nil {
		configure(&config)
	}
	factory, err := NewReceiverFactory(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(factory.Close)
	return factory
}

func newContextCapturingContentFactory(
	t *testing.T,
	fixture *verticalFixture,
	started chan context.Context,
) SenderContentFactory {
	t.Helper()
	readSecret := bytes.Repeat([]byte{localFailureTestReadSecretByte}, link.ReadSecretBytes)
	keys, err := content.NewKeyTree(readSecret, fixture.share)
	clear(readSecret)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := records.NewSealer(records.SealerConfig{
		ShareInstance: fixture.share, Keys: keys, SigningKey: fixture.senderFactory.privateKey,
		NonceSource: &deterministicReader{next: 51},
	})
	if err != nil {
		keys.Destroy()
		t.Fatal(err)
	}
	processBudget, err := contentflow.NewProcessCacheBudget(64 << 20)
	if err != nil {
		sealer.Destroy()
		keys.Destroy()
		t.Fatal(err)
	}
	cache, err := contentflow.NewSharedBlockCache(fixture.share, 16<<20, processBudget)
	if err != nil {
		sealer.Destroy()
		keys.Destroy()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cache.Close()
		sealer.Destroy()
		keys.Destroy()
	})
	store := &contextCapturingContentStore{verticalContentStore: fixture.contentStore, started: started}
	return SenderContentFactoryFunc(func() (*contentflow.SenderService, error) {
		quota, err := content.NewQuotaAccount("local-failure-session", content.DefaultSessionQuotaLimits())
		if err != nil {
			return nil, err
		}
		return contentflow.NewSenderService(contentflow.SenderServiceConfig{
			Store: store, SessionQuota: quota, Sealer: sealer, Cache: cache,
		})
	})
}

func TestBlockLocalFailuresCancelExactOperationAndPreserveSibling(t *testing.T) {
	localOpenFailure := errors.New("local block object rejected")
	tests := []struct {
		name        string
		want        error
		failOpening bool
		inject      func(*testing.T, *SenderRuntime, context.Context, *operationCall)
	}{
		{
			name: "malformed authenticated fragment",
			want: contentflow.ErrFragmentMalformed,
			inject: func(t *testing.T, sender *SenderRuntime, senderContext context.Context, call *operationCall) {
				fragments, err := contentflow.FragmentRecord(
					call.id, bytes.Repeat([]byte{1}, contentflow.MaxFragmentPayloadBytes+1),
				)
				if err != nil {
					t.Fatal(err)
				}
				plaintext := fragments[0].Body()
				plaintext[48] ^= 0x80
				sendInjectedBlockFragment(t, sender, senderContext, decodeInjectedFragment(t, plaintext))
			},
		},
		{
			name: "conflicting authenticated fragment",
			want: contentflow.ErrFragmentConflict,
			inject: func(t *testing.T, sender *SenderRuntime, senderContext context.Context, call *operationCall) {
				fragments, err := contentflow.FragmentRecord(
					call.id, bytes.Repeat([]byte{2}, contentflow.MaxFragmentPayloadBytes+1),
				)
				if err != nil {
					t.Fatal(err)
				}
				sendInjectedBlockFragment(t, sender, senderContext, fragments[0])
				plaintext := fragments[0].Body()
				plaintext[len(plaintext)-1] ^= 0xff
				sendInjectedBlockFragment(t, sender, senderContext, decodeInjectedFragment(t, plaintext))
			},
		},
		{
			name:        "OpenBlock failure after nonfinal fragment",
			want:        localOpenFailure,
			failOpening: true,
			inject: func(t *testing.T, sender *SenderRuntime, senderContext context.Context, call *operationCall) {
				fragments, err := contentflow.FragmentRecord(
					call.id, bytes.Repeat([]byte{3}, contentflow.MaxFragmentPayloadBytes+1),
				)
				if err != nil || len(fragments) != 2 {
					t.Fatalf("two-fragment record: count=%d error=%v", len(fragments), err)
				}
				for _, fragment := range fragments {
					sendInjectedBlockFragment(t, sender, senderContext, fragment)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newVerticalFixture(t)
				blockGate := make(chan struct{})
				var releaseGate sync.Once
				t.Cleanup(func() { releaseGate.Do(func() { close(blockGate) }) })
				fixture.contentStore.blockStart = make(chan uint64, 1)
				fixture.contentStore.blockGate = blockGate
				fixture.contentStore.blockStop = make(chan struct{}, 2)
				blockContext := make(chan context.Context, 1)
				contentFactory := newContextCapturingContentFactory(t, fixture, blockContext)
				senderFactory := newLocalFailureSenderFactory(t, fixture, nil, contentFactory)
				receiverFactory := newLocalFailureReceiverFactory(t, fixture, func(config *ReceiverFactoryConfig) {
					config.After = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
					if test.failOpening {
						config.RecordOpener = blockFailureOpener{RecordOpener: config.RecordOpener, err: test.want}
					}
				})
				pair := connectAuditedVerticalPair(t, senderFactory, receiverFactory)
				opened, err := pair.receiver.OpenRevision(context.Background(), fixture.fileID)
				if err != nil {
					t.Fatal(err)
				}
				receiverTombstones := pair.receiver.operations.TombstoneCount()
				senderTombstones := pair.sender.operations.TombstoneCount()
				result := make(chan error, 1)
				go func() {
					_, err := pair.receiver.BlockBroker().GetBlock(
						context.Background(), opened.LeaseID, opened.Descriptor, 0,
					)
					result <- err
				}()
				var senderContext context.Context
				select {
				case senderContext = <-blockContext:
				case <-time.After(time.Second):
					t.Fatal("sender block handler did not expose its operation context")
				}
				select {
				case <-fixture.contentStore.blockStart:
				case <-time.After(time.Second):
					t.Fatal("sender block read did not reach the cancellation gate")
				}
				operation := captureExactOperationLifecycle(
					t, pair, senderContext, receiverTombstones, senderTombstones,
					protocolsession.MessageRequestBlocks, contentHandlerLifecycle,
				)
				pair.beginReceiverFrameAudit()
				test.inject(t, pair.sender, senderContext, operation.call)
				select {
				case err := <-result:
					if !errors.Is(err, test.want) {
						t.Fatalf("local block failure=%v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("local block failure did not finish its public caller")
				}
				select {
				case <-fixture.contentStore.blockStop:
				case <-time.After(time.Second):
					t.Fatal("local block failure did not cancel the exact remote read")
				}
				assertExactOperationDrained(t, pair, operation, contentHandlerLifecycle)
				if pair.receiver.assembler.ActiveRecords() != 0 {
					t.Fatalf("local block failure retained %d authenticated assemblies",
						pair.receiver.assembler.ActiveRecords())
				}
				select {
				case <-fixture.contentStore.blockStop:
					t.Fatal("local block failure canceled the remote read more than once")
				default:
				}
				if pair.receiver.Err() != nil || pair.sender.Err() != nil {
					t.Fatalf("operation-local block failure terminated session: receiver=%v sender=%v",
						pair.receiver.Err(), pair.sender.Err())
				}
				if _, err := pair.receiver.RequestLane(context.Background(), 0); err != nil {
					t.Fatalf("block failure damaged sibling operation: %v", err)
				}
				if err := pair.receiver.ReleaseRevision(context.Background(), opened.LeaseID); err != nil {
					t.Fatalf("release after block failure: %v", err)
				}
			})
		})
	}
}

func sendInjectedBlockFragment(
	t *testing.T,
	sender *SenderRuntime,
	senderContext context.Context,
	message protocolsession.Message,
) {
	t.Helper()
	ctx := protocolsession.RetainMessageContext(context.Background(), senderContext)
	if err := sender.outbound.SendFragment(ctx, message); err != nil {
		t.Fatalf("send authenticated block fault over the session lane: %v", err)
	}
}

func onlyActiveCall(t *testing.T, rpc *rpcClient) *operationCall {
	t.Helper()
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if len(rpc.calls) != 1 {
		t.Fatalf("active RPC calls=%d, want 1", len(rpc.calls))
	}
	for _, call := range rpc.calls {
		return call
	}
	return nil
}

func decodeInjectedFragment(t *testing.T, plaintext []byte) protocolsession.Message {
	t.Helper()
	message, err := protocolsession.DecodeMessage(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return message
}
