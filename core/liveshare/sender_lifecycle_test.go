package liveshare

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/internal/testoutputroot"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/contentflow"
	sendersession "github.com/windshare/windshare/core/session/sendersession"
	"github.com/windshare/windshare/core/transfer"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs"
)

type blockingSenderRuntimeLifecycle struct {
	beginCalled chan struct{}
	stopCalled  chan struct{}
	allowStop   chan struct{}
}

func (lifecycle *blockingSenderRuntimeLifecycle) BeginStop(string) error {
	close(lifecycle.beginCalled)
	return nil
}

func (lifecycle *blockingSenderRuntimeLifecycle) Stop(ctx context.Context, _ string) error {
	close(lifecycle.stopCalled)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-lifecycle.allowStop:
		return nil
	}
}

type closeOrderCatalogSource struct {
	cacheLoaderExited <-chan struct{}
	closedAfterJoin   chan bool
	stopOwner         func()
}

func (*closeOrderCatalogSource) SelectedRoots() []catalog.NodeRecord {
	return nil
}

func (*closeOrderCatalogSource) ScanDirectory(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
	return catalog.ScanResult{}, errors.New("unused catalog scan")
}

func (source *closeOrderCatalogSource) Close() error {
	if source.stopOwner != nil {
		source.stopOwner()
	}
	afterJoin := false
	select {
	case <-source.cacheLoaderExited:
		afterJoin = true
	default:
	}
	source.closedAfterJoin <- afterJoin
	return nil
}

func TestPreparedSenderConcurrentCloseJoinsCacheBeforeSourceTeardown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		shareBytes := bytes.Repeat([]byte{1}, catalog.IdentityBytes)
		fileBytes := bytes.Repeat([]byte{2}, catalog.IdentityBytes)
		revisionBytes := bytes.Repeat([]byte{3}, content.IdentityBytes)
		share, err := catalog.ShareInstanceFromBytes(shareBytes)
		if err != nil {
			t.Fatal(err)
		}
		file, err := catalog.FileIDFromBytes(fileBytes)
		if err != nil {
			t.Fatal(err)
		}
		revision, err := content.FileRevisionFromBytes(revisionBytes)
		if err != nil {
			t.Fatal(err)
		}
		geometry, err := content.NewFileGeometry(1, catalog.MinChunkSize)
		if err != nil {
			t.Fatal(err)
		}
		descriptor, err := content.NewFileRevisionDescriptor(share, file, revision, geometry, catalog.ModifiedTime{})
		if err != nil {
			t.Fatal(err)
		}
		key, err := contentflow.NewBlockCacheKey(descriptor, 0)
		if err != nil {
			t.Fatal(err)
		}
		process, err := contentflow.NewProcessCacheBudget(uint64(catalog.MinChunkSize))
		if err != nil {
			t.Fatal(err)
		}
		cache, err := contentflow.NewSharedBlockCache(share, uint64(catalog.MinChunkSize), process)
		if err != nil {
			t.Fatal(err)
		}

		loadStarted := make(chan struct{})
		loadCancelled := make(chan struct{})
		allowLoaderExit := make(chan struct{})
		loaderExited := make(chan struct{})
		getDone := make(chan error, 1)
		go func() {
			_, getErr := cache.Get(context.Background(), key, func(loadContext context.Context) ([]byte, error) {
				close(loadStarted)
				<-loadContext.Done()
				close(loadCancelled)
				<-allowLoaderExit
				close(loaderExited)
				return nil, loadContext.Err()
			})
			getDone <- getErr
		}()
		<-loadStarted

		sourceClosed := make(chan bool, 1)
		source := &closeOrderCatalogSource{
			cacheLoaderExited: loaderExited,
			closedAfterJoin:   sourceClosed,
		}
		sender := &PreparedSender{
			cache: cache, source: source,
			capability: link.Link{ReadSecret: []byte{1}}, privateKey: ed25519.PrivateKey{2},
		}
		reentrantStopResult := make(chan error, 1)
		source.stopOwner = func() { reentrantStopResult <- sender.Stop() }
		if err := sender.Stop(); err != nil {
			t.Fatal(err)
		}
		if len(sender.Capability().ReadSecret) != 0 || len(sender.Registration().SenderPrivateKey) != 0 {
			t.Fatal("stopped sender exposed authority while teardown was still joining")
		}
		<-loadCancelled

		firstCloseDone := make(chan error, 1)
		go func() {
			firstCloseDone <- sender.Close()
		}()

		secondCloseStarted := make(chan struct{})
		secondCloseDone := make(chan error, 1)
		go func() {
			close(secondCloseStarted)
			secondCloseDone <- sender.Close()
		}()
		<-secondCloseStarted
		synctest.Wait()
		select {
		case err := <-firstCloseDone:
			t.Fatalf("first close returned before cache loader exit: %v", err)
		default:
		}
		select {
		case err := <-secondCloseDone:
			t.Fatalf("concurrent close did not join cache teardown: %v", err)
		default:
		}
		select {
		case <-sourceClosed:
			t.Fatal("source teardown began before cache loader exit")
		default:
		}

		close(allowLoaderExit)
		<-loaderExited
		if err := <-firstCloseDone; err != nil {
			t.Fatal(err)
		}
		if err := <-secondCloseDone; err != nil {
			t.Fatal(err)
		}
		if afterJoin := <-sourceClosed; !afterJoin {
			t.Fatal("source closed before the cache loader joined")
		}
		select {
		case err := <-reentrantStopResult:
			if err != nil {
				t.Fatalf("dependency callback could not stop its owner: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("dependency callback Stop did not return")
		}
		if err := <-getDone; !errors.Is(err, contentflow.ErrServiceClosed) {
			t.Fatalf("closed cache waiter error = %v", err)
		}
		if cache.UsedBytes() != 0 || process.Used() != 0 {
			t.Fatalf("sender teardown retained cache bytes: cache=%d process=%d", cache.UsedBytes(), process.Used())
		}
		if err := sender.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPreparedSenderStopFreezesRuntimeBeforeAsyncJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lifecycle := &blockingSenderRuntimeLifecycle{
			beginCalled: make(chan struct{}),
			stopCalled:  make(chan struct{}),
			allowStop:   make(chan struct{}),
		}
		sender := &PreparedSender{runtimeFactory: lifecycle}
		if err := sender.Stop(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-lifecycle.beginCalled:
		default:
			t.Fatal("sender Stop returned before runtime admission froze")
		}

		select {
		case <-lifecycle.stopCalled:
		case <-time.After(time.Second):
			t.Fatal("sender teardown did not begin its runtime join")
		}
		closeDone := make(chan error, 1)
		go func() { closeDone <- sender.Close() }()
		synctest.Wait()
		select {
		case err := <-closeDone:
			t.Fatalf("sender Close returned before runtime join: %v", err)
		default:
		}

		close(lifecycle.allowStop)
		if err := <-closeDone; err != nil {
			t.Fatal(err)
		}
		sender.mu.Lock()
		retainedRuntime := sender.runtimeFactory != nil
		sender.mu.Unlock()
		if retainedRuntime {
			t.Fatal("closed sender retained its stopped runtime factory")
		}
	})
}

func (*closeOrderCatalogSource) OpenStable(context.Context, catalog.NodeRecord) (content.StableFile, error) {
	return nil, content.ErrRevisionNotFound
}

func TestPreparedSenderForwardsTransferActivityOnlyForContentDelivery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("transfer-activity"), 256)
	if err := os.WriteFile(filepath.Join(root, "file.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}

	sender, err := PrepareSender(context.Background(), SenderConfig{
		CatalogBudget:    testCatalogBudget(),
		CacheBudget:      testCacheBudget(),
		RevisionCapacity: newTestRevisionCapacity(t),
		Source:           testFileSource([]string{root}),
		Relays:           []string{"ws://127.0.0.1:8484"},
		ChunkSize:        catalog.MinChunkSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	receiver, err := PrepareReceiver(ReceiverConfig{
		Capacity:         testReceiveCapacity(t),
		Capability:       sender.Capability(),
		DescriptorObject: sender.Registration().Descriptor,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()

	var enters, leaves, active atomic.Int32
	config := testRuntimeFactoryConfig(&testTerminalConnectivity{})
	config.TransferActivity = contentflow.TransferActivityFunc(func() func() {
		enters.Add(1)
		active.Add(1)
		return func() {
			active.Add(-1)
			leaves.Add(1)
		}
	})
	factory, err := sender.NewRuntimeFactory(config)
	if err != nil {
		t.Fatal(err)
	}

	senderChannel, receiverChannel := newFacadeChannelPair()
	accepted := make(chan struct {
		runtime *sendersession.Runtime
		err     error
	}, 1)
	go func() {
		runtime, acceptErr := factory.Accept(context.Background(), senderChannel)
		accepted <- struct {
			runtime *sendersession.Runtime
			err     error
		}{runtime: runtime, err: acceptErr}
	}()

	receiverRuntime, err := receiver.Connect(context.Background(), receiverChannel, transferlanes.RouteRelay)
	if err != nil {
		t.Fatal(err)
	}
	defer receiverRuntime.Close()
	acceptedResult := <-accepted
	if acceptedResult.err != nil {
		t.Fatal(acceptedResult.err)
	}
	defer acceptedResult.runtime.Close()

	// Authenticating a session and browsing directories must not count as content transfer.
	if _, err := receiverRuntime.Catalog().LoadDirectory(context.Background(), receiverRuntime.Descriptor().SyntheticRoot()); err != nil {
		t.Fatal(err)
	}
	if got := enters.Load(); got != 0 {
		t.Fatalf("transfer activity enters during catalog browse = %d, want 0", got)
	}

	outputFixture := testoutputroot.New(t)
	output, err := osfs.NewOutputAuthority(osfs.OutputConfig{
		RootPath: outputFixture.RootPath, CreateRoot: outputFixture.CreateRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	rules, _ := transfer.NewSelectionRules(true, nil)
	selection, err := transfer.NewSelectionSpec(
		receiverRuntime.Descriptor().ShareInstance(), receiverRuntime.Descriptor().SyntheticRoot(), rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := output.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	lookup, err := output.SelectOperation(context.Background(), selection, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != osfs.OutputLookupCreateReady {
		t.Fatalf("native lookup = (%d, %v)", lookup.Kind(), err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	operation, err := lookup.Create(context.Background(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("native operation omitted receive intent")
	}
	jobID, err := transfer.NewTransferJobID()
	if err != nil {
		t.Fatal(err)
	}
	job, err := receiverRuntime.NewTransferJob(intent, jobID, operation, diagnostics.Sink{})
	if err != nil {
		t.Fatal(err)
	}
	if result := job.Run(context.Background()); result.Outcome != transfer.DirectTreeOutcomeSuccess {
		t.Fatalf("job result = %+v", result)
	}

	if enters.Load() < 2 {
		t.Fatalf("transfer activity enters after file download = %d, want >= 2 (open + block serve)", enters.Load())
	}
	if active.Load() != 0 || enters.Load() != leaves.Load() {
		t.Fatalf("unbalanced transfer activity after download: enters=%d leaves=%d active=%d",
			enters.Load(), leaves.Load(), active.Load())
	}
}
