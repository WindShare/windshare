package liveshare

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	traces := make(chan RootPrefetchTrace, 4)
	sender, err := PrepareSender(context.Background(), SenderConfig{
		Paths: []string{root}, Relays: []string{"ws://127.0.0.1:8484"}, ChunkSize: catalog.MinChunkSize,
		RootPrefetchTracer: RootPrefetchTraceFunc(func(event RootPrefetchTrace) { traces <- event }),
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
	select {
	case event := <-traces:
		t.Fatalf("prefetch traced before the ready boundary: %+v", event)
	default:
	}

	sender.StartRootPrefetch()
	deadline := time.Now().Add(3 * time.Second)
	var committed catalog.CommittedDirectory
	for {
		if current, found, loadErr := sender.catalogStore.Directory(context.Background(), directory); loadErr != nil {
			t.Fatal(loadErr)
		} else if found {
			committed = current
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("post-ready root prefetch did not commit the direct-child generation")
		}
		time.Sleep(time.Millisecond)
	}
	event := awaitRootPrefetchDecision(t, traces, RootPrefetchCommitted)
	if event.ShareInstance != sender.descriptor.ShareInstance() || event.DirectoryID != directory ||
		event.Generation != committed.Generation() || event.Attempt != 1 ||
		event.EntryCount != committed.EntryCount() || event.OmittedCount != committed.OmittedCount() {
		t.Fatalf("committed root prefetch trace = %+v, committed directory = %+v", event, committed)
	}
	sender.StartRootPrefetch()
}

func TestRootPrefetchYieldsToReceiverDemandAndResumes(t *testing.T) {
	listing := &blockingCatalogListing{calls: make(chan catalogListingCall, 4)}
	traces := make(chan RootPrefetchTrace, 8)
	budget, err := catalog.NewBudgetAccount("test-root-prefetch", catalog.DefaultSessionBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	firstDirectory := catalogAccessDirectory(t, 31)
	secondDirectory := catalogAccessDirectory(t, 32)
	access := &senderCatalogAccess{
		share:   catalogAccessShare(t, 30),
		listing: listing,
		scanner: catalog.DirectoryScannerFunc(func(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
			return catalog.ScanResult{}, nil
		}),
		roots:  []catalog.DirectoryID{firstDirectory, secondDirectory},
		budget: budget,
		wake:   make(chan struct{}, 1),
		tracer: RootPrefetchTraceFunc(func(event RootPrefetchTrace) { traces <- event }),
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
	want := []RootPrefetchDecision{
		RootPrefetchAttemptStarted,
		RootPrefetchYieldedToDemand,
		RootPrefetchRetryScheduled,
		RootPrefetchAttemptStarted,
		RootPrefetchCommitted,
		RootPrefetchAttemptStarted,
		RootPrefetchCommitted,
	}
	got := make([]RootPrefetchDecision, 0, len(want))
	for range want {
		got = append(got, awaitRootPrefetchTrace(t, traces).Decision)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("root prefetch decisions = %v, want %v", got, want)
	}
}

func TestRootPrefetchTraceClassifiesBudgetAndScanFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want RootPrefetchDecision
	}{
		{name: "budget", err: catalog.ErrBudgetExceeded, want: RootPrefetchBudgetFailed},
		{name: "scan", err: errors.New("root scan failed"), want: RootPrefetchScanFailed},
		{name: "cyclic scan", err: newRootPrefetchFailureCycle(), want: RootPrefetchScanFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			budget, err := catalog.NewBudgetAccount("test-prefetch-failure", catalog.DefaultSessionBudgetLimits())
			if err != nil {
				t.Fatal(err)
			}
			traces := make(chan RootPrefetchTrace, 2)
			directory := catalogAccessDirectory(t, 51)
			share := catalogAccessShare(t, 52)
			access := &senderCatalogAccess{
				share: share, listing: failingCatalogListing{err: test.err},
				scanner: catalog.DirectoryScannerFunc(func(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
					return catalog.ScanResult{}, nil
				}),
				roots: []catalog.DirectoryID{directory}, budget: budget, wake: make(chan struct{}, 1),
				tracer: RootPrefetchTraceFunc(func(event RootPrefetchTrace) { traces <- event }),
			}
			t.Cleanup(access.Close)
			access.StartRootPrefetch()
			event := awaitRootPrefetchDecision(t, traces, test.want)
			if event.ShareInstance != share || event.DirectoryID != directory || event.Attempt != 1 {
				t.Fatalf("root prefetch failure trace = %+v", event)
			}
		})
	}
}

func TestRootPrefetchTracerPanicCannotInterruptWarmup(t *testing.T) {
	listing := &blockingCatalogListing{calls: make(chan catalogListingCall, 1)}
	budget, err := catalog.NewBudgetAccount("test-prefetch-tracer-panic", catalog.DefaultSessionBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	access := &senderCatalogAccess{
		share: catalogAccessShare(t, 61), listing: listing,
		scanner: catalog.DirectoryScannerFunc(func(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
			return catalog.ScanResult{}, nil
		}),
		roots: []catalog.DirectoryID{catalogAccessDirectory(t, 62)}, budget: budget, wake: make(chan struct{}, 1),
		tracer: RootPrefetchTraceFunc(func(RootPrefetchTrace) { panic("tracer must be observational") }),
	}
	access.StartRootPrefetch()
	call := awaitCatalogListingCall(t, listing.calls)
	close(call.complete)
	awaitCatalogListingFinish(t, call.finished)
	access.Close()
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

type failingCatalogListing struct{ err error }

type rootPrefetchFailureCycle struct{ next error }

func newRootPrefetchFailureCycle() error {
	cycle := &rootPrefetchFailureCycle{}
	cycle.next = cycle
	return cycle
}

func (*rootPrefetchFailureCycle) Error() string { return "cyclic prefetch failure" }
func (failure *rootPrefetchFailureCycle) Unwrap() error {
	return failure.next
}

func (listing failingCatalogListing) ListChildren(
	context.Context,
	catalog.DirectoryID,
	*catalog.BudgetAccount,
	catalog.ScanOptions,
	catalog.DirectoryScanner,
) (catalog.CommittedDirectory, error) {
	return catalog.CommittedDirectory{}, listing.err
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

func awaitRootPrefetchDecision(
	t *testing.T,
	traces <-chan RootPrefetchTrace,
	want RootPrefetchDecision,
) RootPrefetchTrace {
	t.Helper()
	for {
		event := awaitRootPrefetchTrace(t, traces)
		if event.Decision == want {
			return event
		}
	}
}

func awaitRootPrefetchTrace(t *testing.T, traces <-chan RootPrefetchTrace) RootPrefetchTrace {
	t.Helper()
	select {
	case event := <-traces:
		return event
	case <-time.After(3 * time.Second):
		t.Fatal("root prefetch trace was not emitted")
		return RootPrefetchTrace{}
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

func catalogAccessShare(t *testing.T, seed byte) catalog.ShareInstance {
	t.Helper()
	value := make([]byte, catalog.IdentityBytes)
	for index := range value {
		value[index] = seed + byte(index)
	}
	share, err := catalog.ShareInstanceFromBytes(value)
	if err != nil {
		t.Fatal(err)
	}
	return share
}
