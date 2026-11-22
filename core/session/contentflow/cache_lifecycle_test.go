package contentflow

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

func TestSharedBlockCacheCloseCancelsAndJoinsLoader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		descriptor := flowDescriptor(t, 1)
		process, err := NewProcessCacheBudget(uint64(catalog.MinChunkSize))
		if err != nil {
			t.Fatal(err)
		}
		cache, err := NewSharedBlockCache(descriptor.ShareInstance(), uint64(catalog.MinChunkSize), process)
		if err != nil {
			t.Fatal(err)
		}
		key, err := NewBlockCacheKey(descriptor, 0)
		if err != nil {
			t.Fatal(err)
		}

		loadStarted := make(chan struct{})
		cancellationObserved := make(chan struct{})
		allowLoaderExit := make(chan struct{})
		loaderExited := make(chan struct{})
		getDone := make(chan error, 1)
		go func() {
			_, getErr := cache.get(context.Background(), key, func(loadContext context.Context) ([]byte, error) {
				close(loadStarted)
				<-loadContext.Done()
				close(cancellationObserved)
				<-allowLoaderExit
				close(loaderExited)
				return nil, loadContext.Err()
			})
			getDone <- getErr
		}()
		<-loadStarted

		closeDone := make(chan struct{})
		go func() {
			cache.Close()
			close(closeDone)
		}()
		<-cancellationObserved
		secondCloseStarted := make(chan struct{})
		secondCloseDone := make(chan struct{})
		go func() {
			close(secondCloseStarted)
			cache.Close()
			close(secondCloseDone)
		}()
		<-secondCloseStarted
		synctest.Wait()
		select {
		case <-closeDone:
			t.Fatal("cache close returned before its cancelled loader exited")
		default:
		}
		select {
		case <-secondCloseDone:
			t.Fatal("concurrent cache close returned before the shared join completed")
		default:
		}

		unexpectedLoad := false
		if _, err := cache.get(context.Background(), key, func(context.Context) ([]byte, error) {
			unexpectedLoad = true
			return []byte{1}, nil
		}); !errors.Is(err, ErrServiceClosed) {
			t.Fatalf("load admitted after close began: %v", err)
		}
		if unexpectedLoad {
			t.Fatal("closed cache started a load after joining began")
		}

		close(allowLoaderExit)
		<-loaderExited
		<-closeDone
		<-secondCloseDone
		if err := <-getDone; !errors.Is(err, ErrServiceClosed) {
			t.Fatalf("closed cache waiter error = %v", err)
		}
		if cache.UsedBytes() != 0 || process.Used() != 0 {
			t.Fatalf("closed cache retained bytes: cache=%d process=%d", cache.UsedBytes(), process.Used())
		}
		cache.Close()
	})
}

func TestSharedBlockCacheLoaderCanStopWithoutSelfJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		descriptor := flowDescriptor(t, 1)
		process, err := NewProcessCacheBudget(uint64(catalog.MinChunkSize))
		if err != nil {
			t.Fatal(err)
		}
		cache, err := NewSharedBlockCache(descriptor.ShareInstance(), uint64(catalog.MinChunkSize), process)
		if err != nil {
			t.Fatal(err)
		}
		key, err := NewBlockCacheKey(descriptor, 0)
		if err != nil {
			t.Fatal(err)
		}

		stopReturned := make(chan struct{})
		getDone := make(chan error, 1)
		go func() {
			_, getErr := cache.get(context.Background(), key, func(context.Context) ([]byte, error) {
				cache.Stop()
				close(stopReturned)
				return nil, ErrServiceClosed
			})
			getDone <- getErr
		}()
		select {
		case <-stopReturned:
		case <-time.After(time.Second):
			t.Fatal("loader Stop did not return")
		}
		cache.Close()
		if err := <-getDone; !errors.Is(err, ErrServiceClosed) {
			t.Fatalf("self-stopped cache waiter error = %v", err)
		}
		if cache.UsedBytes() != 0 || process.Used() != 0 {
			t.Fatalf("self-stopped cache retained bytes: cache=%d process=%d", cache.UsedBytes(), process.Used())
		}
	})
}

func TestBlockCacheIsolatesLoaderAndPublicGetPayloads(t *testing.T) {
	share := flowID[catalog.ShareInstance](1)
	budget, err := NewProcessCacheBudget(64)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := NewSharedBlockCache(share, 64, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	key := BlockCacheKey{ShareInstance: share, FileID: flowID[catalog.FileID](2), FileRevision: flowID[content.FileRevision](3)}
	source := []byte("committed")
	loads := 0
	load := func(context.Context) ([]byte, error) {
		loads++
		return source, nil
	}
	first, err := cache.get(t.Context(), key, load)
	if err != nil {
		t.Fatal(err)
	}
	source[0] ^= 1

	exported, err := cache.Get(t.Context(), key, load)
	if err != nil || loads != 1 || string(exported) != "committed" {
		t.Fatalf("exported hit loads=%d payload=%q err=%v", loads, exported, err)
	}
	exported[0] ^= 1

	second, err := cache.get(t.Context(), key, load)
	if err != nil || loads != 1 || second.recordID != first.recordID || second.payload != "committed" {
		t.Fatalf("cached hit loads=%d payload=%q id=%x err=%v", loads, second.payload, second.recordID, err)
	}
}

func TestSharedBlockCacheRetireRevisionReleasesBudgetAndSuppressesInFlightCaching(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first := flowDescriptor(t, uint64(catalog.MinChunkSize)*2)
		otherFile := flowDescriptor(t, uint64(catalog.MinChunkSize)*2)
		otherRev, err := content.NewFileRevisionDescriptor(
			first.ShareInstance(), first.FileID(), flowID[content.FileRevision](99),
			first.Geometry(), first.ModifiedTime(),
		)
		if err != nil {
			t.Fatal(err)
		}
		process, err := NewProcessCacheBudget(16)
		if err != nil {
			t.Fatal(err)
		}
		cache, err := NewSharedBlockCache(first.ShareInstance(), 16, process)
		if err != nil {
			t.Fatal(err)
		}
		defer cache.Close()

		keyFirst0, _ := NewBlockCacheKey(first, 0)
		keyFirst1, _ := NewBlockCacheKey(first, 1)
		keyOtherRev0, _ := NewBlockCacheKey(otherRev, 0)
		if _, err := cache.get(context.Background(), keyFirst0, func(context.Context) ([]byte, error) {
			return []byte("12345678"), nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.get(context.Background(), keyOtherRev0, func(context.Context) ([]byte, error) {
			return []byte("abcdefgh"), nil
		}); err != nil {
			t.Fatal(err)
		}
		if cache.UsedBytes() != 16 || process.Used() != 16 {
			t.Fatalf("pre-retire usage cache=%d process=%d", cache.UsedBytes(), process.Used())
		}

		watchCtx, cancelWatch := context.WithCancelCause(context.Background())
		unwatch, err := cache.watchRevision(first, cancelWatch)
		if err != nil {
			t.Fatal(err)
		}
		defer unwatch()

		loadStarted := make(chan struct{})
		allowFinish := make(chan struct{})
		inFlightDone := make(chan cachedBlock, 1)
		inFlightErr := make(chan error, 1)
		go func() {
			block, getErr := cache.get(context.Background(), keyFirst1, func(loadCtx context.Context) ([]byte, error) {
				close(loadStarted)
				select {
				case <-loadCtx.Done():
					return nil, loadCtx.Err()
				case <-allowFinish:
					return []byte("in-flight"), nil
				}
			})
			inFlightDone <- block
			inFlightErr <- getErr
		}()
		<-loadStarted

		// Retiring first evicts only first's cached blocks and marks its in-flight
		// load uncacheable without cancelling watches or reporting ErrRevisionDrift.
		cache.RetireRevision(first.FileID(), first.FileRevision())
		if cache.UsedBytes() != 8 || process.Used() != 8 {
			t.Fatalf("post-retire usage cache=%d process=%d, want 8", cache.UsedBytes(), process.Used())
		}
		if watchCtx.Err() != nil {
			t.Fatalf("clean retirement cancelled active revision watch: %v", context.Cause(watchCtx))
		}

		// Freed process budget is immediately usable by another share's cache.
		peerShare := flowID[catalog.ShareInstance](77)
		peerCache, err := NewSharedBlockCache(peerShare, 16, process)
		if err != nil {
			t.Fatal(err)
		}
		defer peerCache.Close()
		peerKey := BlockCacheKey{ShareInstance: peerShare, FileID: otherFile.FileID(), FileRevision: otherFile.FileRevision()}
		if _, err := peerCache.get(context.Background(), peerKey, func(context.Context) ([]byte, error) {
			return []byte("peerdata"), nil
		}); err != nil {
			t.Fatal(err)
		}
		if peerCache.UsedBytes() != 8 || process.Used() != 16 {
			t.Fatalf("peer share could not claim retired budget: peer=%d process=%d", peerCache.UsedBytes(), process.Used())
		}

		// The retired revision's in-flight load finishes normally for its waiter
		// without repopulating cache entries.
		close(allowFinish)
		if err := <-inFlightErr; err != nil {
			t.Fatalf("retired in-flight load failed: %v", err)
		}
		if block := <-inFlightDone; block.payload != "in-flight" {
			t.Fatalf("retired in-flight payload=%q", block.payload)
		}
		if cache.UsedBytes() != 8 {
			t.Fatalf("retired in-flight load repopulated cache: used=%d want=8", cache.UsedBytes())
		}

		// A later drift invalidation still reaches remaining watches on that revision.
		cache.InvalidateRevision(first.FileID(), first.FileRevision())
		if !errors.Is(context.Cause(watchCtx), content.ErrRevisionDrift) {
			t.Fatalf("post-retire drift watch cause=%v", context.Cause(watchCtx))
		}
	})
}
