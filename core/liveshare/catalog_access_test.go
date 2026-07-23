package liveshare

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

func TestPreparedSenderStartsRootPrefetchOnlyAtReadyBoundary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "child.txt"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	sender, err := PrepareSender(context.Background(), SenderConfig{
		Paths: []string{root}, Relays: []string{"ws://127.0.0.1:8484"}, ChunkSize: catalog.MinChunkSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	records := sender.selectedSource.SelectedRoots()
	directory, ok := records[0].DirectoryID()
	if !ok {
		t.Fatal("selected directory root lost its directory identity")
	}
	if _, found, err := sender.catalogStore.Directory(context.Background(), directory); err != nil || found {
		t.Fatalf("ready path scanned a descendant directory: found=%v err=%v", found, err)
	}

	sender.StartRootPrefetch()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, found, loadErr := sender.catalogStore.Directory(context.Background(), directory); loadErr != nil {
			t.Fatal(loadErr)
		} else if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("post-ready root prefetch did not commit the direct-child generation")
		}
		time.Sleep(time.Millisecond)
	}
	sender.StartRootPrefetch()
}

func TestRootPrefetchYieldsToReceiverDemandAndResumes(t *testing.T) {
	listing := &blockingCatalogListing{calls: make(chan catalogListingCall, 4)}
	budget, err := catalog.NewBudgetAccount("test-root-prefetch", catalog.DefaultSessionBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	firstDirectory := catalogAccessDirectory(t, 31)
	secondDirectory := catalogAccessDirectory(t, 32)
	access := &senderCatalogAccess{
		listing: listing,
		scanner: catalog.DirectoryScannerFunc(func(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
			return catalog.ScanResult{}, nil
		}),
		roots:  []catalog.DirectoryID{firstDirectory, secondDirectory},
		budget: budget,
		wake:   make(chan struct{}, 1),
	}
	t.Cleanup(access.Close)

	access.StartRootPrefetch()
	first := awaitCatalogListingCall(t, listing.calls)
	if first.directory != firstDirectory {
		t.Fatalf("first prefetch directory = %x", first.directory)
	}
	releaseDemand := access.beginReceiverDemand()
	awaitCatalogListingFinish(t, first.finished)
	select {
	case call := <-listing.calls:
		t.Fatalf("prefetch ran during receiver demand for %x", call.directory)
	case <-time.After(50 * time.Millisecond):
	}

	releaseDemand()
	retry := awaitCatalogListingCall(t, listing.calls)
	if retry.directory != firstDirectory {
		t.Fatalf("interrupted root was not retried: %x", retry.directory)
	}
	close(retry.complete)
	awaitCatalogListingFinish(t, retry.finished)
	second := awaitCatalogListingCall(t, listing.calls)
	if second.directory != secondDirectory {
		t.Fatalf("second prefetch directory = %x", second.directory)
	}
	close(second.complete)
	awaitCatalogListingFinish(t, second.finished)
}

func TestSenderTerminalCancelsRootPrefetchBeforeConnectivityCleanup(t *testing.T) {
	listing := &blockingCatalogListing{calls: make(chan catalogListingCall, 2)}
	budget, err := catalog.NewBudgetAccount("test-terminal-prefetch", catalog.DefaultSessionBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	access := &senderCatalogAccess{
		listing: listing,
		scanner: catalog.DirectoryScannerFunc(func(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
			return catalog.ScanResult{}, nil
		}),
		roots:  []catalog.DirectoryID{catalogAccessDirectory(t, 41)},
		budget: budget,
		wake:   make(chan struct{}, 1),
	}
	terminal := &testTerminalConnectivity{}
	connectivity := prefetchTerminalConnectivity{prefetch: access, delegate: terminal}
	access.StartRootPrefetch()
	call := awaitCatalogListingCall(t, listing.calls)

	connectivity.StopRecovery()
	awaitCatalogListingFinish(t, call.finished)
	access.Close()
	stops, _ := terminal.snapshot()
	if stops != 1 {
		t.Fatalf("terminal recovery stops = %d", stops)
	}
}

type catalogListingCall struct {
	directory catalog.DirectoryID
	complete  chan struct{}
	finished  chan struct{}
}

type blockingCatalogListing struct {
	calls chan catalogListingCall
}

func (listing *blockingCatalogListing) ListChildren(
	ctx context.Context,
	directory catalog.DirectoryID,
	_ *catalog.BudgetAccount,
	_ catalog.ScanOptions,
	_ catalog.DirectoryScanner,
) (catalog.CommittedDirectory, error) {
	call := catalogListingCall{
		directory: directory,
		complete:  make(chan struct{}),
		finished:  make(chan struct{}),
	}
	listing.calls <- call
	defer close(call.finished)
	select {
	case <-ctx.Done():
		return catalog.CommittedDirectory{}, ctx.Err()
	case <-call.complete:
		return catalog.CommittedDirectory{}, nil
	}
}

func awaitCatalogListingCall(t *testing.T, calls <-chan catalogListingCall) catalogListingCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(3 * time.Second):
		t.Fatal("catalog listing did not start")
		return catalogListingCall{}
	}
}

func awaitCatalogListingFinish(t *testing.T, finished <-chan struct{}) {
	t.Helper()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("catalog listing did not finish")
	}
}

func catalogAccessDirectory(t *testing.T, seed byte) catalog.DirectoryID {
	t.Helper()
	value := make([]byte, catalog.IdentityBytes)
	for index := range value {
		value[index] = seed + byte(index)
	}
	directory, err := catalog.DirectoryIDFromBytes(value)
	if err != nil {
		t.Fatal(err)
	}
	return directory
}
