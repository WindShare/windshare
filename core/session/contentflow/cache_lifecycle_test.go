package contentflow

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/catalog"
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
			_, getErr := cache.Get(context.Background(), key, func(loadContext context.Context) ([]byte, error) {
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
		if _, err := cache.Get(context.Background(), key, func(context.Context) ([]byte, error) {
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
			_, getErr := cache.Get(context.Background(), key, func(context.Context) ([]byte, error) {
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
