package receiversession

import (
	"bytes"
	"context"
	"errors"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/internal/sessioncore"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sendersession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
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
	sender          *sendersession.Runtime
	receiver        *Runtime
	receiverChannel *memoryChannel
	receiverFrames  atomic.Int32
}

func connectAuditedVerticalPair(
	t *testing.T,
	senderFactory *sendersession.Factory,
	receiverFactory *Factory,
) *auditedVerticalPair {
	t.Helper()
	senderChannel, receiverChannel := newMemoryChannelPair()
	accepted := make(chan struct {
		runtime *sendersession.Runtime
		err     error
	}, 1)
	go func() {
		runtime, err := senderFactory.Accept(context.Background(), senderChannel)
		accepted <- struct {
			runtime *sendersession.Runtime
			err     error
		}{runtime: runtime, err: err}
	}()
	receiver, err := receiverFactory.Connect(context.Background(), receiverChannel, transferlanes.RouteRelay)
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
	call                *sessioncore.Call
	receiverGeneration  protocolsession.OperationGeneration
	senderGeneration    protocolsession.OperationGeneration
	receiverTombstones  int
	senderTombstones    int
	expectedRequestKind protocolsession.MessageKind
}

func captureExactOperationLifecycle(
	t *testing.T,
	pair *auditedVerticalPair,
	senderContext context.Context,
	receiverTombstones int,
	senderTombstones int,
	expectedKind protocolsession.MessageKind,
) exactOperationLifecycle {
	t.Helper()
	// The sender can enter its handler before the receiver's physical-send
	// receipt stores outbound authority. Quiescence makes the snapshot describe
	// the admitted operation instead of that intentionally transient interval.
	synctest.Wait()
	call := onlyActiveCall(t, pair.receiver.rpc)
	receiverGeneration, authority := call.OperationAuthority()
	senderGeneration, ok := protocolsession.OperationGenerationFromContext(senderContext, call.ID())
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
	clearedGeneration, clearedAuthority := operation.call.OperationAuthority()
	if !clearedGeneration.IsZero() || !clearedAuthority.IsZero() {
		t.Fatal("retired receiver call retained authority")
	}
	assertRequestsRetired(t, pair.receiver.rpc)
}

func newLocalFailureSenderFactory(
	t *testing.T,
	fixture *verticalFixture,
	catalogFactory sendersession.CatalogFactory,
	contentFactory sendersession.ContentFactory,
) *sendersession.Factory {
	t.Helper()
	base := fixture.senderConfig
	if catalogFactory == nil {
		catalogFactory = base.Catalog
	}
	if contentFactory == nil {
		contentFactory = base.Content
	}
	factory, err := sendersession.NewFactory(sendersession.FactoryConfig{
		ShareInstance: fixture.share, SessionAuthKey: base.SessionAuthKey, SenderPrivateKey: base.SenderPrivateKey,
		Catalog: catalogFactory, Content: contentFactory, Peers: base.Peers,
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
	configure func(*FactoryConfig),
) *Factory {
	t.Helper()
	config := fixture.receiverConfig
	config.Random = &deterministicReader{next: 41}
	if configure != nil {
		configure(&config)
	}
	factory, err := NewFactory(config)
	if err == nil {
		trackReceiverFactory(factory)
	}
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
) sendersession.ContentFactory {
	t.Helper()
	readSecret := bytes.Repeat([]byte{localFailureTestReadSecretByte}, link.ReadSecretBytes)
	keys, err := content.NewKeyTree(readSecret, fixture.share)
	clear(readSecret)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := records.NewSealer(records.SealerConfig{
		ShareInstance: fixture.share, Keys: keys, SigningKey: fixture.senderConfig.SenderPrivateKey,
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
	return sendersession.ContentFactoryFunc(func(sessionID protocolsession.ProtocolSessionID) (*contentflow.SenderService, error) {
		sessionCapacity, err := fixture.registerCapacitySession(sessionID)
		if err != nil {
			return nil, err
		}
		return contentflow.NewSenderService(contentflow.SenderServiceConfig{
			Store: store, SessionCapacity: sessionCapacity, Sealer: sealer, Cache: cache,
		})
	})
}

func TestBlockLocalFailuresCancelExactOperationAndPreserveSibling(t *testing.T) {
	localOpenFailure := errors.New("local block object rejected")
	tests := []struct {
		name        string
		want        error
		failOpening bool
		inject      func(*testing.T, *sessioncore.Call)
	}{
		{
			name: "malformed authenticated fragment",
			want: contentflow.ErrFragmentMalformed,
			inject: func(t *testing.T, call *sessioncore.Call) {
				fragments, err := contentflow.FragmentRecord(
					call.ID(), bytes.Repeat([]byte{1}, contentflow.MaxFragmentPayloadBytes+1),
				)
				if err != nil {
					t.Fatal(err)
				}
				plaintext := fragments[0].Body()
				plaintext[48] ^= 0x80
				sendInjectedBlockFragment(t, call, decodeInjectedFragment(t, plaintext))
			},
		},
		{
			name: "conflicting authenticated fragment",
			want: contentflow.ErrFragmentConflict,
			inject: func(t *testing.T, call *sessioncore.Call) {
				fragments, err := contentflow.FragmentRecord(
					call.ID(), bytes.Repeat([]byte{2}, contentflow.MaxFragmentPayloadBytes+1),
				)
				if err != nil {
					t.Fatal(err)
				}
				sendInjectedBlockFragment(t, call, fragments[0])
				plaintext := fragments[0].Body()
				plaintext[len(plaintext)-1] ^= 0xff
				sendInjectedBlockFragment(t, call, decodeInjectedFragment(t, plaintext))
			},
		},
		{
			name:        "OpenBlock failure after nonfinal fragment",
			want:        localOpenFailure,
			failOpening: true,
			inject: func(t *testing.T, call *sessioncore.Call) {
				fragments, err := contentflow.FragmentRecord(
					call.ID(), bytes.Repeat([]byte{3}, contentflow.MaxFragmentPayloadBytes+1),
				)
				if err != nil || len(fragments) != 2 {
					t.Fatalf("two-fragment record: count=%d error=%v", len(fragments), err)
				}
				for _, fragment := range fragments {
					sendInjectedBlockFragment(t, call, fragment)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const chunkSize = 64 << 10
				fixture := newVerticalFixtureWithContent(t, make([]byte, chunkSize), chunkSize)
				blockGate := make(chan struct{})
				var releaseGate sync.Once
				t.Cleanup(func() { releaseGate.Do(func() { close(blockGate) }) })
				fixture.contentStore.blockStart = make(chan uint64, 1)
				fixture.contentStore.blockGate = blockGate
				fixture.contentStore.blockStop = make(chan struct{}, 2)
				blockContext := make(chan context.Context, 1)
				contentFactory := newContextCapturingContentFactory(t, fixture, blockContext)
				senderFactory := newLocalFailureSenderFactory(t, fixture, nil, contentFactory)
				receiverFactory := newLocalFailureReceiverFactory(t, fixture, func(config *FactoryConfig) {
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
				receiverTombstones := 0
				senderTombstones := 0
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
				// The sender can enter its blocked read before the receiver binds the
				// settled request authority. Wait for that independent receipt path
				// before capturing the exact two-sided lifecycle.
				synctest.Wait()
				operation := captureExactOperationLifecycle(
					t, pair, senderContext, receiverTombstones, senderTombstones,
					protocolsession.MessageRequestBlocks,
				)
				pair.beginReceiverFrameAudit()
				test.inject(t, operation.call)
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
				assertExactOperationDrained(t, pair, operation)
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
	call *sessioncore.Call,
	message protocolsession.Message,
) {
	t.Helper()
	if err := deliverCallResponse(call, message); err != nil {
		t.Fatalf("inject authenticated block response: %v", err)
	}
}

func decodeInjectedFragment(t *testing.T, plaintext []byte) protocolsession.Message {
	t.Helper()
	message, err := protocolsession.DecodeMessage(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return message
}
