package transfer

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/fault"
)

// duplicatePageCatalog deliberately places the same NodeID on two otherwise
// valid pages. Catalog validates sibling uniqueness within one page; the job's
// ledger must extend that invariant across the whole generation.
type duplicatePageCatalog struct {
	root  catalog.DirectoryID
	pages []catalog.CatalogPage
}

func (source duplicatePageCatalog) OpenDirectoryPages(
	_ context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	if directory != source.root {
		return nil, catalogIntegrityFailure(ErrCatalogIdentity)
	}
	return &duplicatePageCursor{pages: source.pages}, nil
}

type duplicatePageCursor struct {
	pages []catalog.CatalogPage
	index int
}

type delayedDirectoryCatalog struct {
	source    failingCatalog
	directory catalog.DirectoryID
	started   chan struct{}
	release   chan struct{}
	once      sync.Once
}

type fileScriptedRangeReader struct {
	failFile catalog.FileID
	failAt   int
	failure  error
	calls    map[catalog.FileID]int
}

func (reader *fileScriptedRangeReader) ReadRange(
	ctx context.Context,
	_ content.LeaseID,
	descriptor content.FileRevisionDescriptor,
	requested content.Range,
	sink RangeSink,
) error {
	file := descriptor.FileID()
	reader.calls[file]++
	if file == reader.failFile && reader.calls[file] == reader.failAt {
		return reader.failure
	}
	return sink.WriteRange(ctx, requested.Offset, make([]byte, requested.Length()))
}

func (source *delayedDirectoryCatalog) OpenDirectoryPages(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	if directory == source.directory {
		source.once.Do(func() { close(source.started) })
		select {
		case <-source.release:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	return source.source.OpenDirectoryPages(ctx, directory)
}

func (cursor *duplicatePageCursor) Next(ctx context.Context) (catalog.CatalogPage, bool, error) {
	if err := ctx.Err(); err != nil {
		return catalog.CatalogPage{}, false, err
	}
	if cursor.index >= len(cursor.pages) {
		return catalog.CatalogPage{}, false, nil
	}
	page := cursor.pages[cursor.index]
	cursor.index++
	return page, true, nil
}

func (*duplicatePageCursor) Close() error { return nil }

func TestNodeLedgerRollbackReleasesOnlyDiscardedSuffix(t *testing.T) {
	root := transferID[catalog.DirectoryID](197)
	run, err := newJobRun(&TransferJob{root: root})
	if err != nil {
		t.Fatal(err)
	}
	committed := transferID[catalog.NodeID](198)
	discarded := transferID[catalog.NodeID](199)
	if err := run.claimNode(committed); err != nil {
		t.Fatal(err)
	}
	checkpoint := run.checkpointClaims()
	if err := run.claimNode(discarded); err != nil {
		t.Fatal(err)
	}
	if err := run.rollbackClaims(checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := run.claimNode(discarded); err != nil {
		t.Fatalf("discarded suffix remained claimed: %v", err)
	}
	if err := run.claimNode(committed); normalizedFault(err) != mustCatalogFault(fault.ScopeSessionTerminal, fault.CatalogInvalidGeneration) {
		t.Fatalf("committed prefix duplicate = %v", err)
	}
	if err := run.rollbackClaims(nodeLedgerCheckpoint(len(run.nodeLedger.order) + 1)); !errors.Is(err, ErrNodeLedgerState) {
		t.Fatalf("invalid checkpoint = %v", err)
	}
}

func TestNodeIdentityLedgerFailsClosedAtConfiguredBound(t *testing.T) {
	if _, err := newNodeIdentityLedger(0); !errors.Is(err, ErrNodeLedgerState) {
		t.Fatalf("zero ledger limit = %v", err)
	}
	ledger, err := newNodeIdentityLedger(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.claim(transferID[catalog.NodeID](194)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.claim(transferID[catalog.NodeID](195)); err != nil {
		t.Fatal(err)
	}
	err = ledger.claim(transferID[catalog.NodeID](196))
	if normalizedFault(err) != normalizedFault(resourceBudgetFailure(ErrNodeLedgerBudget)) || !isJobTerminalError(err) {
		t.Fatalf("exhausted ledger = %v", err)
	}
}

func TestTransferJobRejectsCrossPageDuplicateNodeIDBeforeAdmission(t *testing.T) {
	share := transferID[catalog.ShareInstance](201)
	root := transferID[catalog.DirectoryID](202)
	file := transferID[catalog.FileID](203)
	firstEntry := jobEntry(t, file, "a.bin", 0)
	secondEntry := jobEntry(t, file, "b.bin", 0)
	first, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: root,
		Generation: transferID[catalog.DirectoryGeneration](1), Entries: []catalog.Entry{firstEntry},
	}, jobPageCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: root,
		Generation: transferID[catalog.DirectoryGeneration](1), PageIndex: 1,
		Previous: first.Commitment(), Entries: []catalog.Entry{secondEntry}, Terminal: true,
	}, jobPageCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	rules, _ := NewSelectionRules(true, nil)
	descriptor := jobDescriptor(t, share, file, 1, 0)
	opened, _ := NewOpenedRevision(transferID[content.LeaseID](204), descriptor)
	output := newJobOutput(share)
	revisions := &jobRevisionClient{
		opened:   map[catalog.FileID]OpenedRevision{file: opened},
		failures: make(map[catalog.FileID]error),
	}
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog:   duplicatePageCatalog{root: root, pages: []catalog.CatalogPage{first, second}},
		Revisions: revisions, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePaused ||
		result.TerminationFault != mustCatalogFault(fault.ScopeSessionTerminal, fault.CatalogInvalidGeneration) ||
		len(revisions.order) != 0 || len(output.directories) != 0 || output.pauseCalls != 1 || output.completeCalls != 0 {
		t.Fatalf("duplicate node result=%+v opens=%v directories=%v", result, revisions.order, output.directories)
	}
}

func TestTransferJobRejectsCrossPageNameSequenceViolationsBeforeAdmission(t *testing.T) {
	for name, test := range map[string]struct {
		firstName  string
		secondName string
	}{
		"exact duplicate":    {firstName: "same", secondName: "same"},
		"portable collision": {firstName: "Alpha", secondName: "alpha"},
		"boundary order":     {firstName: "zeta", secondName: "alpha"},
	} {
		t.Run(name, func(t *testing.T) {
			share := transferID[catalog.ShareInstance](211)
			root := transferID[catalog.DirectoryID](212)
			generation := transferID[catalog.DirectoryGeneration](2)
			first, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
				ShareInstance: share, DirectoryID: root, Generation: generation,
				Entries: []catalog.Entry{jobEntry(t, transferID[catalog.FileID](213), test.firstName, 0)},
			}, jobPageCommitter{})
			if err != nil {
				t.Fatal(err)
			}
			second, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
				ShareInstance: share, DirectoryID: root, Generation: generation, PageIndex: 1,
				Previous: first.Commitment(), Terminal: true,
				Entries: []catalog.Entry{jobEntry(t, transferID[catalog.FileID](214), test.secondName, 0)},
			}, jobPageCommitter{})
			if err != nil {
				t.Fatal(err)
			}
			rules, _ := NewSelectionRules(true, nil)
			output := newJobOutput(share)
			job, err := newTestTransferJob(t, testTransferJobConfig{
				ShareInstance: share, SyntheticRoot: root, Rules: rules,
				Catalog:   duplicatePageCatalog{root: root, pages: []catalog.CatalogPage{first, second}},
				Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
			})
			if err != nil {
				t.Fatal(err)
			}
			result := job.Run(context.Background())
			if result.Outcome != DirectTreeOutcomePaused ||
				result.TerminationFault != mustCatalogFault(fault.ScopeSessionTerminal, fault.CatalogInvalidGeneration) ||
				len(output.directories) != 0 {
				t.Fatalf("cross-page validation result=%+v directories=%v", result, output.directories)
			}
		})
	}
}

func TestTransferJobDoesNotAdmitOrQueueOmittedChildGeneration(t *testing.T) {
	share := transferID[catalog.ShareInstance](205)
	root := transferID[catalog.DirectoryID](206)
	child := transferID[catalog.DirectoryID](207)
	file := transferID[catalog.FileID](208)
	rules, _ := NewSelectionRules(true, nil)
	descriptor := jobDescriptor(t, share, file, 1, 0)
	opened, _ := NewOpenedRevision(transferID[content.LeaseID](209), descriptor)
	output := newJobOutput(share)
	revisions := &jobRevisionClient{
		opened:   map[catalog.FileID]OpenedRevision{file: opened},
		failures: make(map[catalog.FileID]error),
	}
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root:  jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, child, "child")),
				child: jobSnapshotWithOmissions(t, share, child, 2, 1, jobEntry(t, file, "partial.bin", 0)),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: revisions, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartial || result.TerminationCause != nil || len(result.Directories) != 1 ||
		!errors.Is(result.Directories[0].Cause, ErrCatalogEntriesOmitted) || len(result.Files) != 0 ||
		len(revisions.order) != 0 || len(output.directories) != 1 || output.directories[0] != "" ||
		len(output.finalized) != 1 || output.finalized[0] != "" {
		t.Fatalf("omitted child result=%+v opens=%v directories=%v finalized=%v", result, revisions.order, output.directories, output.finalized)
	}
}

func TestTransferJobCombinesBeginAndTerminalReleaseFailures(t *testing.T) {
	share := transferID[catalog.ShareInstance](210)
	output := newJobOutput(share)
	output.beginErr = outputFailure(fault.ScopeFileLocal, fault.OutputStateIO, errors.New("file create"))
	releaseCause := sessionProtocolFailure(errors.New("revision session closed"))
	revisions := &jobRevisionClient{releaseErr: releaseCause}
	job, _ := branchJob(t, output, revisions, scriptedRangeReader{})

	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePaused ||
		result.TerminationFault != mustSessionFault(fault.ScopeSessionTerminal, fault.SessionProtocol) ||
		len(result.Files) != 1 || result.Files[0].Fault != mustOutputFault(fault.ScopeFileLocal, fault.OutputStateIO) ||
		result.Files[0].LeaseReleaseFault != mustSessionFault(fault.ScopeSessionTerminal, fault.SessionProtocol) ||
		output.pauseCalls != 1 || output.completeCalls != 0 {
		t.Fatalf("begin/release result=%+v", result)
	}
}

func TestOpaqueSelectionBeginsBeforeDelayedUnrelatedBranchAndKeepsItVirtual(t *testing.T) {
	share := transferID[catalog.ShareInstance](220)
	root := transferID[catalog.DirectoryID](221)
	wanted := transferID[catalog.DirectoryID](222)
	unrelated := transferID[catalog.DirectoryID](223)
	emptyChild := transferID[catalog.DirectoryID](224)
	file := transferID[catalog.FileID](225)
	size := uint64(catalog.MinChunkSize)
	rules, err := NewSelectionRules(false, []SelectionOverride{{FileID: file, Selected: true}})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := jobDescriptor(t, share, file, 1, size)
	opened, err := NewOpenedRevision(transferID[content.LeaseID](226), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	releaseUnrelated := make(chan struct{})
	unrelatedStarted := make(chan struct{})
	catalogSource := &delayedDirectoryCatalog{
		source: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1,
					jobDirectoryEntry(t, wanted, "a-wanted"),
					jobDirectoryEntry(t, unrelated, "z-unrelated"),
				),
				wanted:     jobSnapshot(t, share, wanted, 2, jobEntry(t, file, "file.bin", size)),
				unrelated:  jobSnapshot(t, share, unrelated, 3, jobDirectoryEntry(t, emptyChild, "empty")),
				emptyChild: jobSnapshot(t, share, emptyChild, 4),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		directory: unrelated, started: unrelatedStarted, release: releaseUnrelated,
	}
	begin := make(chan struct{})
	var beginOnce sync.Once
	output := newJobOutput(share)
	output.beginHook = func(MaterializationFile) { beginOnce.Do(func() { close(begin) }) }
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: catalogSource,
		Revisions: &jobRevisionClient{
			opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
		},
		Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	resultChannel := make(chan JobResult, 1)
	go func() { resultChannel <- job.Run(context.Background()) }()
	select {
	case <-begin:
	case <-unrelatedStarted:
		close(releaseUnrelated)
		<-resultChannel
		t.Fatal("opaque selection waited for an unrelated branch before beginning output")
	case <-time.After(3 * time.Second):
		close(releaseUnrelated)
		t.Fatal("selected output did not begin")
	}
	close(releaseUnrelated)
	result := <-resultChannel
	if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != 1 ||
		!slices.Equal(output.directories, []string{"", "a-wanted"}) ||
		!slices.Equal(output.finalized, []string{"a-wanted", ""}) {
		t.Fatalf("result=%+v directories=%v finalized=%v", result, output.directories, output.finalized)
	}
	select {
	case <-unrelatedStarted:
		t.Fatal("unrelated non-leaf branch was probed after every opaque target was authenticated")
	default:
	}
}

func TestNonDurableStreamPublishesWithTransientCoverageAndEmptyCheckpoints(t *testing.T) {
	share := transferID[catalog.ShareInstance](230)
	root := transferID[catalog.DirectoryID](234)
	file := transferID[catalog.FileID](238)
	size := uint64(catalog.MinChunkSize) + 1
	rules, _ := NewSelectionRules(true, nil)
	output := newJobOutput(share)
	validated, err := NewDirectTreeCapabilities(DirectTreeCapabilities{Durability: DurabilityNone})
	if err != nil {
		t.Fatal(err)
	}
	output.capabilitiesOverride = &validated
	descriptor := jobDescriptor(t, share, file, 1, size)
	opened, _ := NewOpenedRevision(transferID[content.LeaseID](242), descriptor)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1, jobEntry(t, file, "stream.bin", size)),
			}, failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{
			opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
		},
		Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	transaction := output.transactions["stream.bin"]
	if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != 1 || transaction == nil ||
		!transaction.durable.IsEmpty() || !RangesCoverFile(size, transaction.transient) ||
		!transaction.committed || result.Progress.VerifiedBytes != size ||
		result.Progress.NewlyVerifiedBytes != size || result.Progress.PublishedBytes != size ||
		!result.Progress.CountersExact {
		t.Fatalf("result=%+v transaction=%+v", result, transaction)
	}
}
