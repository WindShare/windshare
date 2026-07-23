package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/transfer"
)

type receiverOperationGate struct {
	entered      chan struct{}
	stopReturned chan struct{}
	release      chan struct{}
	invokeOnce   sync.Once
	releaseOnce  sync.Once
	runtime      *ReceiverRuntime
}

func newReceiverOperationGate() *receiverOperationGate {
	return &receiverOperationGate{
		entered: make(chan struct{}), stopReturned: make(chan struct{}), release: make(chan struct{}),
	}
}

func (gate *receiverOperationGate) blockAndBeginClose() {
	gate.invokeOnce.Do(func() {
		close(gate.entered)
		gate.runtime.BeginClose()
		close(gate.stopReturned)
		<-gate.release
	})
}

func (gate *receiverOperationGate) unblock() {
	gate.releaseOnce.Do(func() { close(gate.release) })
}

type receiverOperationLease struct {
	once     sync.Once
	released chan struct{}
	count    atomic.Int32
}

func newReceiverOperationLease() *receiverOperationLease {
	return &receiverOperationLease{released: make(chan struct{})}
}

func (lease *receiverOperationLease) Release() {
	lease.count.Add(1)
	lease.once.Do(func() { close(lease.released) })
}

type receiverOperationResourceSource struct {
	lease *receiverOperationLease
}

type receiverOperationVerifierFunc func(
	context.Context,
	catalog.ShareInstance,
	catalogflow.ListRequest,
	[]byte,
) (catalogflow.VerifiedObject, error)

func (verify receiverOperationVerifierFunc) Verify(
	ctx context.Context,
	instance catalog.ShareInstance,
	request catalogflow.ListRequest,
	object []byte,
) (catalogflow.VerifiedObject, error) {
	return verify(ctx, instance, request, object)
}

func (source receiverOperationResourceSource) AcquireReceiverRuntimeResources() (
	ReceiverRuntimeResourceLease,
	error,
) {
	return source.lease, nil
}

func TestReceiverCloseJoinsCatalogProgressObserverBeforeResourceRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newVerticalFixture(t)
		gate := newReceiverOperationGate()
		config := fixture.receiverConfig
		config.CatalogProgress = CatalogScanProgressObserverFunc(func(
			context.Context,
			CatalogScanProgress,
		) error {
			gate.blockAndBeginClose()
			return nil
		})
		exerciseReceiverCatalogCallbackLifecycle(t, fixture, config, gate)
	})
}

type receiverOperationBlockLane struct{}

func (receiverOperationBlockLane) FetchBlock(
	context.Context,
	transfer.BlockDemand,
) (records.BlockRecord, error) {
	return records.BlockRecord{}, errors.New("unexpected receiver operation block fetch")
}

func TestReceiverBeginCloseFreezesCachedBlocksAndLaneMutationBeforeJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newVerticalFixture(t)
		gate := newReceiverOperationGate()
		config := fixture.receiverConfig
		config.CatalogProgress = CatalogScanProgressObserverFunc(func(
			context.Context,
			CatalogScanProgress,
		) error {
			gate.blockAndBeginClose()
			return nil
		})
		receiverFactory, err := NewReceiverFactory(config)
		if err != nil {
			t.Fatal(err)
		}
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
		gate.runtime = receiver
		t.Cleanup(func() {
			gate.unblock()
			receiver.Close()
			sender.Close()
			receiverFactory.Close()
		})

		opened, err := receiver.OpenRevision(context.Background(), fixture.fileID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := receiver.BlockBroker().GetBlock(
			context.Background(), opened.LeaseID, opened.Descriptor, 0,
		); err != nil {
			t.Fatal(err)
		}
		if receiver.BlockBroker().UsedBytes() == 0 {
			t.Fatal("block broker did not retain the pre-close plaintext probe")
		}

		loadDone := make(chan error, 1)
		go func() {
			_, loadErr := receiver.Catalog().LoadDirectory(context.Background(), fixture.directoryID)
			loadDone <- loadErr
		}()
		<-fixture.scanStarted
		close(fixture.scanGate)
		<-gate.entered
		select {
		case <-gate.stopReturned:
		case <-time.After(time.Second):
			t.Fatal("catalog callback deadlocked while freezing receiver components")
		}

		closeDone := make(chan struct{})
		go func() {
			receiver.Close()
			close(closeDone)
		}()
		synctest.Wait()
		select {
		case <-closeDone:
			t.Fatal("receiver Close crossed the gated catalog callback")
		default:
		}
		if _, err := receiver.BlockBroker().GetBlock(
			context.Background(), opened.LeaseID, opened.Descriptor, 0,
		); !errors.Is(err, transfer.ErrBrokerClosed) {
			t.Fatalf("cached block remained readable after BeginClose: %v", err)
		}
		if receiver.BlockBroker().UsedBytes() != 0 {
			t.Fatalf("BeginClose retained %d plaintext bytes", receiver.BlockBroker().UsedBytes())
		}
		if err := receiver.LaneSet().Add(
			transfer.LaneIdentity{ID: 99, Epoch: 1}, receiverOperationBlockLane{},
		); !errors.Is(err, transfer.ErrLaneClosed) {
			t.Fatalf("lane mutation admitted after BeginClose: %v", err)
		}

		gate.unblock()
		synctest.Wait()
		if loadErr := <-loadDone; !errors.Is(loadErr, catalogflow.ErrClientClosed) {
			t.Fatalf("catalog load after close = %v", loadErr)
		}
		<-closeDone
	})
}

func TestReceiverTransferDependenciesPromoteClosedRuntimeToSessionFailure(t *testing.T) {
	fixture := newVerticalFixture(t)
	receiverFactory, err := NewReceiverFactory(fixture.receiverConfig)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	t.Cleanup(func() {
		receiver.Close()
		sender.Close()
		receiverFactory.Close()
	})

	opened, err := receiver.OpenRevision(context.Background(), fixture.fileID)
	if err != nil {
		t.Fatal(err)
	}
	dependencies := receiverTransferDependencies{runtime: receiver}
	_, release, liveCatalogErr := dependencies.AcquireDirectory(context.Background(), catalog.DirectoryID{})
	if release == nil {
		t.Fatal("live catalog validation omitted its mandatory release callback")
	}
	release()
	if liveCatalogErr == nil || transfer.IsSessionFailure(liveCatalogErr) {
		t.Fatalf("live catalog validation error was promoted to session failure: %v", liveCatalogErr)
	}
	if err := dependencies.ReadRange(
		context.Background(), opened.LeaseID, opened.Descriptor,
		content.Range{Offset: 0, End: 1}, nil,
	); err == nil || transfer.IsSessionFailure(err) {
		t.Fatalf("live range validation error was promoted to session failure: %v", err)
	}

	receiver.BeginClose()
	receiver.WaitClosed()

	_, release, catalogErr := dependencies.AcquireDirectory(context.Background(), fixture.directoryID)
	if release == nil {
		t.Fatal("closed catalog dependency omitted its mandatory release callback")
	}
	release()
	assertReceiverTransferSessionFailure(t, "catalog", catalogErr, catalogflow.ErrClientClosed)
	_, revisionErr := dependencies.OpenRevision(context.Background(), fixture.fileID)
	assertReceiverTransferSessionFailure(t, "revision open", revisionErr, ErrRuntimeClosed)
	releaseErr := dependencies.ReleaseRevision(context.Background(), opened.LeaseID)
	assertReceiverTransferSessionFailure(t, "revision release", releaseErr, ErrRuntimeClosed)
	blockErr := dependencies.ReadRange(
		context.Background(), opened.LeaseID, opened.Descriptor,
		content.Range{Offset: 0, End: 1},
		transfer.RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil }),
	)
	assertReceiverTransferSessionFailure(t, "block range", blockErr, transfer.ErrBrokerClosed)
}

func assertReceiverTransferSessionFailure(t *testing.T, name string, err, componentErr error) {
	t.Helper()
	if !errors.Is(err, componentErr) || !errors.Is(err, ErrRuntimeClosed) || !transfer.IsSessionFailure(err) {
		t.Fatalf("closed %s error = %v, want component, runtime-closed, and session-failure identities", name, err)
	}
}

func TestReceiverCloseJoinsCatalogVerifierBeforeResourceRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newVerticalFixture(t)
		gate := newReceiverOperationGate()
		config := fixture.receiverConfig
		delegate := config.CatalogVerifier
		config.CatalogVerifier = receiverOperationVerifierFunc(func(
			ctx context.Context,
			instance catalog.ShareInstance,
			request catalogflow.ListRequest,
			object []byte,
		) (catalogflow.VerifiedObject, error) {
			gate.blockAndBeginClose()
			return delegate.Verify(ctx, instance, request, object)
		})
		exerciseReceiverCatalogCallbackLifecycle(t, fixture, config, gate)
	})
}

func exerciseReceiverCatalogCallbackLifecycle(
	t *testing.T,
	fixture *verticalFixture,
	config ReceiverFactoryConfig,
	gate *receiverOperationGate,
) {
	t.Helper()
	resourceLease := newReceiverOperationLease()
	config.RuntimeResources = receiverOperationResourceSource{lease: resourceLease}
	receiverFactory, err := NewReceiverFactory(config)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	ownedPublicKey := receiver.publicKey
	gate.runtime = receiver
	t.Cleanup(func() {
		gate.unblock()
		receiver.Close()
		sender.Close()
		receiverFactory.Close()
	})

	loadDone := make(chan error, 1)
	go func() {
		_, loadErr := receiver.Catalog().LoadDirectory(context.Background(), fixture.directoryID)
		loadDone <- loadErr
	}()
	<-fixture.scanStarted
	close(fixture.scanGate)
	<-gate.entered
	select {
	case <-gate.stopReturned:
	case <-time.After(time.Second):
		t.Fatal("catalog callback deadlocked while beginning receiver close")
	}

	closeDone := make(chan struct{})
	go func() {
		receiver.Close()
		close(closeDone)
	}()
	synctest.Wait()
	assertReceiverOperationStillOwned(t, receiver, resourceLease, closeDone)
	if _, loadErr := receiver.Catalog().LoadDirectory(
		context.Background(),
		fixture.directoryID,
	); !errors.Is(loadErr, catalogflow.ErrClientClosed) {
		t.Fatalf("catalog load admitted after BeginClose: %v", loadErr)
	}

	gate.unblock()
	synctest.Wait()
	if loadErr := <-loadDone; !errors.Is(loadErr, catalogflow.ErrClientClosed) {
		t.Fatalf("catalog load after close = %v", loadErr)
	}
	<-resourceLease.released
	assertReceiverBorrowedGraphReleased(t, receiver, ownedPublicKey)
	<-closeDone
	if resourceLease.count.Load() != 1 {
		t.Fatalf("receiver resource releases = %d, want 1", resourceLease.count.Load())
	}
}

type gatedRevisionOpener struct {
	RecordOpener
	gate  *receiverOperationGate
	calls atomic.Int32
}

func (opener *gatedRevisionOpener) OpenRevision(
	fileID catalog.FileID,
	chunkSize uint32,
	object []byte,
) (content.FileRevisionDescriptor, error) {
	opener.calls.Add(1)
	opener.gate.blockAndBeginClose()
	return opener.RecordOpener.OpenRevision(fileID, chunkSize, object)
}

func (opener *gatedRevisionOpener) OpenBlock(
	descriptor content.FileRevisionDescriptor,
	index uint64,
	object []byte,
) (records.BlockRecord, error) {
	return opener.RecordOpener.OpenBlock(descriptor, index, object)
}

func TestReceiverCloseJoinsRevisionOpenerBeforeResourceRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newVerticalFixture(t)
		gate := newReceiverOperationGate()
		resourceLease := newReceiverOperationLease()
		config := fixture.receiverConfig
		opener := &gatedRevisionOpener{RecordOpener: config.RecordOpener, gate: gate}
		config.RecordOpener = opener
		config.RuntimeResources = receiverOperationResourceSource{lease: resourceLease}
		receiverFactory, err := NewReceiverFactory(config)
		if err != nil {
			t.Fatal(err)
		}
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
		ownedPublicKey := receiver.publicKey
		gate.runtime = receiver
		t.Cleanup(func() {
			gate.unblock()
			receiver.Close()
			sender.Close()
			receiverFactory.Close()
		})

		openDone := make(chan error, 1)
		go func() {
			_, openErr := receiver.OpenRevision(context.Background(), fixture.fileID)
			openDone <- openErr
		}()
		<-gate.entered
		select {
		case <-gate.stopReturned:
		case <-time.After(time.Second):
			t.Fatal("revision opener deadlocked while beginning receiver close")
		}

		closeDone := make(chan struct{})
		go func() {
			receiver.Close()
			close(closeDone)
		}()
		synctest.Wait()
		assertReceiverOperationStillOwned(t, receiver, resourceLease, closeDone)
		if _, openErr := receiver.OpenRevision(
			context.Background(),
			fixture.fileID,
		); !errors.Is(openErr, ErrRuntimeClosed) {
			t.Fatalf("revision open admitted after BeginClose: %v", openErr)
		}
		if opener.calls.Load() != 1 {
			t.Fatalf("closed receiver invoked revision opener %d times", opener.calls.Load())
		}
		if releaseErr := receiver.ReleaseRevision(
			context.Background(),
			fixture.contentStore.lease.ID(),
		); !errors.Is(releaseErr, ErrRuntimeClosed) {
			t.Fatalf("revision release admitted after BeginClose: %v", releaseErr)
		}

		gate.unblock()
		synctest.Wait()
		if openErr := <-openDone; !errors.Is(openErr, ErrRuntimeClosed) {
			t.Fatalf("in-flight revision open after close = %v", openErr)
		}
		<-resourceLease.released
		assertReceiverBorrowedGraphReleased(t, receiver, ownedPublicKey)
		<-closeDone
		if resourceLease.count.Load() != 1 {
			t.Fatalf("receiver resource releases = %d, want 1", resourceLease.count.Load())
		}
	})
}

func assertReceiverBorrowedGraphReleased(
	t *testing.T,
	receiver *ReceiverRuntime,
	ownedPublicKey []byte,
) {
	t.Helper()
	receiver.revisions.mu.Lock()
	retainedRevisionGraph := receiver.revisions.rpc != nil || receiver.revisions.opener != nil ||
		receiver.revisions.after != nil || receiver.revisions.ctx != nil ||
		receiver.revisions.cancel != nil || receiver.revisions.leases != nil
	revisionsClosed := receiver.revisions.closed
	receiver.revisions.mu.Unlock()
	if !revisionsClosed || retainedRevisionGraph {
		t.Fatalf(
			"closed revision client retained borrowed graph: closed=%v retained=%v",
			revisionsClosed,
			retainedRevisionGraph,
		)
	}
	if receiver.opener != nil || receiver.semantic != nil || receiver.publicKey != nil {
		t.Fatal("closed receiver runtime retained opener, semantic validator, or public-key aliases")
	}
	for index, value := range ownedPublicKey {
		if value != 0 {
			t.Fatalf("receiver public-key byte %d was not cleared", index)
		}
	}
	if _, err := receiver.OpenRevision(
		context.Background(), catalog.FileID{},
	); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("post-close revision open error = %v", err)
	}
	if err := receiver.ReleaseRevision(
		context.Background(), content.LeaseID{},
	); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("post-close revision release error = %v", err)
	}
}

func assertReceiverOperationStillOwned(
	t *testing.T,
	receiver *ReceiverRuntime,
	resourceLease *receiverOperationLease,
	closeDone <-chan struct{},
) {
	t.Helper()
	for name, done := range map[string]<-chan struct{}{
		"runtime Done":     receiver.Done(),
		"Close":            closeDone,
		"resource release": resourceLease.released,
	} {
		select {
		case <-done:
			t.Fatalf("%s completed while a callback still borrowed receiver resources", name)
		default:
		}
	}
	if resourceLease.count.Load() != 0 {
		t.Fatalf("receiver released resources %d times before callback exit", resourceLease.count.Load())
	}
}
