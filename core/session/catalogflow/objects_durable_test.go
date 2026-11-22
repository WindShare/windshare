package catalogflow

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/senderobject"
)

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

func TestDescriptorAndControlCodecsAuthenticateCanonicalValues(t *testing.T) {
	fixture := newCatalogObjectFixture(t)
	descriptor, err := catalog.NewReceivedShareDescriptor(catalog.ReceivedDescriptorSpec{
		WireVersion: catalog.WireVersionV2, Suite: catalog.SuiteV2,
		ShareInstance: fixture.share, SyntheticRoot: directoryID(t, 91),
		ChunkSize: catalog.MinChunkSize, Capabilities: catalog.CapabilityCatalog | catalog.CapabilityRanges,
		SenderPublicKey: fixture.publicKey, CreatedAtSeconds: 123, PathPolicy: catalog.PathPolicy,
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
	if !bytes.Equal(opened.Sender.PublicKey(), descriptor.SenderPublicKey()) {
		t.Fatal("bootstrap lost its authenticated sender verifier")
	}
	if opened.Descriptor.ShareInstance() != descriptor.ShareInstance() || opened.Descriptor.SyntheticRoot() != descriptor.SyntheticRoot() ||
		!bytes.Equal(opened.Descriptor.SenderPublicKey(), descriptor.SenderPublicKey()) {
		t.Fatal("authenticated descriptor changed identity")
	}
	tampered := bytes.Clone(object)
	tampered = append(tampered, 0)
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
	_, payload, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: instance, DirectoryID: directory, Generation: generation,
		Entries: []catalog.Entry{fileEntry, directoryEntry}, Terminal: true, OmittedCount: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := payload.Bytes()
	wire := mustCBOR(t, catalogWireEnc, map[uint64]any{
		0: uint64(1), 1: instance.Bytes(), 2: directory.Bytes(), 3: generation.Bytes(),
		4: uint64(0), 5: true, 6: make([]byte, catalog.PageCommitmentBytes),
		7: [][]any{
			{uint64(catalog.NodeKindFile), fileEntry.NodeID().Bytes(), "file.bin", uint64(99), nil, uint64(0), uint64(0)},
			{uint64(catalog.NodeKindDirectory), directoryEntry.NodeID().Bytes(), "folder", nil, int64(-123), uint64(456_000_000), uint64(catalog.TimePrecisionMilliseconds)},
		},
		8: uint64(7),
	})
	if !bytes.Equal(encoded, wire) {
		t.Fatalf("committed page changed protocol bytes: got %x want %x", encoded, wire)
	}
	decoded, err := catalog.DecodeCatalogPage(encoded)
	if err != nil {
		t.Fatal(err)
	}
	entries := decoded.Entries()
	if len(entries) != 2 || entries[0].ExpectedSize() != 99 || entries[1].Name() != "folder" ||
		entries[1].ModifiedTime() != modified || !decoded.Terminal() || decoded.OmittedCount() != 7 {
		t.Fatalf("decoded page changed semantics: %+v", decoded)
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
			if _, err := catalog.DecodeCatalogPage(hostile); err == nil {
				t.Fatal("hostile page was accepted")
			}
		})
	}
}

func TestAddressedSenderServesOnlyTheCommittedPage(t *testing.T) {
	ctx := t.Context()
	share := shareInstance(t, 90)
	directory := directoryID(t, 110)
	entry, err := catalog.NewFileEntry(fileID(t, 112), "expected.bin", 1, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	spec := catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: directory, Generation: generationID(t, 111),
		Entries: []catalog.Entry{entry}, Terminal: true,
	}
	page, payload, err := catalog.NewCatalogPage(spec)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := NewListRequest(directory, nil, 0)
	source := addressedSourceFunc(func(context.Context, ListRequest) (PageResult, error) {
		return CatalogPageResult(payload), nil
	})
	service, _ := NewAddressedSenderService(share, source)
	served, err := service.Serve(ctx, request, nil)
	if err != nil || !bytes.Equal(served, payload.Bytes()) {
		t.Fatalf("committed catalog payload did not round trip: %v", err)
	}
	verified, err := (CatalogPayloadDecoder{}).Verify(ctx, share, request, served)
	if err != nil || verified.Page.Commitment() != page.Commitment() {
		t.Fatalf("served catalog commitment changed: %v", err)
	}
	served[0] ^= 0xff
	servedAgain, err := service.Serve(ctx, request, nil)
	if err != nil || !bytes.Equal(servedAgain, payload.Bytes()) || bytes.Equal(served, servedAgain) {
		t.Fatalf("caller was able to mutate committed catalog bytes: %v", err)
	}
	wrongGeneration := generationID(t, 113)
	wrongRequest, _ := NewListRequest(directory, &wrongGeneration, 0)
	if _, err := service.Serve(ctx, wrongRequest, nil); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("served page from a different generation: %v", err)
	}
}

func TestCommittedFailurePayloadReplaysWithoutRescan(t *testing.T) {
	fixture := newCatalogObjectFixture(t)
	objects := CatalogFailureEncoder{}
	process, _ := catalog.NewBudgetAccount("process", catalog.DefaultProcessBudgetLimits())
	shareBudget, _ := catalog.NewBudgetAccount("share", catalog.DefaultShareBudgetLimits())
	session, _ := catalog.NewBudgetAccount("session", catalog.DefaultSessionBudgetLimits())
	var attempts atomic.Uint32
	store, err := catalog.NewCatalogStore(catalog.StoreConfig{
		ShareInstance: fixture.share, ProcessBudget: process, ShareBudget: shareBudget,
		FailureEncoder: objects,
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
	firstObject, found, err := store.FailurePayload(context.Background(), firstDirectory, firstFailure.AttemptID)
	if err != nil || !found {
		t.Fatalf("first committed failure object: found=%v err=%v", found, err)
	}
	if _, err := store.ListChildren(context.Background(), secondDirectory, session, catalog.ScanOptions{}, scanner); err == nil {
		t.Fatal("second permanent failure was accepted")
	}

	var rescans atomic.Int32
	never := catalog.DirectoryScannerFunc(func(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
		rescans.Add(1)
		return catalog.ScanResult{}, errors.New("committed failure reached scanner")
	})
	source, err := NewCatalogStoreSource(CatalogStoreSourceConfig{
		ShareInstance: fixture.share, Store: store, SessionBudget: session, Scanner: never,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := NewListRequest(firstDirectory, nil, 0)
	result, err := source.LoadPage(context.Background(), request, nil)
	if err != nil || result.failure == nil || result.failure.record.AttemptID != firstFailure.AttemptID ||
		!bytes.Equal(result.failure.payload, firstObject.Bytes()) {
		t.Fatalf("replayed failure result = %+v err=%v", result, err)
	}
	service, _ := NewAddressedSenderService(fixture.share, source)
	served, err := service.Serve(context.Background(), request, nil)
	if err != nil || !bytes.Equal(served, firstObject.Bytes()) {
		t.Fatalf("addressed service changed failure bytes: %v", err)
	}
	verifier := CatalogPayloadDecoder{}
	verified, err := verifier.Verify(context.Background(), fixture.share, request, served)
	if err != nil || verified.Failure == nil || verified.Failure.AttemptID != firstFailure.AttemptID {
		t.Fatalf("verified replayed failure = %+v err=%v", verified, err)
	}
	failureGeneration := generationID(t, 153)
	laterRequest, _ := NewListRequest(firstDirectory, &failureGeneration, 1)
	if _, err := verifier.Verify(context.Background(), fixture.share, laterRequest, served); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("failure object answered a later-page request: %v", err)
	}
	if rescans.Load() != 0 {
		t.Fatalf("replayed permanent failure rescanned: %d", rescans.Load())
	}
	if shareBudget.Snapshot().Used == (catalog.ResourceUsage{}) {
		t.Fatal("committed failure authority was not budgeted")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if process.Snapshot().Used != (catalog.ResourceUsage{}) ||
		shareBudget.Snapshot().Used != (catalog.ResourceUsage{}) {
		t.Fatal("closing store did not release committed failure budget")
	}
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
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

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
	if _, err := wireBytes(mustRawCBOR(t, []byte{1}), 2); !errors.Is(err, ErrWireObject) {
		t.Fatalf("wrong-length bytes = %v", err)
	}
	if _, err := wireText(mustRawCBOR(t, uint64(1))); !errors.Is(err, ErrWireObject) {
		t.Fatalf("integer text = %v", err)
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

	_, page, _ := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: fixture.share, DirectoryID: directory, Generation: generation,
		Entries: []catalog.Entry{entry}, Terminal: true,
	})
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
			return PageResult{failure: &addressedDirectoryFailure{}, page: page}, nil
		},
	))
	if _, err := invalidFailureService.Serve(context.Background(), request, nil); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("mixed page/failure result = %v", err)
	}
	_, otherPage, _ := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: shareInstance(t, 178), DirectoryID: directory, Generation: generation,
		Entries: []catalog.Entry{entry}, Terminal: true,
	})
	wrongShareService, _ := NewAddressedSenderService(fixture.share, addressedSourceFunc(
		func(context.Context, ListRequest) (PageResult, error) {
			return CatalogPageResult(otherPage), nil
		},
	))
	if _, err := wrongShareService.Serve(context.Background(), request, nil); !errors.Is(err, ErrObjectIdentity) {
		t.Fatalf("another share's page result = %v", err)
	}

	verifier := CatalogPayloadDecoder{}
	if _, err := verifier.Verify(cancelled, fixture.share, request, []byte{1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled verification = %v", err)
	}
}

type stepClock struct{ now time.Time }

func (c *stepClock) Now() time.Time { return c.now }

func TestCatalogStoreBudgetFailureEncodesRetryableWireObjectAndRecovers(t *testing.T) {
	fixture := newCatalogObjectFixture(t)
	clock := &stepClock{now: time.Unix(1_700_000_000, 0)}
	process, err := catalog.NewBudgetAccount("wire-process", catalog.DefaultProcessBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	shareBudget, err := catalog.NewBudgetAccount("wire-share", catalog.DefaultShareBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	var attempts, generations atomic.Uint32
	store, err := catalog.NewCatalogStore(catalog.StoreConfig{
		ShareInstance:  fixture.share,
		ProcessBudget:  process,
		ShareBudget:    shareBudget,
		FailureEncoder: CatalogFailureEncoder{},
		Clock:          clock,
		AttemptIDs: catalog.ScanAttemptIDGeneratorFunc(func() (catalog.ScanAttemptID, error) {
			return scanAttemptID(t, byte(attempts.Add(1))), nil
		}),
		Generations: catalog.DirectoryGenerationGeneratorFunc(func() (catalog.DirectoryGeneration, error) {
			return generationID(t, byte(generations.Add(1))), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	generousSession, err := catalog.NewBudgetAccount("wire-generous", catalog.DefaultSessionBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	tightSession, err := catalog.NewBudgetAccount("wire-tight", catalog.BudgetLimits{
		ActiveScans: 4, ScanWork: 100, Entries: 100, MemoryBytes: catalog.MaxCatalogPayloadBytes + (32 << 10),
	})
	if err != nil {
		t.Fatal(err)
	}
	root := directoryID(t, 180)
	dir := directoryID(t, 181)
	commitSelectedDirectories(t, store, fixture.share, root, []catalog.DirectoryID{dir}, generousSession)

	bigRef, err := catalog.NewSourceReference(bytes.Repeat([]byte("x"), 40<<10))
	if err != nil {
		t.Fatal(err)
	}
	scanner := catalog.DirectoryScannerFunc(func(ctx context.Context, request catalog.ScanRequest) (catalog.ScanResult, error) {
		for index := range 3 {
			identity, _ := catalog.NewSourceIdentity([]byte{byte(index + 1)})
			candidate, _ := catalog.NewVersionCandidate([]byte{byte(index + 10)})
			err := request.Children.Add(ctx, catalog.ScannedChild{
				FileID: fileID(t, byte(190+index)), Name: "file-" + string(rune('a'+index)),
				SourceReference: bigRef, SourceIdentity: identity, VersionCandidate: candidate, ExpectedSize: 1,
			})
			if err != nil {
				return catalog.ScanResult{}, err
			}
		}
		return catalog.ScanResult{}, nil
	})

	tightSource, err := NewCatalogStoreSource(CatalogStoreSourceConfig{
		ShareInstance: fixture.share, Store: store, SessionBudget: tightSession, Scanner: scanner,
	})
	if err != nil {
		t.Fatal(err)
	}
	generousSource, err := NewCatalogStoreSource(CatalogStoreSourceConfig{
		ShareInstance: fixture.share, Store: store, SessionBudget: generousSession, Scanner: scanner,
	})
	if err != nil {
		t.Fatal(err)
	}
	tightService, err := NewAddressedSenderService(fixture.share, tightSource)
	if err != nil {
		t.Fatal(err)
	}
	generousService, err := NewAddressedSenderService(fixture.share, generousSource)
	if err != nil {
		t.Fatal(err)
	}

	activeService := tightService
	client, err := NewClient(ClientConfig{
		ShareInstance: fixture.share,
		Transport: PageTransportFunc(func(ctx context.Context, req ListRequest) ([]byte, error) {
			return activeService.Serve(ctx, req, nil)
		}),
		Verifier: CatalogPayloadDecoder{},
		Now:      clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.LoadDirectory(context.Background(), dir)
	var wireFailure DirectoryFailure
	if !errors.As(err, &wireFailure) || wireFailure.Code != DirectoryCodeBudget ||
		!wireFailure.Retryable || wireFailure.RetryAfter != catalog.DefaultScanRetryCooldown {
		t.Fatalf("wire budget failure = %v, want retryable DirectoryCodeBudget", err)
	}

	activeService = generousService
	clock.now = clock.now.Add(wireFailure.RetryAfter)
	snapshot, err := client.LoadDirectory(context.Background(), dir)
	if err != nil || snapshot.EntryCount() != 3 {
		t.Fatalf("wire retry after cooldown = %+v err=%v", snapshot, err)
	}
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
		locator, _ := catalog.NewSourceReference([]byte("object:" + ""))
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
