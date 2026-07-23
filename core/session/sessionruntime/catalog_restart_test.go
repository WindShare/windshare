package sessionruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/catalogflow"
)

func TestCatalogServiceReplaysExactOpaquePageAfterEvictionAndRestart(t *testing.T) {
	ctx := context.Background()
	share := id16[catalog.ShareInstance](41)
	rootID := id16[catalog.DirectoryID](42)
	directoryID := id16[catalog.DirectoryID](43)
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{44}, ed25519.SeedSize))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	catalogKey := bytes.Repeat([]byte{45}, 32)
	rootPath := t.TempDir()

	objects, err := catalogflow.NewSealedCatalogStore(catalogflow.SealedCatalogStoreConfig{
		ShareInstance: share, CatalogKey: catalogKey, SenderPrivateKey: privateKey,
		NonceSource: &deterministicReader{next: 46}, MaxPageCacheBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rawBackend, err := catalog.NewFileCatalogBackend(catalog.FileCatalogBackendConfig{
		Root: rootPath, ShareInstance: share,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := newRestartCatalogStore(t, share, preservingCatalogBackend{CatalogBackend: rawBackend}, objects)
	sessionBudget := newRestartBudget(t, "first-session")
	locator, _ := catalog.NewLocator(0, "")
	identity, _ := catalog.NewSourceIdentity([]byte("restart-directory"))
	selected, err := catalog.NewDirectoryNodeRecord(
		directoryID, rootID, "folder", locator, identity, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	rootCommit, err := catalog.NewSyntheticRootCommit(catalog.SyntheticRootCommitSpec{
		ShareInstance: share, SyntheticRoot: rootID,
		Generation: id16[catalog.DirectoryGeneration](47), SelectedRoots: []catalog.NodeRecord{selected},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitSyntheticRoot(ctx, rootCommit, sessionBudget); err != nil {
		t.Fatal(err)
	}
	scanner := catalog.DirectoryScannerFunc(func(ctx context.Context, request catalog.ScanRequest) (catalog.ScanResult, error) {
		fileLocator, _ := catalog.NewLocator(0, "restart.bin")
		fileIdentity, _ := catalog.NewSourceIdentity([]byte("restart-file"))
		candidate, _ := catalog.NewVersionCandidate([]byte("restart-revision"))
		err := request.Children.Add(ctx, catalog.ScannedChild{
			FileID: id16[catalog.FileID](48), Name: "restart.bin", Locator: fileLocator,
			SourceIdentity: fileIdentity, VersionCandidate: candidate, ExpectedSize: 9,
		})
		return catalog.ScanResult{}, err
	})
	committed, err := store.ListChildren(ctx, directoryID, sessionBudget, catalog.ScanOptions{}, scanner)
	if err != nil {
		t.Fatal(err)
	}
	request, err := catalogflow.NewListRequest(directoryID, new(committed.Generation()), 0)
	if err != nil {
		t.Fatal(err)
	}
	service := newRestartCatalogService(t, share, store, sessionBudget, scanner)
	before, err := service.Serve(ctx, request, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, found, err := store.Page(ctx, directoryID, committed.Generation(), 0)
	if err != nil || !found {
		t.Fatalf("committed page = %v, %v", found, err)
	}
	if _, err := objects.LoadSealedPage(ctx, page); !errors.Is(err, catalogflow.ErrCatalogObjectMissing) {
		t.Fatalf("one-byte cache unexpectedly retained page: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	recoveredNonces := &rejectNonceReader{}
	recoveredObjects, err := catalogflow.NewSealedCatalogStore(catalogflow.SealedCatalogStoreConfig{
		ShareInstance: share, CatalogKey: catalogKey, SenderPrivateKey: privateKey,
		NonceSource: recoveredNonces, MaxPageCacheBytes: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	recoveredBackend, err := catalog.NewFileCatalogBackend(catalog.FileCatalogBackendConfig{
		Root: rootPath, ShareInstance: share,
	})
	if err != nil {
		t.Fatal(err)
	}
	recovered := newRestartCatalogStore(t, share, preservingCatalogBackend{CatalogBackend: recoveredBackend}, recoveredObjects)
	recoveredBudget := newRestartBudget(t, "recovered-session")
	unexpectedScan := catalog.DirectoryScannerFunc(func(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
		return catalog.ScanResult{}, errors.New("durable replay attempted a new scan")
	})
	recoveredService := newRestartCatalogService(
		t, share, recovered, recoveredBudget, unexpectedScan,
	)
	after, err := recoveredService.Serve(ctx, request, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("restart changed random-nonce sender-object bytes")
	}
	if recoveredNonces.calls.Load() != 0 {
		t.Fatalf("durable replay resealed page %d times", recoveredNonces.calls.Load())
	}
	verifier, err := catalogflow.NewCatalogObjectVerifier(catalogflow.CatalogObjectVerifierConfig{
		ShareInstance: share, CatalogKey: catalogKey, SenderPublicKey: publicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := verifier.Verify(ctx, share, request, after)
	if err != nil || verified.Failure != nil || verified.Page.Commitment() != page.Commitment() {
		t.Fatalf("replayed object verification = %+v, %v", verified, err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recoveredBackend.Destroy(); err != nil {
		t.Fatal(err)
	}
}

type preservingCatalogBackend struct{ catalog.CatalogBackend }

type rejectNonceReader struct{ calls atomic.Int32 }

func (reader *rejectNonceReader) Read([]byte) (int, error) {
	reader.calls.Add(1)
	return 0, errors.New("replay must not request a nonce")
}

func newRestartCatalogStore(
	t *testing.T,
	share catalog.ShareInstance,
	backend catalog.CatalogBackend,
	sealer catalog.PageSealer,
) *catalog.CatalogStore {
	t.Helper()
	process := newRestartBudget(t, "process")
	shareBudget := newRestartBudget(t, "share")
	store, err := catalog.NewCatalogStore(catalog.StoreConfig{
		ShareInstance: share, Backend: backend, ProcessBudget: process, ShareBudget: shareBudget, PageSealer: sealer,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newRestartBudget(t *testing.T, name string) *catalog.BudgetAccount {
	t.Helper()
	budget, err := catalog.NewBudgetAccount(name, catalog.DefaultProcessBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	return budget
}

func newRestartCatalogService(
	t *testing.T,
	share catalog.ShareInstance,
	store *catalog.CatalogStore,
	budget *catalog.BudgetAccount,
	scanner catalog.DirectoryScanner,
) *catalogflow.AddressedSenderService {
	t.Helper()
	source, err := catalogflow.NewCatalogStoreSource(catalogflow.CatalogStoreSourceConfig{
		ShareInstance: share, Store: store, SessionBudget: budget, Scanner: scanner,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := catalogflow.NewAddressedSenderService(share, source)
	if err != nil {
		t.Fatal(err)
	}
	return service
}
