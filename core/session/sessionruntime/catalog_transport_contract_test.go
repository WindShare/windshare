package sessionruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func TestCatalogProgressObserverFailureStaysOperationLocal(t *testing.T) {
	if err := (CatalogScanProgressObserverFunc(nil)).ObserveCatalogScanProgress(
		context.Background(), CatalogScanProgress{},
	); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil catalog progress observer error = %v", err)
	}
	fixture := newVerticalFixture(t)
	wantObserverErr := errors.New("progress consumer stopped")
	receiverConfig := fixture.receiverConfig
	receiverConfig.CatalogProgress = CatalogScanProgressObserverFunc(
		func(context.Context, CatalogScanProgress) error { return wantObserverErr },
	)
	receiverFactory, err := NewReceiverFactory(receiverConfig)
	if err != nil {
		t.Fatal(err)
	}
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	result := make(chan error, 1)
	go func() {
		_, err := receiver.Catalog().LoadDirectory(context.Background(), fixture.directoryID)
		result <- err
	}()
	<-fixture.scanStarted
	close(fixture.scanGate)
	if err := <-result; !errors.Is(err, wantObserverErr) {
		t.Fatalf("catalog progress observer error = %v", err)
	}
	if receiver.Err() != nil {
		t.Fatalf("operation-local observer failure terminated ProtocolSession: %v", receiver.Err())
	}
}

func TestCatalogTransportRejectsHostileOperationResponses(t *testing.T) {
	request, err := catalogflow.NewListRequest(id16[catalog.DirectoryID](91), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (rpcCatalogTransport{}).FetchPage(context.Background(), catalogflow.ListRequest{}); !errors.Is(err, catalogflow.ErrInvalidRequest) {
		t.Fatalf("invalid list request error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (rpcCatalogTransport{rpc: &rpcClient{}}).FetchPage(cancelled, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled list request error = %v", err)
	}

	runCatalogTransportSynctest(t, "await cancellation remains operation local", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		harness := newBlockedCatalogFetch(t, ctx)
		cancel()
		if err := harness.wait(); !errors.Is(err, context.Canceled) {
			t.Fatalf("catalog await cancellation error = %v", err)
		}
		if harness.receiver.Err() != nil {
			t.Fatalf("catalog cancellation terminated ProtocolSession: %v", harness.receiver.Err())
		}
	})

	runCatalogTransportSynctest(t, "malformed signed wrapper is session failure", func(t *testing.T) {
		harness := newBlockedCatalogFetch(t, context.Background())
		message, err := protocolsession.NewMessage(
			protocolsession.MessageScanProgress, &harness.call.id, []byte{1},
		)
		if err != nil {
			t.Fatal(err)
		}
		enqueueCallResponse(harness.call, message)
		assertSessionFailure(t, harness.wait())
	})

	runCatalogTransportSynctest(t, "malformed progress semantic is session failure", func(t *testing.T) {
		harness := newBlockedCatalogFetch(t, context.Background())
		enqueueCallResponse(harness.call, signedPeerOperationControl(
			t, harness.receiver, harness.fixture.senderFactory.privateKey,
			protocolsession.MessageScanProgress, harness.call.id, []byte{1},
		))
		err := harness.wait()
		assertSessionFailure(t, err)
		if !errors.Is(err, ErrScanProgress) {
			t.Fatalf("malformed progress error = %v", err)
		}
	})

	runCatalogTransportSynctest(t, "duplicate progress is coalesced before unexpected final", func(t *testing.T) {
		harness := newBlockedCatalogFetch(t, context.Background())
		semantic, err := protocolsession.EncodeScanProgress(protocolsession.ScanProgress{
			AttemptID: id16[catalog.ScanAttemptID](92), DiscoveredEntries: 256,
		})
		if err != nil {
			t.Fatal(err)
		}
		progress := signedPeerOperationControl(
			t, harness.receiver, harness.fixture.senderFactory.privateKey,
			protocolsession.MessageScanProgress, harness.call.id, semantic,
		)
		enqueueCallResponse(harness.call, progress)
		enqueueCallResponse(harness.call, progress)
		unexpected, err := protocolsession.NewMessage(
			protocolsession.MessageOpenResults, &harness.call.id, []byte{1},
		)
		if err != nil {
			t.Fatal(err)
		}
		enqueueCallResponse(harness.call, unexpected)
		if err := harness.wait(); !errors.Is(err, ErrOperationMissing) {
			t.Fatalf("unexpected catalog final error = %v", err)
		}
	})

	runCatalogTransportSynctest(t, "malformed operation error is session failure", func(t *testing.T) {
		harness := newBlockedCatalogFetch(t, context.Background())
		message, err := protocolsession.NewMessage(
			protocolsession.MessageOperationError, &harness.call.id, []byte{1},
		)
		if err != nil {
			t.Fatal(err)
		}
		enqueueCallResponse(harness.call, message)
		assertSessionFailure(t, harness.wait())
	})

	runCatalogTransportSynctest(t, "directory operation error retains its typed domain", func(t *testing.T) {
		harness := newBlockedCatalogFetch(t, context.Background())
		semantic, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{
			Scope: protocolsession.OperationScopeDirectory,
			Code:  catalogflow.DirectoryCodePermanentIO, Message: "directory unavailable",
		})
		if err != nil {
			t.Fatal(err)
		}
		enqueueCallResponse(harness.call, signedPeerOperationControl(
			t, harness.receiver, harness.fixture.senderFactory.privateKey,
			protocolsession.MessageOperationError, harness.call.id, semantic,
		))
		err = harness.wait()
		var remote RemoteOperationError
		value, normalized := boundaryFaultForTest(err)
		expected, _ := transferfault.NewCatalog(
			transferfault.ScopeDirectoryLocal, transferfault.CatalogUnavailable,
		)
		if !errors.As(err, &remote) || !normalized || value != expected ||
			remote.Failure().Scope() != protocolsession.OperationScopeDirectory {
			t.Fatalf("directory operation error = %v", err)
		}
	})

	runCatalogTransportSynctest(t, "malformed catalog result wrapper is rejected", func(t *testing.T) {
		harness := newBlockedCatalogFetch(t, context.Background())
		message, err := protocolsession.NewMessage(
			protocolsession.MessageCatalogResult, &harness.call.id, []byte{1},
		)
		if err != nil {
			t.Fatal(err)
		}
		enqueueCallResponse(harness.call, message)
		if err := harness.wait(); err == nil {
			t.Fatal("malformed catalog result crossed the transport boundary")
		}
	})
}

func runCatalogTransportSynctest(t *testing.T, name string, test func(*testing.T)) {
	t.Helper()
	t.Run(name, func(t *testing.T) { synctest.Test(t, test) })
}

type blockedCatalogFetch struct {
	t        *testing.T
	fixture  *verticalFixture
	sender   *SenderRuntime
	receiver *ReceiverRuntime
	call     *operationCall
	result   <-chan error
	release  sync.Once
}

func newBlockedCatalogFetch(t *testing.T, ctx context.Context) *blockedCatalogFetch {
	t.Helper()
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	result := make(chan error, 1)
	go func() {
		request, err := catalogflow.NewListRequest(fixture.directoryID, nil, 0)
		if err == nil {
			_, err = (rpcCatalogTransport{rpc: receiver.rpc}).FetchPage(ctx, request)
		}
		result <- err
	}()
	select {
	case <-fixture.scanStarted:
	case err := <-result:
		receiver.Close()
		sender.Close()
		t.Fatalf("catalog fetch ended before the blocked scan: %v", err)
	case <-time.After(time.Second):
		receiver.Close()
		sender.Close()
		t.Fatal("catalog scan did not start")
	}
	// scanStarted proves sender-side ownership, but transport delivery can precede
	// the receiver's send receipt. Quiesce before white-box response injection so
	// the harness captures the exact admitted receiver generation.
	synctest.Wait()
	receiver.rpc.mu.Lock()
	activeCalls := len(receiver.rpc.calls)
	if activeCalls != 1 {
		receiver.rpc.mu.Unlock()
		close(fixture.scanGate)
		receiver.Close()
		sender.Close()
		t.Fatalf("active catalog RPC calls = %d, want 1", activeCalls)
	}
	var call *operationCall
	for _, active := range receiver.rpc.calls {
		call = active
	}
	receiver.rpc.mu.Unlock()
	generation, authority := call.operationAuthority()
	if generation.IsZero() || authority.IsZero() {
		close(fixture.scanGate)
		receiver.Close()
		sender.Close()
		t.Fatal("catalog RPC call did not retain admitted operation authority")
	}
	harness := &blockedCatalogFetch{
		t: t, fixture: fixture, sender: sender, receiver: receiver, call: call, result: result,
	}
	t.Cleanup(func() {
		harness.unblockSender()
		receiver.Close()
		sender.Close()
	})
	return harness
}

func (harness *blockedCatalogFetch) wait() error {
	harness.t.Helper()
	select {
	case err := <-harness.result:
		harness.unblockSender()
		return err
	case <-time.After(time.Second):
		harness.unblockSender()
		harness.t.Fatal("catalog fetch did not consume the injected response")
		return nil
	}
}

func (harness *blockedCatalogFetch) unblockSender() {
	harness.release.Do(func() { close(harness.fixture.scanGate) })
}
