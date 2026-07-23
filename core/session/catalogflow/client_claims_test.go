package catalogflow

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestMaxClientLeaseClaimsForCacheCapsBeforeNarrowing(t *testing.T) {
	for _, test := range []struct {
		name       string
		cacheBytes uint64
		want       int
	}{
		{name: "below one claim", cacheBytes: CatalogLeaseClaimMemoryBytes - 1, want: 0},
		{name: "memory bound", cacheBytes: 7 * CatalogLeaseClaimMemoryBytes, want: 7},
		{name: "exact global bound", cacheBytes: uint64(MaxClientLeaseClaims) * CatalogLeaseClaimMemoryBytes, want: MaxClientLeaseClaims},
		{name: "above global bound", cacheBytes: uint64(MaxClientLeaseClaims+1) * CatalogLeaseClaimMemoryBytes, want: MaxClientLeaseClaims},
		{name: "maximum uint64", cacheBytes: math.MaxUint64, want: MaxClientLeaseClaims},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := maxClientLeaseClaimsForCache(test.cacheBytes); got != test.want {
				t.Fatalf("max claims = %d, want %d", got, test.want)
			}
		})
	}
}

func TestNewClientCapsLeaseClaimsBeforeArchitectureNarrowing(t *testing.T) {
	client, err := NewClient(ClientConfig{
		ShareInstance: shareInstance(t, 75),
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			return nil, errors.New("unused transport")
		}),
		Verifier:      &countingVerifier{},
		MaxCacheBytes: math.MaxUint64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.maxLeaseClaims != MaxClientLeaseClaims {
		t.Fatalf("max lease claims = %d, want %d", client.maxLeaseClaims, MaxClientLeaseClaims)
	}
	if client.maxDirectoryLeaseClaims != MaxDirectoryLeaseClaims {
		t.Fatalf("max directory lease claims = %d, want %d", client.maxDirectoryLeaseClaims, MaxDirectoryLeaseClaims)
	}
}

func TestClientBoundsBlockedAndResidentAcquireClaims(t *testing.T) {
	instance := shareInstance(t, 62)
	directory := directoryID(t, 63)
	want := onePageSnapshot(t, instance, directory, 64, "bounded")
	codec := newMemoryObjectCodec()
	object, _ := codec.LoadSealedPage(context.Background(), want.Pages()[0])
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
		Verifier: codec, MaxLeaseClaims: 2, MaxDirectoryLeaseClaims: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		release func()
		err     error
	}
	results := make(chan result, 1)
	go func() {
		_, release, err := client.AcquireDirectory(context.Background(), directory)
		results <- result{release: release, err: err}
	}()
	// The transport starts only after the load's claim is accounted, so this
	// event proves the directory budget is occupied without timing or polling.
	<-started
	_, rejectedRelease, err := client.AcquireDirectory(context.Background(), directory)
	if !errors.Is(err, ErrClientBudget) {
		t.Fatalf("blocked claim overflow=%v", err)
	}
	rejectedRelease()
	close(gate)
	first := <-results
	if first.err != nil || calls != 1 {
		t.Fatalf("surviving claim err=%v calls=%d", first.err, calls)
	}
	_, rejectedRelease, err = client.AcquireDirectory(context.Background(), directory)
	if !errors.Is(err, ErrClientBudget) {
		t.Fatalf("resident claim overflow=%v", err)
	}
	rejectedRelease()
	first.release()
	_, reopenedRelease, err := client.AcquireDirectory(context.Background(), directory)
	if err != nil {
		t.Fatalf("released claim did not reopen capacity: %v", err)
	}
	reopenedRelease()
	client.mu.Lock()
	active := client.activeLeaseClaims
	client.mu.Unlock()
	if active != 0 || client.CachedBytes() != 0 {
		t.Fatalf("active claims=%d retained bytes=%d", active, client.CachedBytes())
	}
}

func TestClientBoundsAcquireClaimsAcrossDirectories(t *testing.T) {
	instance := shareInstance(t, 67)
	codec := newMemoryObjectCodec()
	directories := []catalog.DirectoryID{directoryID(t, 68), directoryID(t, 69), directoryID(t, 70)}
	objects := make(map[catalog.DirectoryID][]byte, len(directories))
	for index, directory := range directories {
		snapshot := onePageSnapshot(t, instance, directory, byte(71+index), fmt.Sprintf("file-%d", index))
		object, err := codec.LoadSealedPage(context.Background(), snapshot.Pages()[0])
		if err != nil {
			t.Fatal(err)
		}
		objects[directory] = object
	}
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(_ context.Context, request ListRequest) ([]byte, error) {
			return append([]byte(nil), objects[request.DirectoryID()]...), nil
		}),
		Verifier: codec, MaxLeaseClaims: 2, MaxDirectoryLeaseClaims: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range directories {
		if _, err := client.LoadDirectory(context.Background(), directory); err != nil {
			t.Fatal(err)
		}
	}
	_, releaseFirst, err := client.AcquireDirectory(context.Background(), directories[0])
	if err != nil {
		t.Fatal(err)
	}
	_, releaseSecond, err := client.AcquireDirectory(context.Background(), directories[1])
	if err != nil {
		t.Fatal(err)
	}
	_, rejectedRelease, err := client.AcquireDirectory(context.Background(), directories[2])
	if !errors.Is(err, ErrClientBudget) {
		t.Fatalf("global claim overflow=%v", err)
	}
	rejectedRelease()
	releaseFirst()
	_, releaseThird, err := client.AcquireDirectory(context.Background(), directories[2])
	if err != nil {
		t.Fatalf("global release did not reopen capacity: %v", err)
	}
	releaseSecond()
	releaseThird()
	for _, directory := range directories {
		if !client.ReleaseDirectory(directory) {
			t.Fatalf("persistent directory %x was not releasable", directory)
		}
	}
	if client.CachedBytes() != 0 {
		t.Fatalf("retained bytes=%d", client.CachedBytes())
	}
}

func TestClientNoncacheableAcquireErrorReturnsClaimBudget(t *testing.T) {
	client, err := NewClient(ClientConfig{
		ShareInstance: shareInstance(t, 65),
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			return nil, errors.New("transport unavailable")
		}),
		Verifier: &countingVerifier{}, MaxLeaseClaims: 1, MaxDirectoryLeaseClaims: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := client.AcquireDirectory(context.Background(), directoryID(t, 66))
	if err == nil {
		t.Fatal("noncacheable transport failure was hidden")
	}
	release()
	client.mu.Lock()
	active := client.activeLeaseClaims
	client.mu.Unlock()
	if active != 0 || client.CachedBytes() != 0 {
		t.Fatalf("active claims=%d retained bytes=%d", active, client.CachedBytes())
	}
}
