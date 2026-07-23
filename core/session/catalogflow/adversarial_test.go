package catalogflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

func TestClientBindsVerificationAndResponseToExactPageRequest(t *testing.T) {
	instance := shareInstance(t, 90)
	directory := directoryID(t, 91)
	snapshot := twoPageSnapshot(t, instance, directory, 92, "a", "b")
	firstPage := snapshot.Pages()[0]
	codec := newMemoryObjectCodec()
	firstObject, err := codec.LoadSealedPage(context.Background(), firstPage)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	transport := PageTransportFunc(func(_ context.Context, request ListRequest) ([]byte, error) {
		calls++
		// The second response deliberately replays page zero into the authenticated
		// request for page one. The verifier ignores context to prove the client
		// performs an independent response-binding check.
		return append([]byte(nil), firstObject...), nil
	})
	var verifiedPages []uint32
	verifier := ObjectVerifierFunc(func(
		ctx context.Context,
		gotInstance catalog.ShareInstance,
		request ListRequest,
		object []byte,
	) (VerifiedObject, error) {
		if gotInstance != instance || request.DirectoryID() != directory {
			return VerifiedObject{}, errors.New("verifier received the wrong authenticated context")
		}
		verifiedPages = append(verifiedPages, request.PageIndex())
		return codec.Verify(ctx, gotInstance, request, object)
	})
	client, err := NewClient(ClientConfig{ShareInstance: instance, Transport: transport, Verifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.LoadDirectory(context.Background(), directory); !errors.Is(err, ErrResponseIdentity) {
		t.Fatalf("cross-request page replay = %v", err)
	}
	if calls != 2 || len(verifiedPages) != 2 || verifiedPages[0] != 0 || verifiedPages[1] != 1 {
		t.Fatalf("verification contexts = calls %d pages %v", calls, verifiedPages)
	}
	if _, committed := client.Snapshot(directory); committed {
		t.Fatal("cross-request replay committed a partial generation")
	}
}

func TestVerifiedCatalogObjectExclusivelyAnswersAuthenticatedRequest(t *testing.T) {
	instance := shareInstance(t, 105)
	directory := directoryID(t, 106)
	snapshot := twoPageSnapshot(t, instance, directory, 107, "a", "b")
	pages := snapshot.Pages()
	failure := mustDirectoryFailure(t, instance, directory, 108, DirectoryCodePermanentIO, false)

	firstRequest, err := NewListRequest(directory, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateVerifiedResponse(instance, firstRequest, VerifiedObject{
		Page: pages[0], Failure: &failure,
	}); !errors.Is(err, ErrUnverifiedObject) {
		t.Fatalf("page/failure ambiguity = %v", err)
	}

	generation := snapshot.Generation()
	laterRequest, err := NewListRequest(directory, &generation, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateVerifiedResponse(instance, laterRequest, VerifiedFailure(failure)); !errors.Is(err, ErrResponseIdentity) {
		t.Fatalf("failure after generation selection = %v", err)
	}

	wrongGeneration := generationID(t, 109)
	wrongGenerationRequest, err := NewListRequest(directory, &wrongGeneration, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateVerifiedResponse(instance, wrongGenerationRequest, VerifiedPage(pages[1])); !errors.Is(err, ErrResponseIdentity) {
		t.Fatalf("cross-generation page replay = %v", err)
	}
}

func TestClientCachesAuthenticatedFailureUntilExplicitRetryCooldown(t *testing.T) {
	instance := shareInstance(t, 93)
	directory := directoryID(t, 94)
	failure := mustDirectoryFailure(t, instance, directory, 95, DirectoryCodeTransientIO, true)
	snapshot := onePageSnapshot(t, instance, directory, 96, "recovered")
	codec := newMemoryObjectCodec()
	failureObject, err := codec.LoadSealedFailure(context.Background(), failure)
	if err != nil {
		t.Fatal(err)
	}
	pageObject, err := codec.LoadSealedPage(context.Background(), snapshot.Pages()[0])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	var calls int
	transport := PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
		calls++
		if calls == 1 {
			return append([]byte(nil), failureObject...), nil
		}
		return append([]byte(nil), pageObject...), nil
	})
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport:     transport,
		Verifier:      codec,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.LoadDirectory(context.Background(), directory); !errors.As(err, &DirectoryFailure{}) {
		t.Fatalf("initial failure = %v", err)
	}
	if _, err := client.LoadDirectory(context.Background(), directory); !errors.As(err, &DirectoryFailure{}) {
		t.Fatalf("cached cooldown failure = %v", err)
	}
	if calls != 1 || client.CachedBytes() != DirectoryFailureMemoryBytes {
		t.Fatalf("cooldown cache = calls %d bytes %d", calls, client.CachedBytes())
	}
	now = now.Add(failure.RetryAfter)
	loaded, err := client.LoadDirectory(context.Background(), directory)
	if err != nil || !loaded.Equal(snapshot) {
		t.Fatalf("explicit retry = %v, %v", loaded, err)
	}
	if calls != 2 {
		t.Fatalf("retry transport calls = %d", calls)
	}

	now = time.Unix(1_700_000_000, 0)
	replayedAttempts := 0
	replayClient, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			replayedAttempts++
			return append([]byte(nil), failureObject...), nil
		}),
		Verifier: codec,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replayClient.LoadDirectory(context.Background(), directory); !errors.As(err, &DirectoryFailure{}) {
		t.Fatalf("replay client initial failure = %v", err)
	}
	now = now.Add(failure.RetryAfter)
	if _, err := replayClient.LoadDirectory(context.Background(), directory); !errors.Is(err, ErrPageConflict) {
		t.Fatalf("retry reused scan attempt = %v", err)
	}
	if replayedAttempts != 2 {
		t.Fatalf("replayed attempt transport calls = %d", replayedAttempts)
	}
}

func TestClientAcquirePreservesRetryableFailureCooldownAfterRelease(t *testing.T) {
	instance := shareInstance(t, 120)
	directory := directoryID(t, 121)
	failure := mustDirectoryFailure(t, instance, directory, 122, DirectoryCodeTransientIO, true)
	snapshot := onePageSnapshot(t, instance, directory, 123, "recovered")
	codec := newMemoryObjectCodec()
	failureObject, err := codec.LoadSealedFailure(context.Background(), failure)
	if err != nil {
		t.Fatal(err)
	}
	pageObject, err := codec.LoadSealedPage(context.Background(), snapshot.Pages()[0])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_200, 0)
	var calls int
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			calls++
			if calls == 1 {
				return append([]byte(nil), failureObject...), nil
			}
			return append([]byte(nil), pageObject...), nil
		}),
		Verifier: codec,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := client.AcquireDirectory(context.Background(), directory)
	if !errors.As(err, &DirectoryFailure{}) {
		t.Fatalf("initial failure=%v", err)
	}
	release()
	_, release, err = client.AcquireDirectory(context.Background(), directory)
	if !errors.As(err, &DirectoryFailure{}) || calls != 1 {
		t.Fatalf("pre-cooldown failure=%v calls=%d", err, calls)
	}
	release()
	now = now.Add(failure.RetryAfter)
	loaded, release, err := client.AcquireDirectory(context.Background(), directory)
	if err != nil || !loaded.Equal(snapshot) || calls != 2 {
		t.Fatalf("post-cooldown snapshot=%v err=%v calls=%d", loaded, err, calls)
	}
	release()
	if client.CachedBytes() != 0 {
		t.Fatalf("lease-only recovered snapshot bytes=%d", client.CachedBytes())
	}
}

func TestClientRetryDoesNotEvictOlderLeasedFailure(t *testing.T) {
	instance := shareInstance(t, 124)
	directory := directoryID(t, 125)
	failure := mustDirectoryFailure(t, instance, directory, 126, DirectoryCodeTransientIO, true)
	snapshot := onePageSnapshot(t, instance, directory, 127, "recovered")
	codec := newMemoryObjectCodec()
	failureObject, _ := codec.LoadSealedFailure(context.Background(), failure)
	pageObject, _ := codec.LoadSealedPage(context.Background(), snapshot.Pages()[0])
	now := time.Unix(1_700_000_300, 0)
	var calls int
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			calls++
			if calls == 1 {
				return append([]byte(nil), failureObject...), nil
			}
			return append([]byte(nil), pageObject...), nil
		}),
		Verifier: codec, Now: func() time.Time { return now }, MaxDirectories: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, releaseOld, err := client.AcquireDirectory(context.Background(), directory)
	if !errors.As(err, &DirectoryFailure{}) {
		t.Fatalf("initial failure=%v", err)
	}
	now = now.Add(failure.RetryAfter)
	_, releaseRejected, err := client.AcquireDirectory(context.Background(), directory)
	if !errors.Is(err, ErrClientBudget) ||
		client.CachedBytes() != DirectoryFailureMemoryBytes+CatalogLeaseClaimMemoryBytes {
		t.Fatalf("pinned replacement err=%v bytes=%d", err, client.CachedBytes())
	}
	releaseRejected()
	releaseOld()
	loaded, releaseRecovered, err := client.AcquireDirectory(context.Background(), directory)
	if err != nil || !loaded.Equal(snapshot) || calls != 3 {
		t.Fatalf("recovered=%v err=%v calls=%d", loaded, err, calls)
	}
	releaseRecovered()
	if client.CachedBytes() != 0 {
		t.Fatalf("recovered snapshot bytes=%d", client.CachedBytes())
	}
}

func TestClientAcquireRetryTransfersPersistentFailureOwnership(t *testing.T) {
	instance := shareInstance(t, 130)
	directory := directoryID(t, 131)
	failure := mustDirectoryFailure(t, instance, directory, 132, DirectoryCodeTransientIO, true)
	snapshot := onePageSnapshot(t, instance, directory, 133, "recovered")
	codec := newMemoryObjectCodec()
	failureObject, _ := codec.LoadSealedFailure(context.Background(), failure)
	pageObject, _ := codec.LoadSealedPage(context.Background(), snapshot.Pages()[0])
	now := time.Unix(1_700_000_400, 0)
	var calls int
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			calls++
			if calls == 1 {
				return append([]byte(nil), failureObject...), nil
			}
			return append([]byte(nil), pageObject...), nil
		}),
		Verifier: codec, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.LoadDirectory(context.Background(), directory); !errors.As(err, &DirectoryFailure{}) {
		t.Fatalf("persistent failure=%v", err)
	}
	now = now.Add(failure.RetryAfter)
	loaded, release, err := client.AcquireDirectory(context.Background(), directory)
	if err != nil || !loaded.Equal(snapshot) {
		t.Fatalf("retry snapshot=%v err=%v", loaded, err)
	}
	release()
	if _, ok := client.Snapshot(directory); !ok || client.CachedBytes() == 0 {
		t.Fatal("job release discarded the preexisting browse owner")
	}
	if !client.ReleaseDirectory(directory) || client.CachedBytes() != 0 {
		t.Fatal("persistent replacement was not explicitly releasable")
	}
}

func TestClientRetryTransportFailurePreservesAuthenticatedFailure(t *testing.T) {
	instance := shareInstance(t, 110)
	directory := directoryID(t, 111)
	failure := mustDirectoryFailure(t, instance, directory, 112, DirectoryCodeTransientIO, true)
	codec := newMemoryObjectCodec()
	failureObject, err := codec.LoadSealedFailure(context.Background(), failure)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_100, 0)
	transportErr := errors.New("catalog transport unavailable")
	var calls int
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			calls++
			if calls == 1 {
				return append([]byte(nil), failureObject...), nil
			}
			return nil, transportErr
		}),
		Verifier: codec,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.LoadDirectory(context.Background(), directory); !errors.As(err, &DirectoryFailure{}) {
		t.Fatalf("initial authenticated failure = %v", err)
	}
	now = now.Add(failure.RetryAfter)
	if _, err := client.LoadDirectory(context.Background(), directory); !errors.Is(err, transportErr) {
		t.Fatalf("retry transport failure = %v", err)
	}
	client.mu.Lock()
	retained, ok := client.cache[directory]
	client.mu.Unlock()
	if !ok || retained.failure == nil || retained.failure.AttemptID != failure.AttemptID ||
		client.CachedBytes() != DirectoryFailureMemoryBytes {
		t.Fatalf("authenticated failure was not retained after transport loss: present=%v value=%+v bytes=%d", ok, retained, client.CachedBytes())
	}
	if !client.ReleaseDirectory(directory) || client.CachedBytes() != 0 {
		t.Fatal("releasing retained failure did not return its cache budget")
	}
}

func TestSenderServiceBoundsAndOwnsStoredObjectBytes(t *testing.T) {
	stored := []byte{1, 2, 3}
	encoded, err := validateSealedObject(stored, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored[0] = 9
	if encoded[0] != 1 {
		t.Fatal("sender exposed sealed-object store backing memory")
	}
	for name, run := range map[string]func() error{
		"store error": func() error {
			_, err := validateSealedObject(nil, errors.New("load"))
			return err
		},
		"empty": func() error {
			_, err := validateSealedObject(nil, nil)
			return err
		},
		"oversize": func() error {
			_, err := validateSealedObject(make([]byte, catalog.MaxCatalogPageObjectBytes+1), nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); err == nil {
				t.Fatal("invalid sealed object was accepted")
			}
		})
	}
}

func TestClientBoundsDistinctInflightDirectories(t *testing.T) {
	instance := shareInstance(t, 97)
	started := make(chan catalog.DirectoryID, 2)
	transport := PageTransportFunc(func(ctx context.Context, request ListRequest) ([]byte, error) {
		started <- request.DirectoryID()
		<-ctx.Done()
		return nil, ctx.Err()
	})
	client, err := NewClient(ClientConfig{
		ShareInstance:      instance,
		Transport:          transport,
		Verifier:           &countingVerifier{},
		MaxConcurrentLoads: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	contexts := make([]context.CancelFunc, 0, 2)
	done := make(chan error, 2)
	for index := range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		contexts = append(contexts, cancel)
		directory := directoryID(t, byte(98+index))
		go func() {
			_, loadErr := client.LoadDirectory(ctx, directory)
			done <- loadErr
		}()
		<-started
	}
	if _, err := client.LoadDirectory(context.Background(), directoryID(t, 101)); !errors.Is(err, ErrClientBudget) {
		t.Fatalf("distinct inflight overflow = %v", err)
	}
	for _, cancel := range contexts {
		cancel()
	}
	for range contexts {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel inflight load = %v", err)
		}
	}
}

func TestClientChargesInflightObjectBytesAcrossDirectories(t *testing.T) {
	instance := shareInstance(t, 102)
	firstDirectory := directoryID(t, 103)
	secondDirectory := directoryID(t, 104)
	verifyStarted := make(chan struct{})
	releaseVerify := make(chan struct{})
	var verifyCalls int
	verifier := ObjectVerifierFunc(func(
		_ context.Context,
		_ catalog.ShareInstance,
		request ListRequest,
		_ []byte,
	) (VerifiedObject, error) {
		verifyCalls++
		if request.DirectoryID() == firstDirectory {
			close(verifyStarted)
			<-releaseVerify
		}
		return VerifiedObject{}, nil
	})
	client, err := NewClient(ClientConfig{
		ShareInstance: instance,
		Transport: PageTransportFunc(func(context.Context, ListRequest) ([]byte, error) {
			return make([]byte, 40), nil
		}),
		Verifier: verifier, MaxObjectBytes: 64, MaxCacheBytes: 64, MaxConcurrentLoads: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, loadErr := client.LoadDirectory(context.Background(), firstDirectory)
		firstDone <- loadErr
	}()
	<-verifyStarted
	if _, err := client.LoadDirectory(context.Background(), secondDirectory); !errors.Is(err, ErrClientBudget) {
		t.Fatalf("cross-directory inflight bytes = %v", err)
	}
	if verifyCalls != 1 {
		t.Fatalf("over-budget object reached verifier: calls=%d", verifyCalls)
	}
	close(releaseVerify)
	if err := <-firstDone; !errors.Is(err, ErrUnverifiedObject) {
		t.Fatalf("first verifier result = %v", err)
	}
	client.mu.Lock()
	remaining := client.inflightBytes
	client.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("failed loads retained inflight bytes: %d", remaining)
	}
}
