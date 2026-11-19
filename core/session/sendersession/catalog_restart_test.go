package sendersession

import (
	"bytes"
	"context"
	"errors"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/catalogflow"
	"sync/atomic"
	"testing"
)

func TestCatalogServiceReplaysCommittedPageAfterEvictionAndRestart(t *testing.T) {
	ctx := context.Background()
	share := id16[catalog.ShareInstance](41)
	rootID := id16[catalog.DirectoryID](42)
	directoryID := id16[catalog.DirectoryID](43)
	rootPath := t.TempDir()

	objects := catalogflow.CatalogFailureEncoder{}
	rawBackend, err := catalog.NewFileCatalogBackend(catalog.FileCatalogBackendConfig{
		Root: rootPath, ShareInstance: share,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := newRestartCatalogStore(t, share, preservingCatalogBackend{CatalogBackend: rawBackend}, objects)
	sessionBudget := newRestartBudget(t, "first-session")
	locator, _ := catalog.NewSourceReference([]byte("object:" + ""))
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
		fileLocator, _ := catalog.NewSourceReference([]byte("object:" + "restart.bin"))
		fileIdentity, _ := catalog.NewSourceIdentity([]byte("restart-file"))
		candidate, _ := catalog.NewVersionCandidate([]byte("restart-revision"))
		err := request.Children.Add(ctx, catalog.ScannedChild{
			FileID: id16[catalog.FileID](48), Name: "restart.bin", SourceReference: fileLocator,
			SourceIdentity: fileIdentity, VersionCandidate: candidate, ExpectedSize: 9,
		})
		return catalog.ScanResult{}, err
	})
	committed, err := store.ListChildren(ctx, directoryID, sessionBudget, catalog.ScanOptions{}, scanner)
	if err != nil {
		t.Fatal(err)
	}
	generation := committed.Generation()
	request, err := catalogflow.NewListRequest(directoryID, &generation, 0)
	if err != nil {
		t.Fatal(err)
	}
	service := newRestartCatalogService(t, share, store, sessionBudget, scanner)
	before, err := service.Serve(ctx, request, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	recoveredObjects := catalogflow.CatalogFailureEncoder{}
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
		t.Fatal("restart changed committed page bytes")
	}
	verifier := catalogflow.CatalogPayloadDecoder{}
	verified, err := verifier.Verify(ctx, share, request, after)
	if err != nil || verified.Failure != nil || verified.Page.Generation() != generation {
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
	sealer catalog.FailureEncoder,
) *catalog.CatalogStore {
	t.Helper()
	process := newRestartBudget(t, "process")
	shareBudget := newRestartBudget(t, "share")
	store, err := catalog.NewCatalogStore(catalog.StoreConfig{
		ShareInstance: share, Backend: backend, ProcessBudget: process, ShareBudget: shareBudget, FailureEncoder: sealer,
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
