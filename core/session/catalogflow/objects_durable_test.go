package catalogflow

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/senderobject"
)

type nonceCounter struct {
	next  byte
	reads atomic.Int32
	err   error
}

func (reader *nonceCounter) Read(destination []byte) (int, error) {
	reader.reads.Add(1)
	if reader.err != nil {
		return 0, reader.err
	}
	for index := range destination {
		destination[index] = reader.next
		reader.next++
	}
	return len(destination), nil
}

type catalogObjectFixture struct {
	share      catalog.ShareInstance
	key        []byte
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
}

func newCatalogObjectFixture(t *testing.T) catalogObjectFixture {
	t.Helper()
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x31}, ed25519.SeedSize))
	return catalogObjectFixture{
		share: shareInstance(t, 90), key: bytes.Repeat([]byte{0x42}, 32), privateKey: privateKey,
		publicKey: privateKey.Public().(ed25519.PublicKey),
	}
}

func newObjectStore(t *testing.T, fixture catalogObjectFixture, nonces io.Reader) *SealedCatalogStore {
	t.Helper()
	store, err := NewSealedCatalogStore(SealedCatalogStoreConfig{
		ShareInstance: fixture.share, CatalogKey: fixture.key,
		SenderPrivateKey: fixture.privateKey, NonceSource: nonces,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestDescriptorAndControlCodecsAuthenticateCanonicalValues(t *testing.T) {
	fixture := newCatalogObjectFixture(t)
	descriptor, err := catalog.NewReceivedShareDescriptor(catalog.ReceivedDescriptorSpec{
		WireVersion: catalog.WireVersionV2, Suite: catalog.SuiteV2,
		ShareInstance: fixture.share, SyntheticRoot: directoryID(t, 91),
		ChunkSize: catalog.MinChunkSize, Capabilities: catalog.CapabilityCatalog | catalog.CapabilityRanges,
		SenderPublicKey: fixture.publicKey, CreatedAtSeconds: 123, PathPolicy: catalog.PathPolicyV1,
	})
	if err != nil {
		t.Fatal(err)
	}
	pkHash, err := link.SenderKeyHash(fixture.publicKey)
	if err != nil {
		t.Fatal(err)
	}
	shareID, err := link.ShareIDForSenderKeyHash(pkHash[:])
	if err != nil {
		t.Fatal(err)
	}
	shareIDRaw, err := base64.RawURLEncoding.Strict().DecodeString(shareID)
	if err != nil {
		t.Fatal(err)
	}
	descriptorKey := bytes.Repeat([]byte{0x53}, 32)
	object, err := SealDescriptor(descriptor, DescriptorObjectConfig{
		PKHash: pkHash[:], ShareIDRaw: shareIDRaw, DescriptorKey: descriptorKey,
		SenderPrivateKey: fixture.privateKey, Nonce: bytes.Repeat([]byte{0x64}, senderobject.NonceBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenDescriptor(object, pkHash[:], shareIDRaw, descriptorKey)
	if err != nil {
		t.Fatal(err)
	}
	if opened.ShareInstance() != descriptor.ShareInstance() || opened.SyntheticRoot() != descriptor.SyntheticRoot() ||
		!bytes.Equal(opened.SenderPublicKey(), descriptor.SenderPublicKey()) {
		t.Fatal("authenticated descriptor changed identity")
	}
	tampered := bytes.Clone(object)
	tampered[len(tampered)-1] ^= 1
	if _, err := OpenDescriptor(tampered, pkHash[:], shareIDRaw, descriptorKey); err == nil {
		t.Fatal("tampered descriptor object was accepted")
	}
	if _, err := SealDescriptor(descriptor, DescriptorObjectConfig{}); err == nil {
		t.Fatal("invalid descriptor binding was accepted")
	}
	if _, err := SealDescriptor(catalog.ShareDescriptor{}, DescriptorObjectConfig{}); !errors.Is(err, ErrWireObject) {
		t.Fatalf("zero descriptor seal = %v", err)
	}
	if _, err := encodeShareDescriptor(catalog.ShareDescriptor{}); !errors.Is(err, ErrWireObject) {
		t.Fatalf("zero descriptor encoding = %v", err)
	}
	encodedDescriptor, err := encodeShareDescriptor(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	for name, hostile := range map[string][]byte{
		"empty":         nil,
		"trailing byte": append(bytes.Clone(encodedDescriptor), 0),
		"wrong schema":  mustCBOR(t, catalogWireEnc, map[uint64]any{0: uint64(2)}),
	} {
		t.Run("descriptor "+name, func(t *testing.T) {
			if _, err := decodeShareDescriptor(hostile); err == nil {
				t.Fatal("hostile descriptor was accepted")
			}
		})
	}

	payload := []byte("opaque sender object")
	encodedResult, err := EncodeCatalogResult(payload)
	if err != nil {
		t.Fatal(err)
	}
	decodedResult, err := DecodeCatalogResult(encodedResult)
	if err != nil || !bytes.Equal(decodedResult, payload) {
		t.Fatalf("catalog result round trip = %q err=%v", decodedResult, err)
	}
	if _, err := EncodeCatalogResult(nil); !errors.Is(err, ErrCatalogCodec) {
		t.Fatalf("empty result encoding = %v", err)
	}
	if _, err := DecodeCatalogResult(append(encodedResult, 0)); !errors.Is(err, ErrCatalogCodec) {
		t.Fatalf("noncanonical result decoding = %v", err)
	}
	permanent := mustDirectoryFailure(t, fixture.share, directoryID(t, 92), 93, DirectoryCodePermission, false)
	retryable := mustDirectoryFailure(t, fixture.share, directoryID(t, 94), 95, DirectoryCodeTransientIO, true)
	for _, failure := range []DirectoryFailure{permanent, retryable} {
		encoded, err := EncodeDirectoryFailure(failure)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeDirectoryFailure(encoded)
		if err != nil || decoded != failure || decoded.Error() == "" {
			t.Fatalf("failure round trip = %+v err=%v", decoded, err)
		}
		if _, err := DecodeDirectoryFailure(append(encoded, 0)); !errors.Is(err, ErrCatalogCodec) {
			t.Fatalf("noncanonical failure decoding = %v", err)
		}
	}
	if _, err := EncodeDirectoryFailure(DirectoryFailure{}); err == nil {
		t.Fatal("zero failure was encoded")
	}
	malformedFailure := mustCBOR(t, catalogFlowEnc, map[uint64]any{
		0: catalogControlSchema, 1: fixture.share.Bytes(), 2: directoryID(t, 96).Bytes(),
		3: scanAttemptID(t, 97).Bytes(), 4: uint64(DirectoryCodePermanentIO), 5: false, 6: uint64(1),
	})
	if _, err := DecodeDirectoryFailure(malformedFailure); !errors.Is(err, ErrCatalogCodec) {
		t.Fatalf("permanent failure with retry delay = %v", err)
	}
}

func TestCatalogPageCodecPreservesEntriesTimesAndRejectsHostileCBOR(t *testing.T) {
	instance := shareInstance(t, 100)
	directory := directoryID(t, 101)
	generation := generationID(t, 102)
	modified, err := catalog.NewModifiedTime(-123, 456_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	directoryEntry, err := catalog.NewDirectoryEntry(directoryID(t, 103), "folder", modified)
	if err != nil {
		t.Fatal(err)
	}
	fileEntry, err := catalog.NewFileEntry(fileID(t, 104), "file.bin", 99, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: instance, DirectoryID: directory, Generation: generation,
		Entries: []catalog.Entry{fileEntry, directoryEntry}, Terminal: true, OmittedCount: 7,
	}, testCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeCatalogPage(page)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeCatalogPage(encoded, testCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	entries := decoded.Entries()
	if len(entries) != 2 || entries[0].ExpectedSize() != 99 || entries[1].Name() != "folder" ||
		entries[1].ModifiedTime() != modified || !decoded.Terminal() || decoded.OmittedCount() != 7 {
		t.Fatalf("decoded page changed semantics: %+v", decoded)
	}
	if _, err := decodeCatalogPage(encoded, nil); !errors.Is(err, ErrWireObject) {
		t.Fatalf("nil committer = %v", err)
	}
	for name, hostile := range map[string][]byte{
		"empty":         nil,
		"trailing byte": append(bytes.Clone(encoded), 0),
		"wrong schema": mustCBOR(t, catalogWireEnc, map[uint64]any{
			0: uint64(2), 1: instance.Bytes(), 2: directory.Bytes(), 3: generation.Bytes(),
			4: uint64(0), 5: true, 6: make([]byte, catalog.PageCommitmentBytes), 7: []any{}, 8: uint64(0),
		}),
		"bad entry kind": mustCBOR(t, catalogWireEnc, map[uint64]any{
			0: wireSchemaVersion, 1: instance.Bytes(), 2: directory.Bytes(), 3: generation.Bytes(),
			4: uint64(0), 5: true, 6: make([]byte, catalog.PageCommitmentBytes),
			7: []any{[]any{uint64(99), fileID(t, 105).Bytes(), "bad", nil, nil, uint64(0), uint64(0)}}, 8: uint64(0),
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeCatalogPage(hostile, testCommitter{}); err == nil {
				t.Fatal("hostile page was accepted")
			}
		})
	}
	if _, err := encodeCatalogPage(catalog.CatalogPage{}); !errors.Is(err, ErrWireObject) {
		t.Fatalf("zero page encoding = %v", err)
	}
	if _, err := decodeWireModified(mustRawCBOR(t, nil), mustRawCBOR(t, uint64(1)), mustRawCBOR(t, uint64(0))); err == nil {
		t.Fatal("absent modified time with nonzero nanos was accepted")
	}
}

func TestSealedPageObjectsVerifyAndCacheEvictionCannotAffectAddressedServing(t *testing.T) {
	fixture := newCatalogObjectFixture(t)
	nonces := &nonceCounter{next: 1}
	objects := newObjectStore(t, fixture, nonces)
	directory := directoryID(t, 110)
	generation := generationID(t, 111)
	entry, err := catalog.NewFileEntry(fileID(t, 112), "first.bin", 1, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	input := catalog.PageCommitInput{
		ShareInstance: fixture.share, DirectoryID: directory, Generation: generation,
		Entries: []catalog.Entry{entry},
	}
	sealed, err := objects.Seal(input)
	if err != nil {
		t.Fatal(err)
	}
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: fixture.share, DirectoryID: directory, Generation: generation,
		Entries: input.Entries,
	}, catalog.PageCommitterFunc(func(catalog.PageCommitInput) (catalog.PageCommitment, error) {
		return sealed.Commitment(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := objects.LoadSealedPage(context.Background(), page)
	if err != nil || !bytes.Equal(loaded, sealed.Bytes()) {
		t.Fatalf("cached page object changed: err=%v", err)
	}
	verifier, err := NewCatalogObjectVerifier(CatalogObjectVerifierConfig{
		ShareInstance: fixture.share, CatalogKey: fixture.key, SenderPublicKey: fixture.publicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := NewListRequest(directory, nil, 0)
	verified, err := verifier.Verify(context.Background(), fixture.share, request, sealed.Bytes())
	if err != nil || verified.Page.DirectoryID() != directory || verified.Page.Commitment() != sealed.Commitment() {
		t.Fatalf("verified page = %+v err=%v", verified, err)
	}
	tampered := sealed.Bytes()
	tampered[len(tampered)-1] ^= 1
	if _, err := verifier.Verify(context.Background(), fixture.share, request, tampered); err == nil {
		t.Fatal("tampered page object was verified")
	}
	if _, err := verifier.Verify(context.Background(), shareInstance(t, 113), request, sealed.Bytes()); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("wrong-share verification = %v", err)
	}
	objects.mu.Lock()
	objects.maxCacheBytes = objects.pageCacheBytes
	objects.mu.Unlock()
	secondInput := input
	secondInput.PageIndex = 1
	secondInput.Previous = sealed.Commitment()
	secondInput.Terminal = true
	second, err := objects.Seal(secondInput)
	if err != nil {
		t.Fatal(err)
	}
	objects.cachePage(second)
	if _, err := objects.LoadSealedPage(context.Background(), page); !errors.Is(err, ErrCatalogObjectMissing) {
		t.Fatalf("old page remained after bounded eviction: %v", err)
	}
	if nonces.reads.Load() != 2 {
		t.Fatalf("page sealing nonce reads = %d", nonces.reads.Load())
	}
	if _, err := NewSealedCatalogStore(SealedCatalogStoreConfig{}); err == nil {
		t.Fatal("invalid object store configuration was accepted")
	}
	if _, err := NewCatalogObjectVerifier(CatalogObjectVerifierConfig{}); err == nil {
		t.Fatal("invalid verifier configuration was accepted")
	}
}

func TestAddressedPageServiceUsesDurableBackendAfterObjectCacheEviction(t *testing.T) {
	fixture := newCatalogObjectFixture(t)
	objects := newObjectStore(t, fixture, &nonceCounter{next: 20})
	process, _ := catalog.NewBudgetAccount("process", catalog.DefaultProcessBudgetLimits())
	shareBudget, _ := catalog.NewBudgetAccount("share", catalog.DefaultShareBudgetLimits())
	session, _ := catalog.NewBudgetAccount("session", catalog.DefaultSessionBudgetLimits())
	store, err := catalog.NewCatalogStore(catalog.StoreConfig{
		ShareInstance: fixture.share, Backend: catalog.NewMemoryCatalogBackend(),
		ProcessBudget: process, ShareBudget: shareBudget, PageSealer: objects,
		AttemptIDs: catalog.ScanAttemptIDGeneratorFunc(func() (catalog.ScanAttemptID, error) {
			return scanAttemptID(t, 120), nil
		}),
		Generations: catalog.DirectoryGenerationGeneratorFunc(func() (catalog.DirectoryGeneration, error) {
			return generationID(t, 121), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := directoryID(t, 122)
	directory := directoryID(t, 123)
	unscanned := directoryID(t, 126)
	commitSelectedDirectories(t, store, fixture.share, root, []catalog.DirectoryID{directory, unscanned}, session)
	var scans atomic.Int32
	scanner := catalog.DirectoryScannerFunc(func(ctx context.Context, request catalog.ScanRequest) (catalog.ScanResult, error) {
		scans.Add(1)
		locator, _ := catalog.NewLocator(0, "served.bin")
		identity, _ := catalog.NewSourceIdentity([]byte("served-source"))
		version, _ := catalog.NewVersionCandidate([]byte("served-version"))
		return catalog.ScanResult{}, request.Children.Add(ctx, catalog.ScannedChild{
			FileID: fileID(t, 124), Name: "served.bin", Locator: locator,
			SourceIdentity: identity, VersionCandidate: version, ExpectedSize: 5,
		})
	})
	source, err := NewCatalogStoreSource(CatalogStoreSourceConfig{
		ShareInstance: fixture.share, Store: store, SessionBudget: session, Scanner: scanner,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := NewListRequest(directory, nil, 0)
	result, err := source.LoadPage(context.Background(), request, nil)
	if err != nil || result.Page.DirectoryID() != directory || len(result.SealedObject) == 0 {
		t.Fatalf("addressed page result = %+v err=%v", result, err)
	}
	objects.mu.Lock()
	objects.pageCache = make(map[catalog.PageCommitment][]byte)
	objects.pageCacheOrder = nil
	objects.pageCacheBytes = 0
	objects.mu.Unlock()
	service, err := NewAddressedSenderService(fixture.share, source)
	if err != nil {
		t.Fatal(err)
	}
	served, err := service.Serve(context.Background(), request, nil)
	if err != nil || !bytes.Equal(served, result.SealedObject) || scans.Load() != 1 {
		t.Fatalf("durable addressed serving changed object: err=%v scans=%d", err, scans.Load())
	}
	verifier, _ := NewCatalogObjectVerifier(CatalogObjectVerifierConfig{
		ShareInstance: fixture.share, CatalogKey: fixture.key, SenderPublicKey: fixture.publicKey,
	})
	if _, err := verifier.Verify(context.Background(), fixture.share, request, served); err != nil {
		t.Fatal(err)
	}
	currentGeneration := result.Page.Generation()
	outOfRange, _ := NewListRequest(directory, &currentGeneration, 9)
	if _, err := source.LoadPage(context.Background(), outOfRange, nil); !errors.Is(err, ErrPageOutOfRange) {
		t.Fatalf("out-of-range page = %v", err)
	}
	stale := generationID(t, 125)
	staleRequest, _ := NewListRequest(directory, &stale, 0)
	if _, err := source.LoadPage(context.Background(), staleRequest, nil); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("stale generation = %v", err)
	}
	unscannedGeneration := generationID(t, 127)
	unscannedLater, _ := NewListRequest(unscanned, &unscannedGeneration, 1)
	if _, err := source.LoadPage(context.Background(), unscannedLater, nil); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("later page triggered an uncommitted scan: %v", err)
	}
	wrongSource, _ := NewCatalogStoreSource(CatalogStoreSourceConfig{
		ShareInstance: shareInstance(t, 128), Store: store, SessionBudget: session, Scanner: scanner,
	})
	if _, err := wrongSource.LoadPage(context.Background(), request, nil); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("source accepted another share's committed page: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.LoadPage(cancelled, request, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled page load = %v", err)
	}
	if _, err := NewCatalogStoreSource(CatalogStoreSourceConfig{}); err == nil {
		t.Fatal("invalid catalog store source was accepted")
	}
	if _, err := NewAddressedSenderService(catalog.ShareInstance{}, source); err == nil {
		t.Fatal("invalid addressed service was accepted")
	}
}

func TestDurableFailureObjectSurvivesCacheEvictionAndRestartWithoutReseal(t *testing.T) {
	fixture := newCatalogObjectFixture(t)
	rootPath := t.TempDir()
	nonces := &nonceCounter{next: 40}
	objects := newObjectStore(t, fixture, nonces)
	process, _ := catalog.NewBudgetAccount("process", catalog.DefaultProcessBudgetLimits())
	shareBudget, _ := catalog.NewBudgetAccount("share", catalog.DefaultShareBudgetLimits())
	session, _ := catalog.NewBudgetAccount("session", catalog.DefaultSessionBudgetLimits())
	backend, err := catalog.NewFileCatalogBackend(catalog.FileCatalogBackendConfig{
		Root: filepath.Join(rootPath, "catalog"), ShareInstance: fixture.share,
	})
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Uint32
	store, err := catalog.NewCatalogStore(catalog.StoreConfig{
		ShareInstance: fixture.share, Backend: backend, ProcessBudget: process, ShareBudget: shareBudget,
		PageSealer: objects, SpillFactory: catalog.NewFileSpillFactory(filepath.Join(rootPath, "sort")),
		AttemptIDs: catalog.ScanAttemptIDGeneratorFunc(func() (catalog.ScanAttemptID, error) {
			return scanAttemptID(t, byte(130+attempts.Add(1))), nil
		}),
		Generations: catalog.DirectoryGenerationGeneratorFunc(func() (catalog.DirectoryGeneration, error) {
			return generationID(t, byte(140+attempts.Load())), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	root := directoryID(t, 150)
	firstDirectory := directoryID(t, 151)
	secondDirectory := directoryID(t, 152)
	commitSelectedDirectories(t, store, fixture.share, root, []catalog.DirectoryID{firstDirectory, secondDirectory}, session)
	scanner := catalog.DirectoryScannerFunc(func(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
		return catalog.ScanResult{}, catalog.NewPermanentScanError(errors.New("permission denied"))
	})
	_, firstErr := store.ListChildren(context.Background(), firstDirectory, session, catalog.ScanOptions{}, scanner)
	var firstFailure *catalog.DirectoryFailure
	if !errors.As(firstErr, &firstFailure) {
		t.Fatalf("first failure = %v", firstErr)
	}
	firstObject, found, err := store.FailureObject(context.Background(), firstDirectory, firstFailure.AttemptID)
	if err != nil || !found {
		t.Fatalf("first durable object: found=%v err=%v", found, err)
	}
	objects.mu.Lock()
	objects.maxFailureBytes = objects.failureBytes
	objects.mu.Unlock()
	if _, err := store.ListChildren(context.Background(), secondDirectory, session, catalog.ScanOptions{}, scanner); err == nil {
		t.Fatal("second permanent failure was accepted")
	}
	firstWire := DirectoryFailure{
		ShareInstance: fixture.share, DirectoryID: firstDirectory, AttemptID: firstFailure.AttemptID,
		Code: DirectoryCodePermanentIO,
	}
	if _, err := objects.LoadSealedFailure(context.Background(), firstWire); !errors.Is(err, ErrCatalogObjectMissing) {
		t.Fatalf("first failure cache entry was not evicted: %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	restartNonces := &nonceCounter{err: errors.New("restart attempted to reseal")}
	restartObjects := newObjectStore(t, fixture, restartNonces)
	recoveredProcess, _ := catalog.NewBudgetAccount("recovered-process", catalog.DefaultProcessBudgetLimits())
	recoveredShare, _ := catalog.NewBudgetAccount("recovered-share", catalog.DefaultShareBudgetLimits())
	recoveredSession, _ := catalog.NewBudgetAccount("recovered-session", catalog.DefaultSessionBudgetLimits())
	recoveredBackend, err := catalog.NewFileCatalogBackend(catalog.FileCatalogBackendConfig{
		Root: filepath.Join(rootPath, "catalog"), ShareInstance: fixture.share,
	})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := catalog.NewCatalogStore(catalog.StoreConfig{
		ShareInstance: fixture.share, Backend: recoveredBackend,
		ProcessBudget: recoveredProcess, ShareBudget: recoveredShare, PageSealer: restartObjects,
		SpillFactory: catalog.NewFileSpillFactory(filepath.Join(rootPath, "sort")),
	})
	if err != nil {
		t.Fatal(err)
	}
	var rescans atomic.Int32
	never := catalog.DirectoryScannerFunc(func(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
		rescans.Add(1)
		return catalog.ScanResult{}, errors.New("recovered failure reached scanner")
	})
	source, err := NewCatalogStoreSource(CatalogStoreSourceConfig{
		ShareInstance: fixture.share, Store: recovered, SessionBudget: recoveredSession, Scanner: never,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := NewListRequest(firstDirectory, nil, 0)
	result, err := source.LoadPage(context.Background(), request, nil)
	if err != nil || result.Failure == nil || result.Failure.AttemptID != firstFailure.AttemptID ||
		!bytes.Equal(result.SealedObject, firstObject.Bytes()) {
		t.Fatalf("recovered failure result = %+v err=%v", result, err)
	}
	service, _ := NewAddressedSenderService(fixture.share, source)
	served, err := service.Serve(context.Background(), request, nil)
	if err != nil || !bytes.Equal(served, firstObject.Bytes()) {
		t.Fatalf("restarted service changed failure bytes: %v", err)
	}
	verifier, _ := NewCatalogObjectVerifier(CatalogObjectVerifierConfig{
		ShareInstance: fixture.share, CatalogKey: fixture.key, SenderPublicKey: fixture.publicKey,
	})
	verified, err := verifier.Verify(context.Background(), fixture.share, request, served)
	if err != nil || verified.Failure == nil || verified.Failure.AttemptID != firstFailure.AttemptID {
		t.Fatalf("verified recovered failure = %+v err=%v", verified, err)
	}
	failureGeneration := generationID(t, 153)
	laterRequest, _ := NewListRequest(firstDirectory, &failureGeneration, 1)
	if _, err := verifier.Verify(context.Background(), fixture.share, laterRequest, served); !errors.Is(err, senderobject.ErrSignature) {
		t.Fatalf("failure object answered a later-page request: %v", err)
	}
	if restartNonces.reads.Load() != 0 || rescans.Load() != 0 {
		t.Fatalf("restart resealed/rescanned: nonce reads=%d scans=%d", restartNonces.reads.Load(), rescans.Load())
	}
	if recoveredShare.Snapshot().Used == (catalog.ResourceUsage{}) {
		t.Fatal("recovered durable authority was not budgeted")
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	if recoveredProcess.Snapshot().Used != (catalog.ResourceUsage{}) ||
		recoveredShare.Snapshot().Used != (catalog.ResourceUsage{}) {
		t.Fatal("closing recovered store did not release durable authority budget")
	}
	_ = store.Close()
}

type addressedSourceFunc func(context.Context, ListRequest) (PageResult, error)

func (function addressedSourceFunc) LoadPage(
	ctx context.Context,
	request ListRequest,
	_ catalog.ScanProgressObserver,
) (PageResult, error) {
	return function(ctx, request)
}

func TestHostileCatalogObjectsAndWireFieldsFailClosed(t *testing.T) {
	fixture := newCatalogObjectFixture(t)
	directory := directoryID(t, 170)
	generation := generationID(t, 171)
	entry, _ := catalog.NewFileEntry(fileID(t, 172), "hostile.bin", 1, catalog.ModifiedTime{})
	input := catalog.PageCommitInput{
		ShareInstance: fixture.share, DirectoryID: directory, Generation: generation,
		Entries: []catalog.Entry{entry}, Terminal: true,
	}
	objects := newObjectStore(t, fixture, &nonceCounter{next: 70})
	if _, err := (*SealedCatalogStore)(nil).Seal(input); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("nil page sealer = %v", err)
	}
	wrongShare := input
	wrongShare.ShareInstance = shareInstance(t, 173)
	if _, err := objects.Seal(wrongShare); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("wrong-share page seal = %v", err)
	}
	badNonces := newObjectStore(t, fixture, &nonceCounter{err: errors.New("nonce failed")})
	if _, err := badNonces.Seal(input); err == nil {
		t.Fatal("nonce failure was ignored")
	}
	badDirectory := input
	badDirectory.DirectoryID = catalog.DirectoryID{}
	if _, err := objects.Seal(badDirectory); err == nil {
		t.Fatal("zero-directory page was sealed")
	}
	sealed, err := objects.Seal(input)
	if err != nil {
		t.Fatal(err)
	}
	objects.mu.Lock()
	objects.pageCache = make(map[catalog.PageCommitment][]byte)
	objects.pageCacheOrder = nil
	objects.pageCacheBytes = 0
	objects.maxCacheBytes = 1
	objects.mu.Unlock()
	objects.cachePage(sealed)
	if _, err := objects.LoadSealedPage(context.Background(), catalogPageForSealedObject(t, input, sealed)); !errors.Is(err, ErrCatalogObjectMissing) {
		t.Fatalf("oversized page cache entry was retained: %v", err)
	}
	objects.mu.Lock()
	objects.maxFailureBytes = 1
	objects.cacheFailureLocked(
		failureObjectIdentity{directory: directory, attempt: scanAttemptID(t, 177)},
		sha256.Sum256([]byte("failure")), []byte("failure"),
	)
	if len(objects.failures) != 0 {
		objects.mu.Unlock()
		t.Fatal("oversized failure cache entry was retained")
	}
	objects.mu.Unlock()
	if _, err := objects.LoadSealedPage(context.Background(), catalog.CatalogPage{}); !errors.Is(err, ErrCatalogObjectMissing) {
		t.Fatalf("zero cached page = %v", err)
	}
	if _, err := (*SealedCatalogStore)(nil).LoadSealedFailure(context.Background(), DirectoryFailure{}); !errors.Is(err, ErrCatalogObjectMissing) {
		t.Fatalf("nil failure cache = %v", err)
	}
	if _, err := objects.LoadSealedFailure(context.Background(), DirectoryFailure{
		ShareInstance: shareInstance(t, 175), DirectoryID: directory, AttemptID: scanAttemptID(t, 176),
	}); !errors.Is(err, ErrCatalogObjectMissing) {
		t.Fatalf("failure cache accepted another share: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := objects.LoadSealedPage(cancelled, catalog.CatalogPage{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cache read = %v", err)
	}
	if _, err := objects.LoadSealedFailure(cancelled, DirectoryFailure{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled failure cache read = %v", err)
	}

	for kind, want := range map[catalog.FailureKind]uint16{
		catalog.FailureKindStale:       DirectoryCodeStale,
		catalog.FailureKindPermission:  DirectoryCodePermission,
		catalog.FailureKindCollision:   DirectoryCodeCollision,
		catalog.FailureKindTooWide:     DirectoryCodeTooWide,
		catalog.FailureKindBudget:      DirectoryCodeBudget,
		catalog.FailureKindPermanentIO: DirectoryCodePermanentIO,
		catalog.FailureKindTransientIO: DirectoryCodeTransientIO,
	} {
		if got, err := directoryCodeForFailureKind(kind); err != nil || got != want {
			t.Fatalf("failure kind %d = %#x err=%v", kind, got, err)
		}
	}
	if _, err := directoryCodeForFailureKind(catalog.FailureKindUnknown); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("unknown failure kind = %v", err)
	}

	if _, err := decodeWireMap(nil, 1); !errors.Is(err, ErrWireObject) {
		t.Fatalf("empty wire map = %v", err)
	}
	if _, err := decodeWireMap(mustCBOR(t, catalogWireEnc, map[uint64]any{1: uint64(1)}), 1); !errors.Is(err, ErrWireObject) {
		t.Fatalf("gapped wire map = %v", err)
	}
	if _, err := wireUint(mustRawCBOR(t, "not uint")); !errors.Is(err, ErrWireObject) {
		t.Fatalf("text uint = %v", err)
	}
	if value, err := wireInt(mustRawCBOR(t, int64(-7))); err != nil || value != -7 {
		t.Fatalf("signed integer = %d err=%v", value, err)
	}
	if value, err := wireInt(mustRawCBOR(t, uint64(7))); err != nil || value != 7 {
		t.Fatalf("unsigned integer = %d err=%v", value, err)
	}
	if _, err := wireInt(mustRawCBOR(t, uint64(^uint64(0)))); !errors.Is(err, ErrWireObject) {
		t.Fatalf("overflow integer = %v", err)
	}
	if _, err := wireInt(mustRawCBOR(t, "not int")); !errors.Is(err, ErrWireObject) {
		t.Fatalf("text integer = %v", err)
	}
	if _, err := wireBytes(mustRawCBOR(t, []byte{1}), 2); !errors.Is(err, ErrWireObject) {
		t.Fatalf("wrong-length bytes = %v", err)
	}
	if _, err := wireText(mustRawCBOR(t, uint64(1))); !errors.Is(err, ErrWireObject) {
		t.Fatalf("integer text = %v", err)
	}
	if _, err := wireBool(mustRawCBOR(t, uint64(1))); !errors.Is(err, ErrWireObject) {
		t.Fatalf("integer bool = %v", err)
	}
	if _, err := encodeCatalogPageFields(catalog.PageCommitInput{Entries: []catalog.Entry{{}}}); !errors.Is(err, ErrWireObject) {
		t.Fatalf("zero entry encoding = %v", err)
	}
	badDirectoryEntry := mustRawCBOR(t, []any{
		uint64(catalog.NodeKindDirectory), directory.Bytes(), "directory", uint64(1), nil, uint64(0), uint64(0),
	})
	if _, err := decodeWireEntry(badDirectoryEntry); !errors.Is(err, ErrWireObject) {
		t.Fatalf("directory with size = %v", err)
	}
	badFileEntry := mustRawCBOR(t, []any{
		uint64(catalog.NodeKindFile), fileID(t, 174).Bytes(), "file", uint64(catalog.MaxFileSize) + 1,
		nil, uint64(0), uint64(0),
	})
	if _, err := decodeWireEntry(badFileEntry); !errors.Is(err, ErrWireObject) {
		t.Fatalf("oversized file entry = %v", err)
	}

	if _, err := decodeCatalogMap(mustCBOR(t, catalogFlowEnc, map[uint64]any{1: uint64(1)}), 1); !errors.Is(err, ErrCatalogCodec) {
		t.Fatalf("gapped control map = %v", err)
	}
	if _, err := catalogUint(mustCBOR(t, catalogFlowEnc, "not uint")); !errors.Is(err, ErrCatalogCodec) {
		t.Fatalf("text control uint = %v", err)
	}
	if _, err := catalogBytes(mustCBOR(t, catalogFlowEnc, []byte{1}), 2); !errors.Is(err, ErrCatalogCodec) {
		t.Fatalf("wrong-length control bytes = %v", err)
	}

	page, _ := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: fixture.share, DirectoryID: directory, Generation: generation,
		Entries: []catalog.Entry{entry}, Terminal: true,
	}, testCommitter{})
	request, _ := NewListRequest(directory, nil, 0)
	errorService, _ := NewAddressedSenderService(fixture.share, addressedSourceFunc(
		func(context.Context, ListRequest) (PageResult, error) {
			return PageResult{}, errors.New("source failed")
		},
	))
	if _, err := errorService.Serve(context.Background(), request, nil); err == nil {
		t.Fatal("addressed source error was hidden")
	}
	invalidFailureService, _ := NewAddressedSenderService(fixture.share, addressedSourceFunc(
		func(context.Context, ListRequest) (PageResult, error) {
			return PageResult{Failure: &DirectoryFailure{}, Page: page, SealedObject: []byte{1}}, nil
		},
	))
	if _, err := invalidFailureService.Serve(context.Background(), request, nil); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("mixed page/failure result = %v", err)
	}
	invalidPageService, _ := NewAddressedSenderService(fixture.share, addressedSourceFunc(
		func(context.Context, ListRequest) (PageResult, error) {
			return PageResult{Page: page, SealedObject: []byte{1}}, nil
		},
	))
	if _, err := invalidPageService.Serve(context.Background(), request, nil); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("uncommitted page object = %v", err)
	}
	otherPage, _ := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: shareInstance(t, 178), DirectoryID: directory, Generation: generation,
		Entries: []catalog.Entry{entry}, Terminal: true,
	}, testCommitter{})
	wrongShareService, _ := NewAddressedSenderService(fixture.share, addressedSourceFunc(
		func(context.Context, ListRequest) (PageResult, error) {
			return PageResult{Page: otherPage, SealedObject: []byte{1}}, nil
		},
	))
	if _, err := wrongShareService.Serve(context.Background(), request, nil); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("another share's page result = %v", err)
	}

	verifier, _ := NewCatalogObjectVerifier(CatalogObjectVerifierConfig{
		ShareInstance: fixture.share, CatalogKey: fixture.key, SenderPublicKey: fixture.publicKey,
	})
	if _, err := verifier.Verify(cancelled, fixture.share, request, []byte{1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled verification = %v", err)
	}
}

func catalogPageForSealedObject(
	t *testing.T,
	input catalog.PageCommitInput,
	object catalog.SealedPageObject,
) catalog.CatalogPage {
	t.Helper()
	page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: input.ShareInstance, DirectoryID: input.DirectoryID, Generation: input.Generation,
		PageIndex: input.PageIndex, Previous: input.Previous, Entries: input.Entries,
		Terminal: input.Terminal, OmittedCount: input.OmittedCount,
	}, catalog.PageCommitterFunc(func(catalog.PageCommitInput) (catalog.PageCommitment, error) {
		return object.Commitment(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func commitSelectedDirectories(
	t *testing.T,
	store *catalog.CatalogStore,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	directories []catalog.DirectoryID,
	session *catalog.BudgetAccount,
) {
	t.Helper()
	records := make([]catalog.NodeRecord, len(directories))
	for index, directory := range directories {
		locator, _ := catalog.NewLocator(catalog.RootSlot(index), "")
		identity, _ := catalog.NewSourceIdentity([]byte{byte(index + 1)})
		record, err := catalog.NewDirectoryNodeRecord(
			directory, root, "directory-"+string(rune('a'+index)), locator, identity, catalog.ModifiedTime{},
		)
		if err != nil {
			t.Fatal(err)
		}
		records[index] = record
	}
	commit, err := catalog.NewSyntheticRootCommit(catalog.SyntheticRootCommitSpec{
		ShareInstance: share, SyntheticRoot: root, Generation: generationID(t, 160), SelectedRoots: records,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitSyntheticRoot(context.Background(), commit, session); err != nil {
		t.Fatal(err)
	}
}

type cborMarshaler interface {
	Marshal(any) ([]byte, error)
}

func mustCBOR(t *testing.T, mode cborMarshaler, value any) []byte {
	t.Helper()
	encoded, err := mode.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func mustRawCBOR(t *testing.T, value any) []byte {
	t.Helper()
	return mustCBOR(t, catalogWireEnc, value)
}
