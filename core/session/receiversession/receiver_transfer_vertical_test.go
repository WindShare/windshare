package receiversession

import (
	"bytes"
	"context"
	"errors"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/internal/testoutputroot"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCompositeRuntimeTransferJobPublishesDurableFilesystemOutput(t *testing.T) {
	fixture := newVerticalFixture(t)
	close(fixture.scanGate)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	outputFixture := testoutputroot.New(t)
	outputRoot := outputFixture.RootPath
	authority, err := osfs.NewOutputAuthority(osfs.OutputConfig{
		RootPath: outputRoot, CreateRoot: outputFixture.CreateRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := authority.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, statErr := os.Stat(outputRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("output authority created its root before terminal selection: %v", statErr)
	}
	selection, err := transfer.NewSelectionSpec(
		receiver.Descriptor().ShareInstance(), receiver.Descriptor().SyntheticRoot(),
		rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.SelectOperation(context.Background(), selection, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != osfs.OutputLookupCreateReady {
		t.Fatalf("native lookup = (%d, %v)", lookup.Kind(), err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	operation, err := lookup.Create(context.Background(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("native operation omitted receive intent")
	}
	jobID, err := transfer.NewTransferJobID()
	if err != nil {
		t.Fatal(err)
	}
	job, err := receiver.NewTransferJob(intent, jobID, operation, diagnostics.Sink{})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != transfer.DirectTreeOutcomeSuccess ||
		result.Settlement.Kind() != transfer.DirectTreeSettlementSuccess ||
		result.SucceededFiles != 1 || result.TerminationCause != nil ||
		result.TransferJobID != jobID || result.ReceiveIntentDigest != intent.Digest() || result.ReceiveIntent.IsZero() {
		t.Fatalf("transfer result = %+v", result)
	}
	destination, _ := intent.MaterializationPlan().DestinationReservation()
	written, err := os.ReadFile(filepath.Join(outputRoot, destination.PhysicalName(), "folder", "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, fixture.fileData) {
		t.Fatal("durably published file changed bytes")
	}
	if fixture.contentStore.blockReads.Load() != 3 {
		t.Fatalf("durable job block reads = %d", fixture.contentStore.blockReads.Load())
	}
}

type countingCatalogVerifier struct {
	inner catalogflow.ObjectVerifier
	mu    sync.Mutex
	calls map[catalog.DirectoryID]int
}

func (v *countingCatalogVerifier) Verify(
	ctx context.Context,
	share catalog.ShareInstance,
	req catalogflow.ListRequest,
	object []byte,
) (catalogflow.VerifiedObject, error) {
	v.mu.Lock()
	v.calls[req.DirectoryID()]++
	v.mu.Unlock()
	return v.inner.Verify(ctx, share, req, object)
}

func (v *countingCatalogVerifier) count(dir catalog.DirectoryID) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls[dir]
}

func TestResolveShapeAndTransferJobReuseRootCatalogPageOnce(t *testing.T) {
	fixture := newVerticalFixture(t)
	close(fixture.scanGate)
	verifier := &countingCatalogVerifier{
		inner: fixture.receiverConfig.CatalogVerifier,
		calls: make(map[catalog.DirectoryID]int),
	}
	cfg := fixture.receiverConfig
	cfg.CatalogVerifier = verifier
	receiverFactory, err := NewFactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	trackReceiverFactory(receiverFactory)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, receiverFactory)
	defer sender.Close()
	defer receiver.Close()

	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(
		receiver.Descriptor().ShareInstance(), receiver.Descriptor().SyntheticRoot(), rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := receiver.ResolveOrdinaryOutputShape(
		context.Background(), selection, ordinaryoutput.DefaultShapeProbeBudgetV1,
	)
	if err != nil || decision.Kind() != ordinaryoutput.ShapeCompleteDirectory {
		t.Fatalf("ResolveOrdinaryOutputShape = (%+v, %v)", decision, err)
	}
	if got := verifier.count(fixture.syntheticRoot); got != 1 {
		t.Fatalf("root catalog RPCs after shape resolution = %d, want 1", got)
	}

	artifact, err := transfer.MaterializeOrdinaryOutputShape(decision)
	if err != nil {
		t.Fatal(err)
	}
	outputFixture := testoutputroot.New(t)
	authority, err := osfs.NewOutputAuthority(osfs.OutputConfig{
		RootPath: outputFixture.RootPath, CreateRoot: outputFixture.CreateRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	if err := authority.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.SelectOperation(context.Background(), selection, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != osfs.OutputLookupCreateReady {
		t.Fatalf("lookup = (%d, %v)", lookup.Kind(), err)
	}
	operation, err := lookup.Create(context.Background(), artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("missing receive intent")
	}
	jobID, err := transfer.NewTransferJobID()
	if err != nil {
		t.Fatal(err)
	}
	job, err := receiver.NewTransferJob(intent, jobID, operation, diagnostics.Sink{})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != transfer.DirectTreeOutcomeSuccess || result.SucceededFiles != 1 {
		t.Fatalf("transfer result = %+v", result)
	}
	if rootRPCs, folderRPCs := verifier.count(fixture.syntheticRoot), verifier.count(fixture.directoryID); rootRPCs != 1 || folderRPCs != 1 {
		t.Fatalf("catalog RPCs after shape+transfer: root=%d folder=%d, want 1 and 1", rootRPCs, folderRPCs)
	}

	// Prefetched root entry is consume-once: a subsequent cursor fetches live.
	deps, err := receiver.TransferDependencies()
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := deps.OpenDirectoryPages(context.Background(), fixture.syntheticRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cursor.Close() }()
	if _, ok, err := cursor.Next(context.Background()); err != nil || !ok {
		t.Fatalf("subsequent live fetch = (%v, %v)", ok, err)
	}
	if got := verifier.count(fixture.syntheticRoot); got != 2 {
		t.Fatalf("root catalog RPCs after subsequent live open = %d, want 2", got)
	}

	// Closing the runtime clears any prefetched cache and fails closed.
	if _, err := receiver.ResolveOrdinaryOutputShape(
		context.Background(), selection, ordinaryoutput.DefaultShapeProbeBudgetV1,
	); err != nil {
		t.Fatal(err)
	}
	prefetchedCursor, err := deps.OpenDirectoryPages(context.Background(), fixture.syntheticRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := prefetchedCursor.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := prefetchedCursor.Next(context.Background()); err == nil {
		t.Fatal("closed prefetched cursor returned nil error")
	}
	// Invalid/non-terminal pages and closed runtimes must never populate cache.
	receiver.RecordPrefetchedDirectory(catalog.DirectoryID{}, nil)
	receiver.BeginClose()
	if _, ok := receiver.takePrefetchedDirectory(fixture.syntheticRoot); ok {
		t.Fatal("BeginClose retained prefetched directory pages")
	}
}
