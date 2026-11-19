package liveshare

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/contentflow"
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
