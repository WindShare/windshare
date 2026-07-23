package catalogflow

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

func TestListRequestCanonicalRoundTrip(t *testing.T) {
	directory := directoryID(t, 1)
	generation := generationID(t, 2)

	first, err := NewListRequest(directory, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeListRequest(first)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeListRequest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.DirectoryID() != directory || decoded.PageIndex() != 0 {
		t.Fatalf("decoded first request = %#v", decoded)
	}
	if _, present := decoded.Generation(); present {
		t.Fatal("first request unexpectedly gained a generation")
	}

	later, err := NewListRequest(directory, &generation, 7)
	if err != nil {
		t.Fatal(err)
	}
	laterBytes, err := EncodeListRequest(later)
	if err != nil {
		t.Fatal(err)
	}
	laterDecoded, err := DecodeListRequest(laterBytes)
	if err != nil {
		t.Fatal(err)
	}
	gotGeneration, present := laterDecoded.Generation()
	if !present || gotGeneration != generation || laterDecoded.PageIndex() != 7 {
		t.Fatalf("decoded later request = %#v", laterDecoded)
	}
}

func TestListRequestRejectsHostileCBOR(t *testing.T) {
	directory := directoryID(t, 3)
	request, err := NewListRequest(directory, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := EncodeListRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	for name, encoded := range map[string][]byte{
		"trailing byte": append(append([]byte(nil), canonical...), 0),
		"indefinite":    append(append([]byte{0x9f}, canonical[1:]...), 0xff),
		"wrong arity":   {0x82, 0x50, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0xf6},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeListRequest(encoded); err == nil {
				t.Fatal("hostile request was accepted")
			}
		})
	}
	if _, err := NewListRequest(directory, nil, 1); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("later page without generation error = %v", err)
	}
}

func TestSenderServiceAndClientCommitOnlyTerminalGeneration(t *testing.T) {
	instance := shareInstance(t, 4)
	directory := directoryID(t, 5)
	sibling := directoryID(t, 6)
	snapshot := twoPageSnapshot(t, instance, directory, 7, "a", "b")
	codec := newMemoryObjectCodec()
	source := &recordingSource{results: map[catalog.DirectoryID]DirectoryResult{
		directory: SnapshotResult(snapshot),
		sibling:   SnapshotResult(onePageSnapshot(t, instance, sibling, 8, "sibling")),
	}}
	service, err := NewSenderService(instance, source, codec)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	transport := &serviceTransport{service: service, beforeSecond: gate, secondReached: make(chan struct{})}
	client, err := NewClient(ClientConfig{ShareInstance: instance, Transport: transport, Verifier: codec})
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		loaded, loadErr := client.LoadDirectory(context.Background(), directory)
		if loadErr == nil && !loaded.Equal(snapshot) {
			loadErr = errors.New("client committed another snapshot")
		}
		result <- loadErr
	}()
	<-transport.secondReached
	if _, committed := client.Snapshot(directory); committed {
		t.Fatal("client exposed a generation before its terminal page")
	}
	if source.CallCount(sibling) != 0 {
		t.Fatal("loading one directory touched its sibling")
	}
	close(gate)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, committed := client.Snapshot(directory); !committed {
		t.Fatal("terminal generation was not committed")
	}
	if transport.CallCount() != 2 || source.CallCount(directory) != 2 {
		t.Fatalf("page/source calls = %d/%d", transport.CallCount(), source.CallCount(directory))
	}
	if !client.ReleaseDirectory(directory) || client.CachedBytes() != 0 {
		t.Fatal("directory cache release did not return its budget")
	}
	staleGeneration := generationID(t, 99)
	staleRequest, _ := NewListRequest(directory, &staleGeneration, 1)
	if _, err := service.Serve(context.Background(), staleRequest); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("stale generation error = %v", err)
	}
	currentGeneration := snapshot.Generation()
	outOfRange, _ := NewListRequest(directory, &currentGeneration, 9)
	if _, err := service.Serve(context.Background(), outOfRange); !errors.Is(err, ErrPageOutOfRange) {
		t.Fatalf("out-of-range page error = %v", err)
	}
}

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
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			loaded, loadErr := client.LoadDirectory(context.Background(), healthy)
			if loadErr != nil || !loaded.Equal(healthySnapshot) {
				t.Errorf("concurrent load = %v, %v", loaded, loadErr)
			}
		}()
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

func TestAssemblerRejectsGapConflictIdentityAndPostTerminal(t *testing.T) {
	instance := shareInstance(t, 14)
	directory := directoryID(t, 15)
	snapshot := twoPageSnapshot(t, instance, directory, 16, "a", "b")
	pages := snapshot.Pages()

	assembler, err := NewAssembler(instance, directory, 4)
	if err != nil {
		t.Fatal(err)
	}
	wrongGenerationPage := twoPageSnapshot(t, instance, directory, 99, "wrong-a", "wrong-b").Pages()[1]
	if _, err := assembler.Accept(VerifiedPage(wrongGenerationPage)); !errors.Is(err, ErrPageGap) {
		t.Fatalf("gap error = %v", err)
	}
	if status, err := assembler.Accept(VerifiedPage(pages[0])); err != nil || status != PageAccepted {
		t.Fatalf("first page = %v, %v", status, err)
	}
	if status, err := assembler.Accept(VerifiedPage(pages[0])); err != nil || status != PageReplay {
		t.Fatalf("idempotent replay = %v, %v", status, err)
	}
	conflicting := twoPageSnapshot(t, instance, directory, 16, "changed", "z").Pages()[0]
	if _, err := assembler.Accept(VerifiedPage(conflicting)); !errors.Is(err, ErrPageConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}
	if status, err := assembler.Accept(VerifiedPage(pages[1])); err != nil || status != GenerationCommitted {
		t.Fatalf("terminal page = %v, %v", status, err)
	}
	if status, err := assembler.Accept(VerifiedPage(pages[1])); err != nil || status != PageReplay {
		t.Fatalf("terminal replay = %v, %v", status, err)
	}
	extraEntry, err := catalog.NewFileEntry(fileID(t, 17), "extra", 1, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	extra, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: instance, DirectoryID: directory, Generation: snapshot.Generation(),
		PageIndex: 2, Previous: pages[1].Commitment(), Entries: []catalog.Entry{extraEntry}, Terminal: true,
	}, testCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assembler.Accept(VerifiedPage(extra)); !errors.Is(err, ErrPostTerminal) {
		t.Fatalf("post-terminal append error = %v", err)
	}
	failure := mustDirectoryFailure(t, instance, directory, 18, DirectoryCodePermanentIO, false)
	if _, err := assembler.Accept(VerifiedFailure(failure)); !errors.Is(err, ErrPostTerminal) {
		t.Fatalf("post-terminal failure error = %v", err)
	}

	other := onePageSnapshot(t, instance, directoryID(t, 19), 20, "other").Pages()[0]
	otherAssembler, _ := NewAssembler(instance, directory, 2)
	if _, err := otherAssembler.Accept(VerifiedPage(other)); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("cross-directory error = %v", err)
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

func TestDirectoryFailureValidation(t *testing.T) {
	directory := directoryID(t, 24)
	attempt := scanAttemptID(t, 25)
	valid := DirectoryFailure{
		ShareInstance: shareInstance(t, 26), DirectoryID: directory, AttemptID: attempt, Code: DirectoryCodeBudget,
		Retryable: true, RetryAfter: time.Second,
	}
	if _, err := NewDirectoryFailure(valid); err != nil {
		t.Fatalf("retryable budget failure = %v", err)
	}
	for name, mutate := range map[string]func(*DirectoryFailure){
		"zero attempt": func(f *DirectoryFailure) { f.AttemptID = catalog.ScanAttemptID{} },
		"wrong scope":  func(f *DirectoryFailure) { f.Code = 0x3001 },
		"short retry":  func(f *DirectoryFailure) { f.RetryAfter = time.Millisecond },
		"transient permanent": func(f *DirectoryFailure) {
			f.Code, f.Retryable, f.RetryAfter = DirectoryCodeTransientIO, false, 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if _, err := NewDirectoryFailure(candidate); err == nil {
				t.Fatal("invalid directory failure was accepted")
			}
		})
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

func TestCatalogFlowConstructorsAndRequestEdgesFailClosed(t *testing.T) {
	instance := shareInstance(t, 30)
	directory := directoryID(t, 31)
	generation := generationID(t, 32)
	if _, err := NewAssembler(catalog.ShareInstance{}, directory, 1); err == nil {
		t.Fatal("assembler accepted a zero share")
	}
	if _, err := NewSenderService(instance, nil, newMemoryObjectCodec()); err == nil {
		t.Fatal("sender accepted a nil source")
	}
	if _, err := NewListRequest(catalog.DirectoryID{}, nil, 0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("zero directory request = %v", err)
	}
	if _, err := NewListRequest(directory, &catalog.DirectoryGeneration{}, 0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("zero generation request = %v", err)
	}
	if _, err := EncodeListRequest(ListRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("zero request encode = %v", err)
	}
	if _, err := DecodeListRequest(make([]byte, MaxListRequestBytes+1)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversize request = %v", err)
	}

	badDirectory, _ := requestEncMode.Marshal([]any{[]byte{1}, nil, uint64(0)})
	badGeneration, _ := requestEncMode.Marshal([]any{directory.Bytes(), []byte{1}, uint64(0)})
	bigPage, _ := requestEncMode.Marshal([]any{directory.Bytes(), generation.Bytes(), uint64(math.MaxUint32) + 1})
	for name, encoded := range map[string][]byte{
		"directory":  badDirectory,
		"generation": badGeneration,
		"page":       bigPage,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeListRequest(encoded); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestAssemblerRejectsUnverifiedAndCrossPageStateWithoutMutation(t *testing.T) {
	instance := shareInstance(t, 33)
	directory := directoryID(t, 34)
	snapshot := twoPageSnapshot(t, instance, directory, 35, "a", "b")
	pages := snapshot.Pages()
	assembler, _ := NewAssembler(instance, directory, 1)
	if _, err := assembler.Accept(VerifiedObject{}); !errors.Is(err, ErrUnverifiedObject) {
		t.Fatalf("empty verified object = %v", err)
	}
	failure := mustDirectoryFailure(t, instance, directory, 36, DirectoryCodePermanentIO, false)
	both := VerifiedFailure(failure)
	both.Page = pages[0]
	if _, err := assembler.Accept(both); !errors.Is(err, ErrUnverifiedObject) {
		t.Fatalf("page and failure = %v", err)
	}
	wrongShareFailure := failure
	wrongShareFailure.ShareInstance = shareInstance(t, 37)
	if _, err := assembler.Accept(VerifiedFailure(wrongShareFailure)); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("cross-share failure = %v", err)
	}
	if status, err := assembler.Accept(VerifiedPage(pages[0])); err != nil || status != PageAccepted {
		t.Fatalf("first page = %v, %v", status, err)
	}
	if assembler.PageCount() != 1 {
		t.Fatalf("page count = %d", assembler.PageCount())
	}
	if _, err := assembler.Accept(VerifiedPage(pages[1])); !errors.Is(err, ErrClientBudget) {
		t.Fatalf("page budget = %v", err)
	}
	if assembler.PageCount() != 1 {
		t.Fatal("rejected terminal mutated assembler")
	}

	full, _ := NewAssembler(instance, directory, 2)
	_, _ = full.Accept(VerifiedPage(pages[0]))
	if status, err := full.Accept(VerifiedPage(pages[1])); err != nil || status != GenerationCommitted {
		t.Fatalf("commit = %v, %v", status, err)
	}
	if _, err := full.NextRequest(); !errors.Is(err, ErrPostTerminal) {
		t.Fatalf("request after terminal = %v", err)
	}
}

func TestSenderServiceRejectsAmbiguousAndMisbindingSources(t *testing.T) {
	instance := shareInstance(t, 38)
	directory := directoryID(t, 39)
	request, _ := NewListRequest(directory, nil, 0)
	codec := newMemoryObjectCodec()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service, _ := NewSenderService(instance, &recordingSource{}, codec)
	if _, err := service.Serve(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled serve = %v", err)
	}
	if _, err := service.Serve(context.Background(), ListRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid serve request = %v", err)
	}

	loadError := DirectorySourceFunc(func(context.Context, catalog.DirectoryID) (DirectoryResult, error) {
		return DirectoryResult{}, errors.New("load failed")
	})
	service, _ = NewSenderService(instance, loadError, codec)
	if _, err := service.Serve(context.Background(), request); err == nil {
		t.Fatal("source error was hidden")
	}

	snapshot := onePageSnapshot(t, instance, directory, 40, "item")
	failure := mustDirectoryFailure(t, instance, directory, 41, DirectoryCodePermanentIO, false)
	for name, result := range map[string]DirectoryResult{
		"neither":              {},
		"both":                 {Snapshot: snapshot, Failure: &failure},
		"wrong share snapshot": SnapshotResult(onePageSnapshot(t, shareInstance(t, 42), directory, 43, "item")),
		"wrong share failure":  FailureResult(withFailure(failure, func(value *DirectoryFailure) { value.ShareInstance = shareInstance(t, 44) })),
	} {
		t.Run(name, func(t *testing.T) {
			source := DirectorySourceFunc(func(context.Context, catalog.DirectoryID) (DirectoryResult, error) { return result, nil })
			candidate, _ := NewSenderService(instance, source, codec)
			if _, err := candidate.Serve(context.Background(), request); err == nil {
				t.Fatal("invalid source result was accepted")
			}
		})
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

type DirectorySourceFunc func(context.Context, catalog.DirectoryID) (DirectoryResult, error)

func (f DirectorySourceFunc) LoadDirectory(ctx context.Context, directory catalog.DirectoryID) (DirectoryResult, error) {
	return f(ctx, directory)
}

type ObjectVerifierFunc func(context.Context, catalog.ShareInstance, ListRequest, []byte) (VerifiedObject, error)

func (f ObjectVerifierFunc) Verify(
	ctx context.Context,
	instance catalog.ShareInstance,
	request ListRequest,
	object []byte,
) (VerifiedObject, error) {
	return f(ctx, instance, request, object)
}

func withFailure(failure DirectoryFailure, mutate func(*DirectoryFailure)) DirectoryFailure {
	mutate(&failure)
	return failure
}

type PageTransportFunc func(context.Context, ListRequest) ([]byte, error)

func (f PageTransportFunc) FetchPage(ctx context.Context, request ListRequest) ([]byte, error) {
	return f(ctx, request)
}

type countingVerifier struct{ calls int }

func (v *countingVerifier) Verify(context.Context, catalog.ShareInstance, ListRequest, []byte) (VerifiedObject, error) {
	v.calls++
	return VerifiedObject{}, nil
}

type cancellingTransport struct {
	once      sync.Once
	started   chan struct{}
	cancelled chan struct{}
}

func (t *cancellingTransport) FetchPage(ctx context.Context, _ ListRequest) ([]byte, error) {
	t.once.Do(func() { close(t.started) })
	<-ctx.Done()
	close(t.cancelled)
	return nil, ctx.Err()
}

func waitForWaiters(t *testing.T, client *Client, directory catalog.DirectoryID, count int) {
	t.Helper()
	for {
		client.mu.Lock()
		call := client.inflight[directory]
		ready := call != nil && call.waiters == count
		client.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

type recordingSource struct {
	mu      sync.Mutex
	results map[catalog.DirectoryID]DirectoryResult
	calls   map[catalog.DirectoryID]int
}

func (s *recordingSource) LoadDirectory(ctx context.Context, directory catalog.DirectoryID) (DirectoryResult, error) {
	if err := ctx.Err(); err != nil {
		return DirectoryResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = make(map[catalog.DirectoryID]int)
	}
	s.calls[directory]++
	result, ok := s.results[directory]
	if !ok {
		return DirectoryResult{}, fmt.Errorf("unexpected directory %x", directory)
	}
	return result, nil
}

func (s *recordingSource) CallCount(directory catalog.DirectoryID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[directory]
}

type memoryObjectCodec struct {
	mu      sync.Mutex
	objects map[string]VerifiedObject
}

func newMemoryObjectCodec() *memoryObjectCodec {
	return &memoryObjectCodec{objects: make(map[string]VerifiedObject)}
}

func (c *memoryObjectCodec) LoadSealedPage(_ context.Context, page catalog.CatalogPage) ([]byte, error) {
	key := append([]byte{1}, page.Commitment().Bytes()...)
	c.mu.Lock()
	c.objects[string(key)] = VerifiedPage(page)
	c.mu.Unlock()
	return key, nil
}

func (c *memoryObjectCodec) LoadSealedFailure(_ context.Context, failure DirectoryFailure) ([]byte, error) {
	key := append([]byte{2}, failure.AttemptID.Bytes()...)
	c.mu.Lock()
	c.objects[string(key)] = VerifiedFailure(failure)
	c.mu.Unlock()
	return key, nil
}

func (c *memoryObjectCodec) Verify(
	_ context.Context,
	_ catalog.ShareInstance,
	_ ListRequest,
	encoded []byte,
) (VerifiedObject, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	object, ok := c.objects[string(encoded)]
	if !ok {
		return VerifiedObject{}, errors.New("object authentication failed")
	}
	return object, nil
}

type serviceTransport struct {
	service       *SenderService
	beforeSecond  <-chan struct{}
	secondReached chan struct{}
	once          sync.Once
	mu            sync.Mutex
	calls         int
}

func (t *serviceTransport) FetchPage(ctx context.Context, request ListRequest) ([]byte, error) {
	t.mu.Lock()
	t.calls++
	call := t.calls
	t.mu.Unlock()
	if call == 2 && t.beforeSecond != nil {
		t.once.Do(func() { close(t.secondReached) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.beforeSecond:
		}
	}
	return t.service.Serve(ctx, request)
}

func (t *serviceTransport) CallCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

type testCommitter struct{}

func (testCommitter) Commit(input catalog.PageCommitInput) (catalog.PageCommitment, error) {
	hash := sha256.New()
	_, _ = hash.Write(input.ShareInstance.Bytes())
	_, _ = hash.Write(input.DirectoryID.Bytes())
	_, _ = hash.Write(input.Generation.Bytes())
	var index [4]byte
	binary.BigEndian.PutUint32(index[:], input.PageIndex)
	_, _ = hash.Write(index[:])
	_, _ = hash.Write(input.Previous.Bytes())
	for _, entry := range input.Entries {
		_, _ = hash.Write(entry.NodeID().Bytes())
		_, _ = hash.Write([]byte(entry.Name()))
	}
	if input.Terminal {
		_, _ = hash.Write([]byte{1})
	}
	return catalog.NewPageCommitment(hash.Sum(nil))
}

func twoPageSnapshot(t *testing.T, instance catalog.ShareInstance, directory catalog.DirectoryID, generationByte byte, firstName, secondName string) catalog.DirectorySnapshot {
	t.Helper()
	generation := generationID(t, generationByte)
	firstEntry, err := catalog.NewFileEntry(fileID(t, generationByte+1), firstName, 1, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: instance, DirectoryID: directory, Generation: generation,
		PageIndex: 0, Entries: []catalog.Entry{firstEntry},
	}, testCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	secondEntry, err := catalog.NewFileEntry(fileID(t, generationByte+2), secondName, 2, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: instance, DirectoryID: directory, Generation: generation,
		PageIndex: 1, Previous: first.Commitment(), Entries: []catalog.Entry{secondEntry}, Terminal: true,
	}, testCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.NewDirectorySnapshot([]catalog.CatalogPage{first, second})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func onePageSnapshot(t *testing.T, instance catalog.ShareInstance, directory catalog.DirectoryID, generationByte byte, name string) catalog.DirectorySnapshot {
	t.Helper()
	generation := generationID(t, generationByte)
	entry, err := catalog.NewFileEntry(fileID(t, generationByte+1), name, 1, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: instance, DirectoryID: directory, Generation: generation,
		PageIndex: 0, Entries: []catalog.Entry{entry}, Terminal: true,
	}, testCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.NewDirectorySnapshot([]catalog.CatalogPage{page})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func mustDirectoryFailure(t *testing.T, instance catalog.ShareInstance, directory catalog.DirectoryID, attemptByte byte, code uint16, retryable bool) DirectoryFailure {
	t.Helper()
	failure := DirectoryFailure{
		ShareInstance: instance, DirectoryID: directory, AttemptID: scanAttemptID(t, attemptByte), Code: code,
		Retryable: retryable,
	}
	if retryable {
		failure.RetryAfter = time.Second
	}
	checked, err := NewDirectoryFailure(failure)
	if err != nil {
		t.Fatal(err)
	}
	return checked
}

func fixedIdentity(first byte) []byte {
	result := make([]byte, catalog.IdentityBytes)
	for index := range result {
		result[index] = first + byte(index)
	}
	return result
}

func shareInstance(t *testing.T, first byte) catalog.ShareInstance {
	t.Helper()
	value, err := catalog.ShareInstanceFromBytes(fixedIdentity(first))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func directoryID(t *testing.T, first byte) catalog.DirectoryID {
	t.Helper()
	value, err := catalog.DirectoryIDFromBytes(fixedIdentity(first))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func fileID(t *testing.T, first byte) catalog.FileID {
	t.Helper()
	value, err := catalog.FileIDFromBytes(fixedIdentity(first))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func generationID(t *testing.T, first byte) catalog.DirectoryGeneration {
	t.Helper()
	value, err := catalog.DirectoryGenerationFromBytes(fixedIdentity(first))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func scanAttemptID(t *testing.T, first byte) catalog.ScanAttemptID {
	t.Helper()
	value, err := catalog.ScanAttemptIDFromBytes(fixedIdentity(first))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
