package catalogflow

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestClientSingleflightAndDirectoryFailureIsolation(t *testing.T) {
	instance := shareInstance(t, 9)
	healthy := directoryID(t, 10)
	failed := directoryID(t, 11)
	healthySnapshot := onePageSnapshot(t, instance, healthy, 12, "ok")
	failure := mustDirectoryFailure(t, instance, failed, 13, DirectoryCodeTransientIO, true)
	codec := newMemoryObjectCodec()
	source := &recordingSource{results: map[catalog.DirectoryID]DirectoryResult{
		healthy: SnapshotResult(healthySnapshot),
		failed:  FailureResult(failure),
	}}
	service, err := NewSenderService(instance, source, codec)
	if err != nil {
		t.Fatal(err)
	}
	transport := &serviceTransport{service: service}
	client, err := NewClient(ClientConfig{ShareInstance: instance, Transport: transport, Verifier: codec})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var wait sync.WaitGroup
	for range 16 {
		wait.Go(func() {
			<-start
			loaded, loadErr := client.LoadDirectory(context.Background(), healthy)
			if loadErr != nil || !loaded.Equal(healthySnapshot) {
				t.Errorf("concurrent load = %v, %v", loaded, loadErr)
			}
		})
	}
	close(start)
	wait.Wait()
	if source.CallCount(healthy) != 1 {
		t.Fatalf("single-page generation source calls = %d", source.CallCount(healthy))
	}

	_, err = client.LoadDirectory(context.Background(), failed)
	var gotFailure DirectoryFailure
	if !errors.As(err, &gotFailure) || gotFailure.AttemptID != failure.AttemptID {
		t.Fatalf("typed directory failure = %T %v", err, err)
	}
	if _, cached := client.Snapshot(failed); cached {
		t.Fatal("failed directory was cached as a generation")
	}
	if loaded, loadErr := client.LoadDirectory(context.Background(), healthy); loadErr != nil || !loaded.Equal(healthySnapshot) {
		t.Fatalf("healthy branch was poisoned by sibling failure: %v", loadErr)
	}
}

func TestClientDirectoryLeaseOwnsOnlyItsCacheRetention(t *testing.T) {
	newClient := func(t *testing.T, seed byte) (*Client, catalog.DirectoryID, catalog.DirectorySnapshot, *int) {
		t.Helper()
		instance := shareInstance(t, seed)
		directory := directoryID(t, seed+1)
		snapshot := onePageSnapshot(t, instance, directory, seed+2, "leased")
		codec := newMemoryObjectCodec()
		object, err := codec.LoadSealedPage(context.Background(), snapshot.Pages()[0])
		if err != nil {
			t.Fatal(err)
		}
		calls := new(int)
		client, err := NewClient(ClientConfig{
			ShareInstance: instance,
			Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
				*calls++
				return append([]byte(nil), object...), nil
			}),
			Verifier: codec,
		})
		if err != nil {
			t.Fatal(err)
		}
		return client, directory, snapshot, calls
	}

	t.Run("lease-only result returns to baseline", func(t *testing.T) {
		client, directory, want, calls := newClient(t, 40)
		got, release, err := client.AcquireDirectory(context.Background(), directory)
		if err != nil || !got.Equal(want) || client.CachedBytes() == 0 {
			t.Fatalf("acquire result=%v err=%v bytes=%d", got, err, client.CachedBytes())
		}
		release()
		release()
		if client.CachedBytes() != 0 {
			t.Fatalf("released bytes=%d", client.CachedBytes())
		}
		_, releaseAgain, err := client.AcquireDirectory(context.Background(), directory)
		if err != nil || *calls != 2 {
			t.Fatalf("reacquire err=%v calls=%d", err, *calls)
		}
		releaseAgain()
	})

	t.Run("preexisting browse owner survives job lease", func(t *testing.T) {
		client, directory, _, calls := newClient(t, 44)
		if _, err := client.LoadDirectory(context.Background(), directory); err != nil {
			t.Fatal(err)
		}
		baseline := client.CachedBytes()
		_, release, err := client.AcquireDirectory(context.Background(), directory)
		if err != nil {
			t.Fatal(err)
		}
		release()
		if client.CachedBytes() != baseline || *calls != 1 {
			t.Fatalf("post-lease bytes=%d baseline=%d calls=%d", client.CachedBytes(), baseline, *calls)
		}
		if _, ok := client.Snapshot(directory); !ok || !client.ReleaseDirectory(directory) || client.CachedBytes() != 0 {
			t.Fatal("browse owner was not independently releasable")
		}
	})

	t.Run("normal load promotes lease-only result", func(t *testing.T) {
		client, directory, _, _ := newClient(t, 48)
		_, release, err := client.AcquireDirectory(context.Background(), directory)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.LoadDirectory(context.Background(), directory); err != nil {
			t.Fatal(err)
		}
		release()
		if client.CachedBytes() == 0 || !client.ReleaseDirectory(directory) || client.CachedBytes() != 0 {
			t.Fatal("normal load did not acquire independent persistent ownership")
		}
	})
}

func TestClientDirectoryFailureLeasePreservesSessionAuthority(t *testing.T) {
	instance := shareInstance(t, 52)
	directory := directoryID(t, 53)
	failure := mustDirectoryFailure(t, instance, directory, 54, DirectoryCodePermanentIO, false)
	codec := newMemoryObjectCodec()
	object, err := codec.LoadSealedFailure(context.Background(), failure)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			calls++
			return append([]byte(nil), object...), nil
		}),
		Verifier: codec,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := client.AcquireDirectory(context.Background(), directory)
	var gotFailure DirectoryFailure
	if !errors.As(err, &gotFailure) ||
		client.CachedBytes() != DirectoryFailureMemoryBytes+CatalogLeaseClaimMemoryBytes {
		t.Fatalf("failure=%v bytes=%d", err, client.CachedBytes())
	}
	release()
	_, releaseAgain, err := client.AcquireDirectory(context.Background(), directory)
	if !errors.As(err, &gotFailure) || calls != 1 ||
		client.CachedBytes() != DirectoryFailureMemoryBytes+CatalogLeaseClaimMemoryBytes {
		t.Fatalf("cached failure=%v calls=%d bytes=%d", err, calls, client.CachedBytes())
	}
	releaseAgain()
}

func TestClientCompletedPersistentWaiterWinsCancellation(t *testing.T) {
	instance := shareInstance(t, 56)
	directory := directoryID(t, 57)
	want := onePageSnapshot(t, instance, directory, 58, "complete")
	codec := newMemoryObjectCodec()
	object, err := codec.LoadSealedPage(context.Background(), want.Pages()[0])
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			return append([]byte(nil), object...), nil
		}),
		Verifier: codec,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	client.mu.Lock()
	_, call, immediate, _ := client.beginRequestLocked(ctx, directory, true, nil)
	client.mu.Unlock()
	if immediate {
		t.Fatal("uncached request completed synchronously")
	}
	<-call.done
	cancel()
	got, err := client.awaitLoad(ctx, directory, call)
	if err != nil || !got.Equal(want) {
		t.Fatalf("completed result=%v err=%v", got, err)
	}
	if !client.ReleaseDirectory(directory) || client.CachedBytes() != 0 {
		t.Fatalf("completed waiter retained bytes=%d", client.CachedBytes())
	}
}
func TestClientCancelsSharedFetchOnlyAfterLastWaiterLeaves(t *testing.T) {
	instance := shareInstance(t, 22)
	directory := directoryID(t, 23)
	transport := &cancellingTransport{started: make(chan struct{}), cancelled: make(chan struct{})}
	client, err := NewClient(ClientConfig{
		ShareInstance: instance, Transport: transport, Verifier: &countingVerifier{},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstContext, cancelFirst := context.WithCancel(context.Background())
	secondContext, cancelSecond := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() {
		_, loadErr := client.LoadDirectory(firstContext, directory)
		firstDone <- loadErr
	}()
	<-transport.started
	go func() {
		_, loadErr := client.LoadDirectory(secondContext, directory)
		secondDone <- loadErr
	}()
	waitForWaiters(t, client, directory, 2)
	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first waiter error = %v", err)
	}
	select {
	case <-transport.cancelled:
		t.Fatal("shared fetch was cancelled while another waiter remained")
	default:
	}
	cancelSecond()
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("second waiter error = %v", err)
	}
	<-transport.cancelled
}

func TestClientCanceledAcquireDoesNotStealSurvivingLease(t *testing.T) {
	instance := shareInstance(t, 59)
	directory := directoryID(t, 60)
	want := onePageSnapshot(t, instance, directory, 61, "shared")
	codec := newMemoryObjectCodec()
	object, err := codec.LoadSealedPage(context.Background(), want.Pages()[0])
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	var calls int
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(ctx context.Context, _ ListRequest) ([]byte, error) {
			calls++
			once.Do(func() { close(started) })
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-gate:
				return append([]byte(nil), object...), nil
			}
		}),
		Verifier: codec,
	})
	if err != nil {
		t.Fatal(err)
	}
	type acquireResult struct {
		snapshot catalog.DirectorySnapshot
		release  func()
		err      error
	}
	firstContext, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan acquireResult, 1)
	secondDone := make(chan acquireResult, 1)
	go func() {
		snapshot, release, err := client.AcquireDirectory(firstContext, directory)
		firstDone <- acquireResult{snapshot: snapshot, release: release, err: err}
	}()
	<-started
	go func() {
		snapshot, release, err := client.AcquireDirectory(context.Background(), directory)
		secondDone <- acquireResult{snapshot: snapshot, release: release, err: err}
	}()
	waitForWaiters(t, client, directory, 2)
	cancelFirst()
	first := <-firstDone
	first.release()
	if !errors.Is(first.err, context.Canceled) {
		t.Fatalf("canceled acquire error=%v", first.err)
	}
	close(gate)
	second := <-secondDone
	if second.err != nil || !second.snapshot.Equal(want) {
		t.Fatalf("surviving acquire=%v err=%v", second.snapshot, second.err)
	}
	second.release()
	client.mu.Lock()
	activeClaims, directoryClaims := client.activeLeaseClaims, len(client.leaseClaimsByDirectory)
	client.mu.Unlock()
	if calls != 1 || client.CachedBytes() != 0 || activeClaims != 0 || directoryClaims != 0 {
		t.Fatalf("calls=%d retained bytes=%d active=%d directories=%d", calls, client.CachedBytes(), activeClaims, directoryClaims)
	}
}
func TestClientRejectsObjectBeforeVerificationBudget(t *testing.T) {
	instance := shareInstance(t, 20)
	directory := directoryID(t, 21)
	transport := PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
		return make([]byte, 33), nil
	})
	verifier := &countingVerifier{}
	client, err := NewClient(ClientConfig{
		ShareInstance: instance, Transport: transport, Verifier: verifier,
		MaxObjectBytes: 32, MaxCacheBytes: 64, MaxDirectories: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.LoadDirectory(context.Background(), directory); !errors.Is(err, ErrClientBudget) {
		t.Fatalf("oversize object error = %v", err)
	}
	if verifier.calls != 0 {
		t.Fatal("oversize object reached the verifier")
	}
}
func TestClientConfigurationFetchAndCacheFailuresAreBounded(t *testing.T) {
	instance := shareInstance(t, 45)
	directory := directoryID(t, 46)
	validTransport := PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) { return []byte{1}, nil })
	validVerifier := ObjectVerifierFunc(func(context.Context, catalog.ShareInstance, ListRequest, []byte) (VerifiedObject, error) {
		return VerifiedObject{}, nil
	})
	for name, config := range map[string]ClientConfig{
		"zero share":      {Transport: validTransport, Verifier: validVerifier},
		"page wire limit": {ShareInstance: instance, Transport: validTransport, Verifier: validVerifier, MaxPagesPerDirectory: catalog.MaxDirectoryEntries + 1},
		"object wire limit": {ShareInstance: instance, Transport: validTransport, Verifier: validVerifier,
			MaxObjectBytes: catalog.MaxCatalogPageObjectBytes + 1},
		"negative directories": {ShareInstance: instance, Transport: validTransport, Verifier: validVerifier, MaxDirectories: -1},
		"concurrent load wire limit": {
			ShareInstance: instance, Transport: validTransport, Verifier: validVerifier,
			MaxConcurrentLoads: MaxConcurrentDirectoryLoads + 1,
		},
		"global lease claim limit": {
			ShareInstance: instance, Transport: validTransport, Verifier: validVerifier,
			MaxLeaseClaims: MaxClientLeaseClaims + 1,
		},
		"directory lease claim limit": {
			ShareInstance: instance, Transport: validTransport, Verifier: validVerifier,
			MaxDirectoryLeaseClaims: MaxDirectoryLeaseClaims + 1,
		},
		"directory exceeds global claims": {
			ShareInstance: instance, Transport: validTransport, Verifier: validVerifier,
			MaxLeaseClaims: 2, MaxDirectoryLeaseClaims: 3,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewClient(config); err == nil {
				t.Fatal("invalid client configuration was accepted")
			}
		})
	}

	client, _ := NewClient(ClientConfig{ShareInstance: instance, Transport: validTransport, Verifier: validVerifier})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.LoadDirectory(canceled, directory); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled load = %v", err)
	}
	if _, err := client.LoadDirectory(context.Background(), catalog.DirectoryID{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("zero directory load = %v", err)
	}
	if client.ReleaseDirectory(directory) {
		t.Fatal("release reported a missing cache entry")
	}

	for name, transport := range map[string]PageTransport{
		"fetch error":  PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) { return nil, errors.New("fetch") }),
		"empty object": PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) { return nil, nil }),
	} {
		t.Run(name, func(t *testing.T) {
			candidate, _ := NewClient(ClientConfig{ShareInstance: instance, Transport: transport, Verifier: validVerifier})
			if _, err := candidate.LoadDirectory(context.Background(), directory); err == nil {
				t.Fatal("failed fetch was accepted")
			}
		})
	}

	verifyError := ObjectVerifierFunc(func(context.Context, catalog.ShareInstance, ListRequest, []byte) (VerifiedObject, error) {
		return VerifiedObject{}, errors.New("verify")
	})
	candidate, _ := NewClient(ClientConfig{ShareInstance: instance, Transport: validTransport, Verifier: verifyError})
	if _, err := candidate.LoadDirectory(context.Background(), directory); err == nil {
		t.Fatal("verification error was hidden")
	}
	candidate, _ = NewClient(ClientConfig{ShareInstance: instance, Transport: validTransport, Verifier: validVerifier})
	if _, err := candidate.LoadDirectory(context.Background(), directory); !errors.Is(err, ErrUnverifiedObject) {
		t.Fatalf("empty verified object = %v", err)
	}

	snapshot := onePageSnapshot(t, instance, directory, 47, "item")
	codec := newMemoryObjectCodec()
	encoded, _ := codec.LoadSealedPage(context.Background(), snapshot.Pages()[0])
	cacheLimited, _ := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport:     PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) { return encoded, nil }),
		Verifier:      codec, MaxCacheBytes: uint64(len(encoded)) - 1, MaxDirectories: 1,
	})
	if _, err := cacheLimited.LoadDirectory(context.Background(), directory); !errors.Is(err, ErrClientBudget) {
		t.Fatalf("cache byte budget = %v", err)
	}
}
