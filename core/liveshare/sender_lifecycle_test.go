package liveshare

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/catalogflow"
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
			cache: cache, selectedSource: source,
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

func TestPreparedSenderRollbackDestroysPartiallyBuiltSealers(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "selected.bin")
	if err := os.WriteFile(filename, []byte("rollback"), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := productionSenderPreparationDependencies()
	newCatalogObjects := dependencies.newCatalogObjects
	newRecordSealer := dependencies.newRecordSealer
	var catalogObjects *catalogflow.SealedCatalogStore
	var recordSealer *records.Sealer
	dependencies.newCatalogObjects = func(config catalogflow.SealedCatalogStoreConfig) (*catalogflow.SealedCatalogStore, error) {
		created, err := newCatalogObjects(config)
		if err == nil {
			catalogObjects = created
		}
		return created, err
	}
	dependencies.newRecordSealer = func(config records.SealerConfig) (*records.Sealer, error) {
		created, err := newRecordSealer(config)
		if err == nil {
			recordSealer = created
		}
		return created, err
	}
	lateFailure := errors.New("forced session authority failure")
	dependencies.sessionAuthKey = func(*content.KeyTree) (content.DerivedKey, error) {
		return content.DerivedKey{}, lateFailure
	}

	sender, err := PrepareSender(context.Background(), SenderConfig{
		Paths: []string{filename}, Relays: []string{"ws://127.0.0.1:8484"},
		ChunkSize: catalog.MinChunkSize, Random: mathrand.New(mathrand.NewSource(7)),
		preparation: dependencies,
	})
	if sender != nil || !errors.Is(err, lateFailure) {
		t.Fatalf("late preparation result = %v, %v", sender, err)
	}
	if catalogObjects == nil || recordSealer == nil {
		t.Fatal("late failure occurred before helper ownership transferred")
	}
	// The test never calls Close: only PrepareSender's error rollback can revoke
	// the helper authority retained by these constructor observers.
	if _, err := catalogObjects.LoadSealedPage(context.Background(), catalog.CatalogPage{}); !errors.Is(err, catalogflow.ErrSealedCatalogStoreDestroyed) {
		t.Fatalf("automatic rollback retained catalog sealer authority: %v", err)
	}
	if _, err := recordSealer.SealRevision(content.FileRevisionDescriptor{}); !errors.Is(err, records.ErrSealerDestroyed) {
		t.Fatalf("automatic rollback retained record sealer authority: %v", err)
	}
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
