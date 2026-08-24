package transfer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/fault"
)

type blockingFirstRangeReader struct {
	started     chan struct{}
	release     chan struct{}
	blockOnce   sync.Once
	releaseOnce sync.Once
}

type cancelFirstDemandReader struct {
	cancel    context.CancelFunc
	requested content.Range
	calls     int
}

func (reader *cancelFirstDemandReader) ReadRange(
	_ context.Context,
	_ content.LeaseID,
	_ content.FileRevisionDescriptor,
	requested content.Range,
	_ RangeSink,
) error {
	reader.calls++
	reader.requested = requested
	reader.cancel()
	return context.Canceled
}

func newBlockingFirstRangeReader() *blockingFirstRangeReader {
	return &blockingFirstRangeReader{started: make(chan struct{}), release: make(chan struct{})}
}

func (reader *blockingFirstRangeReader) ReadRange(
	ctx context.Context,
	_ content.LeaseID,
	_ content.FileRevisionDescriptor,
	requested content.Range,
	sink RangeSink,
) error {
	blocked := false
	reader.blockOnce.Do(func() {
		blocked = true
		close(reader.started)
	})
	if blocked {
		select {
		case <-reader.release:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	return sink.WriteRange(ctx, requested.Offset, make([]byte, requested.Length()))
}

func (reader *blockingFirstRangeReader) Unblock() {
	reader.releaseOnce.Do(func() { close(reader.release) })
}

func TestTransferJobPublishesTerminalDiscoveryBeforeContentDrain(t *testing.T) {
	share := transferID[catalog.ShareInstance](201)
	root := transferID[catalog.DirectoryID](202)
	file := transferID[catalog.FileID](203)
	descriptor := jobDescriptor(t, share, file, 204, 1)
	opened, _ := NewOpenedRevision(transferID[content.LeaseID](205), descriptor)
	blocks := newBlockingFirstRangeReader()
	defer blocks.Unblock()
	rules, _ := NewSelectionRules(true, nil)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1, jobEntry(t, file, "file.bin", 1)),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{
			opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
		},
		Blocks: blocks, Materializer: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	discoveryTrace := make(chan TransferLifecycleTrace, 1)
	job.tracer = TransferLifecycleTraceFunc(func(event TransferLifecycleTrace) {
		if event.Stage == TransferDiscoveryCompleted {
			discoveryTrace <- event
		}
	})
	updates := job.ProgressSnapshots()
	resultCh := make(chan JobResult, 1)
	go func() { resultCh <- job.Run(context.Background()) }()

	select {
	case <-blocks.started:
	case <-time.After(2 * time.Second):
		t.Fatal("content transfer did not start")
	}
	var terminal ReceiveProgressSnapshot
	for terminal.Discovery != DiscoveryComplete {
		select {
		case measure, ok := <-updates:
			if !ok && terminal.Discovery != DiscoveryComplete {
				t.Fatal("selection updates closed without terminal discovery")
			}
			if ok {
				terminal = measure
			}
		case <-time.After(2 * time.Second):
			t.Fatal("terminal discovery remained coupled to content drainage")
		}
	}
	select {
	case trace := <-discoveryTrace:
		if trace.Discovery != DiscoveryComplete || trace.ReceiveOperationID.IsZero() ||
			trace.Progress.DiscoveredFiles != 1 {
			t.Fatalf("discovery trace=%+v", trace)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("discovery completion trace was not emitted before content drainage")
	}
	select {
	case result := <-resultCh:
		t.Fatalf("job settled while the file range was blocked: %+v", result)
	default:
	}
	blocks.Unblock()
	select {
	case result := <-resultCh:
		if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != 1 {
			t.Fatalf("result=%+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job did not settle after content was released")
	}
	var final ReceiveProgressSnapshot
	for measure := range updates {
		final = measure
	}
	if final.PublishedFiles != 1 || final.PublishedBytes != 1 || final.Discovery != DiscoveryComplete {
		t.Fatalf("final progress after worker drainage=%+v", final)
	}
}

func TestTransferJobSettlesSyntheticRootAfterIsolatedChildFinalization(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xb1)
	root := transferID[catalog.DirectoryID](0xb2)
	child := transferID[catalog.DirectoryID](0xb3)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	output := newJobOutput(share)
	metadataFault, err := fault.NewOutput(fault.ScopeDirectoryLocal, fault.OutputDirectoryMetadata)
	if err != nil {
		t.Fatal(err)
	}
	output.directorySettlement = func(admission DirectoryAdmission) (DirectorySettlement, error) {
		if admission.Path() == "child" {
			return NewIsolatedDirectorySettlement(admission, metadataFault)
		}
		return NewFinalizedDirectorySettlement(admission)
	}
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root:  jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, child, "child")),
				child: jobSnapshot(t, share, child, 2),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartial || result.TerminationCause != nil ||
		result.SettlementFailure != nil || output.pauseCalls != 0 || output.completeCalls != 1 {
		t.Fatalf("result=%+v pause=%d complete=%d", result, output.pauseCalls, output.completeCalls)
	}
	if len(result.Directories) != 1 || result.Directories[0].DirectoryID != child ||
		result.Directories[0].Path != "child" || result.Directories[0].Fault != metadataFault {
		t.Fatalf("directory failures=%+v", result.Directories)
	}
	if len(output.directoryAdmissions) != 2 || len(output.finalizedAdmissions) != 2 ||
		output.directoryAdmissions[1] != output.finalizedAdmissions[0] ||
		output.directoryAdmissions[0] != output.finalizedAdmissions[1] ||
		output.finalizedAdmissions[1].Path() != "" {
		t.Fatalf("admitted=%+v finalized=%+v", output.directoryAdmissions, output.finalizedAdmissions)
	}
}

func TestTransferJobPausesWhenDirectorySettlementNamesAnotherAdmission(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xc1)
	root := transferID[catalog.DirectoryID](0xc2)
	child := transferID[catalog.DirectoryID](0xc3)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	output := newJobOutput(share)
	output.directorySettlement = func(admission DirectoryAdmission) (DirectorySettlement, error) {
		if admission.Path() != "child" {
			return NewFinalizedDirectorySettlement(admission)
		}
		output.mu.Lock()
		rootAdmission := output.directoryAdmissions[0]
		output.mu.Unlock()
		return NewFinalizedDirectorySettlement(rootAdmission)
	}
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root:  jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, child, "child")),
				child: jobSnapshot(t, share, child, 2),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePaused ||
		result.TerminationFault != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) ||
		result.SettlementFault != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) || len(result.Directories) != 0 ||
		output.pauseCalls != 1 || output.completeCalls != 0 ||
		!slices.Equal(output.finalized, []string{"child"}) {
		t.Fatalf("result=%+v finalized=%v pause=%d complete=%d", result, output.finalized,
			output.pauseCalls, output.completeCalls)
	}
}

func TestTransferJobRejectsDirectoryAdmissionFromAnotherIntentScope(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xd1)
	root := transferID[catalog.DirectoryID](0xd2)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	foreignRules, err := NewSelectionRules(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	foreignScope, err := NewDirectoryAdmissionScope(
		testReceiveIntent(t, share, root, foreignRules),
	)
	if err != nil {
		t.Fatal(err)
	}
	output := newJobOutput(share)
	output.directoryAdmission = func(
		directory AuthenticatedSourceDirectory,
		_ DirectoryAdmission,
	) (DirectoryAdmission, error) {
		return NewDirectoryAdmissionWithSecret(
			output.directorySecret[:], foreignScope, admissionTestMaterializationDirectory(t, directory),
		)
	}
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePaused ||
		result.TerminationFault != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) ||
		len(output.directoryAdmissions) != 1 || len(output.finalizedAdmissions) != 0 ||
		output.pauseCalls != 1 || output.completeCalls != 0 {
		t.Fatalf("result=%+v admitted=%+v finalized=%+v pause=%d complete=%d", result,
			output.directoryAdmissions, output.finalizedAdmissions, output.pauseCalls, output.completeCalls)
	}
}

func TestTransferJobCatalogLedgerRejectsDuplicateUnselectedNodeBeforeOutputAdmission(t *testing.T) {
	share := transferID[catalog.ShareInstance](0xe1)
	root := transferID[catalog.DirectoryID](0xe2)
	file := transferID[catalog.FileID](0xe3)
	generation := transferID[catalog.DirectoryGeneration](0xe4)
	first, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: root, Generation: generation,
		Entries: []catalog.Entry{jobEntry(t, file, "a.bin", 0)},
	}, jobPageCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: root, Generation: generation,
		PageIndex: 1, Previous: first.Commitment(), Terminal: true,
		Entries: []catalog.Entry{jobEntry(t, file, "b.bin", 0)},
	}, jobPageCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	rules, err := NewSelectionRules(false, nil)
	if err != nil {
		t.Fatal(err)
	}
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
		len(output.directoryAdmissions) != 0 || len(output.finalizedAdmissions) != 0 ||
		output.pauseCalls != 1 || output.completeCalls != 0 {
		t.Fatalf("result=%+v admitted=%+v finalized=%+v pause=%d complete=%d", result,
			output.directoryAdmissions, output.finalizedAdmissions, output.pauseCalls, output.completeCalls)
	}
}

type replayCursorEvent struct {
	openIndex int
}

type observedReplayCatalog struct {
	snapshot catalog.DirectorySnapshot
	events   chan replayCursorEvent
	mu       sync.Mutex
	opens    int
}

func (source *observedReplayCatalog) OpenDirectoryPages(
	_ context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	if directory != source.snapshot.DirectoryID() {
		return nil, catalogIntegrityFailure(ErrCatalogIdentity)
	}
	source.mu.Lock()
	openIndex := source.opens
	source.opens++
	source.mu.Unlock()
	return &observedReplayCursor{
		inner: snapshotPages(source.snapshot), events: source.events, openIndex: openIndex,
	}, nil
}

type observedReplayCursor struct {
	inner     catalog.DirectoryPageCursor
	events    chan<- replayCursorEvent
	openIndex int
}

func (cursor *observedReplayCursor) Next(ctx context.Context) (catalog.CatalogPage, bool, error) {
	cursor.events <- replayCursorEvent{openIndex: cursor.openIndex}
	return cursor.inner.Next(ctx)
}

func (cursor *observedReplayCursor) Close() error { return cursor.inner.Close() }

func multiPageJobSnapshot(
	t *testing.T,
	share catalog.ShareInstance,
	directory catalog.DirectoryID,
	generation byte,
	entries ...catalog.Entry,
) catalog.DirectorySnapshot {
	t.Helper()
	pages := make([]catalog.CatalogPage, 0, len(entries))
	var previous catalog.PageCommitment
	for index, entry := range entries {
		page, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
			ShareInstance: share, DirectoryID: directory,
			Generation: transferID[catalog.DirectoryGeneration](generation),
			PageIndex:  uint32(index), Previous: previous, Entries: []catalog.Entry{entry},
			Terminal: index == len(entries)-1,
		}, jobPageCommitter{})
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, page)
		previous = page.Commitment()
	}
	snapshot, err := catalog.NewDirectorySnapshot(pages)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestTransferJobQueueBackpressuresGenerationReplay(t *testing.T) {
	share := transferID[catalog.ShareInstance](210)
	root := transferID[catalog.DirectoryID](211)
	entries := make([]catalog.Entry, 0, 4)
	revisions := &jobRevisionClient{
		opened: make(map[catalog.FileID]OpenedRevision), failures: make(map[catalog.FileID]error),
	}
	for index := range 4 {
		file := transferID[catalog.FileID](byte(212 + index))
		entries = append(entries, jobEntry(t, file, "file-"+string(rune('a'+index))+".bin", 1))
		descriptor := jobDescriptor(t, share, file, byte(220+index), 1)
		revisions.opened[file], _ = NewOpenedRevision(
			transferID[content.LeaseID](byte(224+index)), descriptor,
		)
	}
	source := &observedReplayCatalog{
		snapshot: multiPageJobSnapshot(t, share, root, 1, entries...),
		events:   make(chan replayCursorEvent, 32),
	}
	blocks := newBlockingFirstRangeReader()
	defer blocks.Unblock()
	rules, _ := NewSelectionRules(true, nil)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		FileQueueCapacity: 1, Catalog: source, Revisions: revisions, Blocks: blocks, Materializer: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	var traceMu sync.Mutex
	enqueuedFiles := 0
	job.tracer = TransferLifecycleTraceFunc(func(event TransferLifecycleTrace) {
		if event.Stage == TransferFileEnqueued {
			traceMu.Lock()
			enqueuedFiles++
			traceMu.Unlock()
		}
	})
	resultCh := make(chan JobResult, 1)
	go func() { resultCh <- job.Run(context.Background()) }()
	select {
	case <-blocks.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first queued file did not reach the worker")
	}
	fileReplayCalls := 0
	for fileReplayCalls < 3 {
		select {
		case event := <-source.events:
			if event.openIndex == 1 {
				fileReplayCalls++
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("file replay calls=%d, want queue saturation at 3", fileReplayCalls)
		}
	}
	select {
	case event := <-source.events:
		if event.openIndex == 1 {
			t.Fatal("generation replay fetched past the queue-full page")
		}
	default:
	}
	traceMu.Lock()
	queuedBeforeRelease := enqueuedFiles
	traceMu.Unlock()
	if queuedBeforeRelease != 2 {
		t.Fatalf("enqueued traces=%d, want only the two successful queue sends", queuedBeforeRelease)
	}
	measure := job.Progress()
	if measure.DiscoveredFiles != 4 || measure.Discovery != DiscoveryOpen {
		t.Fatalf("measure while replay is backpressured=%+v", measure)
	}
	blocks.Unblock()
	select {
	case result := <-resultCh:
		if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != 4 {
			t.Fatalf("result=%+v", result)
		}
		traceMu.Lock()
		queuedAfterRelease := enqueuedFiles
		traceMu.Unlock()
		if queuedAfterRelease != 4 {
			t.Fatalf("completed enqueued traces=%d", queuedAfterRelease)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backpressured job did not resume")
	}
}

func TestTransferJobGenerationReplayBudgetFailsBeforeAdmission(t *testing.T) {
	share := transferID[catalog.ShareInstance](230)
	root := transferID[catalog.DirectoryID](231)
	first := transferID[catalog.FileID](232)
	second := transferID[catalog.FileID](233)
	snapshot := multiPageJobSnapshot(
		t, share, root, 1,
		jobEntry(t, first, "first.bin", 0), jobEntry(t, second, "second.bin", 0),
	)
	output := newJobOutput(share)
	rules, _ := NewSelectionRules(true, nil)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		GenerationReplayPages: 1,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: snapshot},
			failures:  make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePaused ||
		result.TerminationFault != mustSessionFault(fault.ScopeOutputPause, fault.SessionResourceBudget) ||
		result.Progress.DiscoveredFiles != 0 || len(output.directories) != 0 || output.pauseCalls != 1 {
		t.Fatalf("result=%+v directories=%v pause=%d", result, output.directories, output.pauseCalls)
	}
}

func TestTransferJobFailsClosedForUnknownChildCursorFailureWithoutPhantomSelection(t *testing.T) {
	share := transferID[catalog.ShareInstance](240)
	root := transferID[catalog.DirectoryID](241)
	branch := transferID[catalog.DirectoryID](242)
	partial := transferID[catalog.FileID](243)
	first, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: share, DirectoryID: branch,
		Generation: transferID[catalog.DirectoryGeneration](2),
		Entries:    []catalog.Entry{jobEntry(t, partial, "partial.bin", 7)},
	}, jobPageCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	cursorFailure := errors.New("cursor verifier budget failure")
	output := newJobOutput(share)
	rules, _ := NewSelectionRules(true, nil)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: prefixFailureCatalog{
			root: root, branch: branch,
			rootPages: jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, branch, "branch")),
			first:     first, failure: cursorFailure,
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePaused || result.TerminationFault != fault.DependencyContractFault() ||
		result.Progress.DiscoveredFiles != 0 || result.Progress.DiscoveredBytes != 0 ||
		!slices.Equal(output.directories, []string{""}) || len(output.finalized) != 0 {
		t.Fatalf("result=%+v directories=%v finalized=%v", result, output.directories, output.finalized)
	}
}

func TestTransferJobFailureDiagnosticsAreIndependentlyBounded(t *testing.T) {
	run := &jobRun{}
	for index := 0; index <= MaximumRetainedJobFailures; index++ {
		run.recordDirectoryFailure(DirectoryJobFailure{Path: "x"})
	}
	directories, files, omittedDirectories, omittedFiles, _ := run.failureSnapshot()
	if len(directories) != MaximumRetainedJobFailures || len(files) != 0 ||
		omittedDirectories != 1 || omittedFiles != 0 {
		t.Fatalf("count-bounded diagnostics=(%d,%d,%d,%d)", len(directories), len(files), omittedDirectories, omittedFiles)
	}

	run = &jobRun{}
	run.recordFileFailure(FileJobFailure{Path: strings.Repeat("x", int(MaximumRetainedFailurePathBytes))})
	run.recordFileFailure(FileJobFailure{Path: "overflow"})
	directories, files, omittedDirectories, omittedFiles, _ = run.failureSnapshot()
	if len(directories) != 0 || len(files) != 1 || omittedDirectories != 0 || omittedFiles != 1 {
		t.Fatalf("byte-bounded diagnostics=(%d,%d,%d,%d)", len(directories), len(files), omittedDirectories, omittedFiles)
	}
}

func TestTransferJobSelectionResolutionSurvivesSaturatedDiagnostics(t *testing.T) {
	share := transferID[catalog.ShareInstance](250)
	root := transferID[catalog.DirectoryID](251)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := newJobRun(job)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= MaximumRetainedJobFailures; index++ {
		run.recordDirectoryFailure(DirectoryJobFailure{Path: "x"})
	}
	drift := sourceInvalidatedFailure(content.ErrRevisionDrift)
	run.recordFileFailure(FileJobFailure{Path: "omitted-drift.bin", Cause: drift})
	want := errors.Join(ErrSelectionTargetMissing, errors.New("path missing.bin"))
	run.selectionResolutionFailure = want
	result := run.finish(context.Background())
	if result.Outcome != DirectTreeOutcomePartial ||
		result.SelectionResolutionFailure != want ||
		result.SourceDriftFailure != drift ||
		result.SourceDriftFault != mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionInvalidated) ||
		len(result.Directories) != MaximumRetainedJobFailures ||
		result.OmittedDirectoryFailures != 1 || result.OmittedFileFailures != 1 {
		t.Fatalf("result=%+v", result)
	}
}

func TestTransferLifecycleTracerPanicCannotChangeTransferAuthority(t *testing.T) {
	share := transferID[catalog.ShareInstance](183)
	output := newJobOutput(share)
	job, _ := branchJob(t, output, &jobRevisionClient{}, scriptedRangeReader{})
	job.tracer = TransferLifecycleTraceFunc(func(TransferLifecycleTrace) {
		panic("diagnostic observer failed")
	})

	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != 1 ||
		output.pauseCalls != 0 || output.completeCalls != 1 {
		t.Fatalf("result=%+v pauses=%d completes=%d", result, output.pauseCalls, output.completeCalls)
	}
}

func TestHugeFileRangePlanningDemandsOneBoundedWindowBeforeCancellation(t *testing.T) {
	share := transferID[catalog.ShareInstance](184)
	root := transferID[catalog.DirectoryID](185)
	file := transferID[catalog.FileID](186)
	rules, _ := NewSelectionRules(true, nil)
	descriptor := jobDescriptor(t, share, file, 1, catalog.MaxFileSize)
	opened, _ := NewOpenedRevision(transferID[content.LeaseID](187), descriptor)
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelFirstDemandReader{cancel: cancel}
	output := newJobOutput(share)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1, jobEntry(t, file, "huge.bin", catalog.MaxFileSize)),
			}, failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{
			opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
		},
		Blocks: reader, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(ctx)
	want := content.Range{
		Offset: 0,
		End:    uint64(defaultFileReadWindowBlocks * catalog.MinChunkSize),
	}
	if result.Outcome != DirectTreeOutcomePaused || reader.calls != 1 || reader.requested != want ||
		output.pauseCalls != 1 {
		t.Fatalf("outcome=%d calls=%d requested=%+v pauses=%d", result.Outcome,
			reader.calls, reader.requested, output.pauseCalls)
	}
}
