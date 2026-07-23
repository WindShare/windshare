package catalogflow

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

func TestSealedCatalogStoreDestroyClearsClonedSecretsCachesAndRejectsOperations(t *testing.T) {
	fixture := newCatalogObjectFixture(t)
	catalogKeySource := bytes.Clone(fixture.key)
	privateKeySource := bytes.Clone(fixture.privateKey)
	store, err := NewSealedCatalogStore(SealedCatalogStoreConfig{
		ShareInstance:    fixture.share,
		CatalogKey:       catalogKeySource,
		SenderPrivateKey: privateKeySource,
		NonceSource:      &nonceCounter{next: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	ownedCatalogKey := store.key
	ownedPrivateKey := store.privateKey
	clear(catalogKeySource)
	clear(privateKeySource)
	if !bytes.Equal(ownedCatalogKey, fixture.key) || !bytes.Equal(ownedPrivateKey, fixture.privateKey) {
		t.Fatal("sealed catalog store retained caller-owned key storage")
	}

	input := catalogLifecyclePageInput(t, fixture.share, 180)
	sealed, err := store.Seal(input)
	if err != nil {
		t.Fatal(err)
	}
	pageCacheObject := store.pageCache[sealed.Commitment()]
	if len(pageCacheObject) == 0 {
		t.Fatal("page cache was not populated")
	}
	failureIdentity := failureObjectIdentity{
		directory: directoryID(t, 181),
		attempt:   scanAttemptID(t, 182),
	}
	failureCacheObject := []byte("cached failure object")
	store.mu.Lock()
	store.failures[failureIdentity] = storedSenderObject{object: failureCacheObject}
	store.failureOrder = append(store.failureOrder, failureIdentity)
	store.failureBytes = objectCacheEntryOverhead + uint64(len(failureCacheObject))
	store.mu.Unlock()

	store.Destroy()
	store.Destroy()
	if !allCatalogLifecycleBytesZero(ownedCatalogKey) || !allCatalogLifecycleBytesZero(ownedPrivateKey) {
		t.Fatal("destroyed store retained key material")
	}
	if !allCatalogLifecycleBytesZero(pageCacheObject) || !allCatalogLifecycleBytesZero(failureCacheObject) {
		t.Fatal("destroyed store retained cache bytes")
	}
	if store.key != nil || store.privateKey != nil || store.nonces != nil ||
		store.pageCache != nil || store.pageCacheOrder != nil || store.pageCacheBytes != 0 ||
		store.failures != nil || store.failureOrder != nil || store.failureBytes != 0 {
		t.Fatal("destroyed store retained keys, cache state, or nonce source")
	}
	if _, err := store.Seal(input); !errors.Is(err, ErrSealedCatalogStoreDestroyed) {
		t.Fatalf("post-destroy page seal = %v", err)
	}
	if _, err := store.SealFailure(catalog.DirectoryFailureRecord{}); !errors.Is(err, ErrSealedCatalogStoreDestroyed) {
		t.Fatalf("post-destroy failure seal = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.LoadSealedPage(canceled, catalog.CatalogPage{}); !errors.Is(err, ErrSealedCatalogStoreDestroyed) {
		t.Fatalf("post-destroy page load = %v", err)
	}
	if _, err := store.LoadSealedFailure(context.Background(), DirectoryFailure{}); !errors.Is(err, ErrSealedCatalogStoreDestroyed) {
		t.Fatalf("post-destroy failure load = %v", err)
	}

	var nilStore *SealedCatalogStore
	nilStore.Stop()
	nilStore.Destroy()
	if _, err := nilStore.Seal(input); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("nil store seal = %v", err)
	}
}

func TestSealedCatalogStoreReentrantStopAndDestroyDrainActiveNonceCallback(t *testing.T) {
	fixture := newCatalogObjectFixture(t)
	reader := &reentrantStoreStopNonceReader{
		started:      make(chan struct{}),
		stopReturned: make(chan struct{}),
		release:      make(chan struct{}),
	}
	store, err := NewSealedCatalogStore(SealedCatalogStoreConfig{
		ShareInstance:    fixture.share,
		CatalogKey:       fixture.key,
		SenderPrivateKey: fixture.privateKey,
		NonceSource:      reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	reader.store = store
	ownedCatalogKey := store.key
	ownedPrivateKey := store.privateKey
	input := catalogLifecyclePageInput(t, fixture.share, 183)
	sealResult := make(chan error, 1)
	go func() {
		_, sealErr := store.Seal(input)
		sealResult <- sealErr
	}()

	awaitCatalogLifecycleSignal(t, reader.started, "store nonce callback")
	awaitCatalogLifecycleSignal(t, reader.stopReturned, "store reentrant Stop")
	store.Stop()
	if _, err := store.Seal(input); !errors.Is(err, ErrSealedCatalogStoreDestroyed) {
		t.Fatalf("store admitted a page after Stop = %v", err)
	}
	store.lifecycle.mu.Lock()
	active := store.lifecycle.active
	drained := store.lifecycle.drained
	store.lifecycle.mu.Unlock()
	if active != 1 {
		t.Fatalf("active store operations = %d, want 1", active)
	}
	select {
	case <-drained:
		t.Fatal("store drained before the nonce callback returned")
	default:
	}

	destroyed := make(chan struct{})
	go func() {
		store.Destroy()
		close(destroyed)
	}()
	close(reader.release)
	if err := awaitCatalogLifecycleError(t, sealResult, "active store seal"); err != nil {
		t.Fatalf("active store seal after Stop = %v", err)
	}
	awaitCatalogLifecycleSignal(t, destroyed, "store Destroy")
	if !allCatalogLifecycleBytesZero(ownedCatalogKey) || !allCatalogLifecycleBytesZero(ownedPrivateKey) {
		t.Fatal("store Destroy returned before clearing keys")
	}
}

func TestCatalogObjectVerifierDestroyClearsClonedKeyAndRejectsVerify(t *testing.T) {
	fixture := newCatalogObjectFixture(t)
	store := newObjectStore(t, fixture, &nonceCounter{next: 7})
	t.Cleanup(store.Destroy)
	input := catalogLifecyclePageInput(t, fixture.share, 184)
	sealed, err := store.Seal(input)
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewListRequest(input.DirectoryID, nil, input.PageIndex)
	if err != nil {
		t.Fatal(err)
	}

	catalogKeySource := bytes.Clone(fixture.key)
	publicKeySource := bytes.Clone(fixture.publicKey)
	verifier, err := NewCatalogObjectVerifier(CatalogObjectVerifierConfig{
		ShareInstance:   fixture.share,
		CatalogKey:      catalogKeySource,
		SenderPublicKey: publicKeySource,
	})
	if err != nil {
		t.Fatal(err)
	}
	ownedCatalogKey := verifier.key
	ownedPublicKey := verifier.publicKey
	clear(catalogKeySource)
	clear(publicKeySource)
	if _, err := verifier.Verify(context.Background(), fixture.share, request, sealed.Bytes()); err != nil {
		t.Fatalf("verifier did not own cloned key material: %v", err)
	}
	verifier.Stop()
	verifier.Stop()
	if _, err := verifier.Verify(context.Background(), fixture.share, request, sealed.Bytes()); !errors.Is(err, ErrCatalogObjectVerifierDestroyed) {
		t.Fatalf("post-stop Verify = %v", err)
	}

	verifier.Destroy()
	verifier.Destroy()
	if !allCatalogLifecycleBytesZero(ownedCatalogKey) || !allCatalogLifecycleBytesZero(ownedPublicKey) {
		t.Fatal("destroyed verifier retained key material")
	}
	if verifier.key != nil || verifier.publicKey != nil {
		t.Fatal("destroyed verifier retained key slices")
	}
	if _, err := verifier.Verify(context.Background(), fixture.share, request, sealed.Bytes()); !errors.Is(err, ErrCatalogObjectVerifierDestroyed) {
		t.Fatalf("post-destroy Verify = %v", err)
	}

	var nilVerifier *CatalogObjectVerifier
	nilVerifier.Stop()
	nilVerifier.Destroy()
	if _, err := nilVerifier.Verify(context.Background(), fixture.share, request, sealed.Bytes()); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("nil verifier Verify = %v", err)
	}
}

func TestCatalogObjectVerifierDestroyIsSafeWithConcurrentVerify(t *testing.T) {
	fixture := newCatalogObjectFixture(t)
	store := newObjectStore(t, fixture, &nonceCounter{next: 11})
	t.Cleanup(store.Destroy)
	input := catalogLifecyclePageInput(t, fixture.share, 185)
	sealed, err := store.Seal(input)
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewListRequest(input.DirectoryID, nil, input.PageIndex)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewCatalogObjectVerifier(CatalogObjectVerifierConfig{
		ShareInstance:   fixture.share,
		CatalogKey:      fixture.key,
		SenderPublicKey: fixture.publicKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	const workers = 8
	start := make(chan struct{})
	firstSuccess := make(chan struct{})
	var successOnce sync.Once
	var workersWait sync.WaitGroup
	unexpected := make(chan error, workers)
	workersWait.Add(workers)
	for range workers {
		go func() {
			defer workersWait.Done()
			<-start
			for {
				_, verifyErr := verifier.Verify(context.Background(), fixture.share, request, sealed.Bytes())
				switch {
				case verifyErr == nil:
					successOnce.Do(func() { close(firstSuccess) })
				case errors.Is(verifyErr, ErrCatalogObjectVerifierDestroyed):
					return
				default:
					unexpected <- verifyErr
					return
				}
			}
		}()
	}
	close(start)
	awaitCatalogLifecycleSignal(t, firstSuccess, "concurrent Verify")
	ownedCatalogKey := verifier.key
	verifier.Destroy()
	done := make(chan struct{})
	go func() {
		workersWait.Wait()
		close(done)
	}()
	awaitCatalogLifecycleSignal(t, done, "Verify workers")
	close(unexpected)
	for err := range unexpected {
		t.Fatalf("concurrent Verify = %v", err)
	}
	if !allCatalogLifecycleBytesZero(ownedCatalogKey) {
		t.Fatal("verifier Destroy returned before clearing its key")
	}
}

type reentrantStoreStopNonceReader struct {
	store        *SealedCatalogStore
	started      chan struct{}
	stopReturned chan struct{}
	release      chan struct{}
	once         sync.Once
}

func (reader *reentrantStoreStopNonceReader) Read(destination []byte) (int, error) {
	reader.once.Do(func() {
		close(reader.started)
		reader.store.Stop()
		close(reader.stopReturned)
		<-reader.release
	})
	for index := range destination {
		destination[index] = byte(index + 1)
	}
	return len(destination), nil
}

func catalogLifecyclePageInput(
	t *testing.T,
	share catalog.ShareInstance,
	identity byte,
) catalog.PageCommitInput {
	t.Helper()
	entry, err := catalog.NewFileEntry(
		fileID(t, identity+3),
		"secret-lifecycle.bin",
		9,
		catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return catalog.PageCommitInput{
		ShareInstance: share,
		DirectoryID:   directoryID(t, identity),
		Generation:    generationID(t, identity+1),
		Entries:       []catalog.Entry{entry},
		Terminal:      true,
	}
}

func allCatalogLifecycleBytesZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}

func awaitCatalogLifecycleSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func awaitCatalogLifecycleError(t *testing.T, result <-chan error, label string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
		return nil
	}
}
