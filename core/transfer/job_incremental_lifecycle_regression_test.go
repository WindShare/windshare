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
	job, err := newTestTransferJob(t, TransferJobConfig{
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
		Blocks: blocks, Output: newJobOutput(share),
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
	updates := job.SelectionMeasures()
	resultCh := make(chan JobResult, 1)
	go func() { resultCh <- job.Run(context.Background()) }()

	select {
	case <-blocks.started:
	case <-time.After(2 * time.Second):
		t.Fatal("content transfer did not start")
	}
	var terminal SelectionMeasure
	for !terminal.DiscoveryTerminalSuccess {
		select {
		case measure, ok := <-updates:
			if !ok && !terminal.DiscoveryTerminalSuccess {
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
		if trace.Discovery != DiscoveryComplete || trace.DirectoryID != root || trace.DirectoryGeneration.IsZero() {
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
		if result.Outcome != JobSucceeded || result.SucceededFiles != 1 {
			t.Fatalf("result=%+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job did not settle after content was released")
	}
	var final SelectionMeasure
	for measure := range updates {
		final = measure
	}
	if final.CompletedFiles != 1 || final.CompletedBytes != 1 || !final.DiscoveryTerminalSuccess {
		t.Fatalf("final progress after worker drainage=%+v", final)
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
		return nil, NewSessionFailure(ErrCatalogIdentity)
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
	job, err := newTestTransferJob(t, TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		FileQueueCapacity: 1, Catalog: source, Revisions: revisions, Blocks: blocks, Output: newJobOutput(share),
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
	measure := job.Measure()
	if measure.DiscoveredFiles != 4 || measure.Discovery != DiscoveryOpen {
		t.Fatalf("measure while replay is backpressured=%+v", measure)
	}
	blocks.Unblock()
	select {
	case result := <-resultCh:
		if result.Outcome != JobSucceeded || result.SucceededFiles != 4 {
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
	job, err := newTestTransferJob(t, TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		GenerationReplayPages: 1,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: snapshot},
			failures:  make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != JobPausedOutcome || !errors.Is(result.TerminationCause, ErrGenerationReplayBudget) ||
		result.Measure.DiscoveredFiles != 0 || len(output.directories) != 0 || output.pauseCalls != 1 {
		t.Fatalf("result=%+v directories=%v pause=%d", result, output.directories, output.pauseCalls)
	}
}

func TestTransferJobPropagatesRawChildCursorFailureWithoutPhantomSelection(t *testing.T) {
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
	job, err := newTestTransferJob(t, TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: prefixFailureCatalog{
			root: root, branch: branch,
			rootPages: jobSnapshot(t, share, root, 1, jobDirectoryEntry(t, branch, "branch")),
			first:     first, failure: cursorFailure,
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != JobPausedOutcome || !errors.Is(result.TerminationCause, cursorFailure) ||
		errors.Is(result.TerminationCause, ErrSelectionTargetMissing) ||
		result.Measure.DiscoveredFiles != 0 || result.Measure.DiscoveredBytes != 0 ||
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
	job, err := newTestTransferJob(t, TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: newJobOutput(share),
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
	drift := content.ErrRevisionDrift
	run.recordFileFailure(FileJobFailure{Path: "omitted-drift.bin", Cause: drift})
	want := errors.Join(ErrSelectionTargetMissing, errors.New("path missing.bin"))
	run.selectionResolutionFailure = want
	result := run.finish(context.Background())
	if result.Outcome != JobCompletedWithErrors ||
		result.SelectionResolutionFailure != want ||
		result.SourceDriftFailure != drift ||
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
	if result.Outcome != JobSucceeded || result.SucceededFiles != 1 ||
		output.pauseCalls != 0 || output.completeCalls != 1 {
		t.Fatalf("result=%+v pauses=%d completes=%d", result, output.pauseCalls, output.completeCalls)
	}
}

func TestHugeFileRangePlanningDemandsOneBlockBeforeCancellation(t *testing.T) {
	share := transferID[catalog.ShareInstance](184)
	root := transferID[catalog.DirectoryID](185)
	file := transferID[catalog.FileID](186)
	rules, _ := NewSelectionRules(true, nil)
	descriptor := jobDescriptor(t, share, file, 1, catalog.MaxFileSize)
	opened, _ := NewOpenedRevision(transferID[content.LeaseID](187), descriptor)
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelFirstDemandReader{cancel: cancel}
	output := newJobOutput(share)
	job, err := newTestTransferJob(t, TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 1, jobEntry(t, file, "huge.bin", catalog.MaxFileSize)),
			}, failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{
			opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
		},
		Blocks: reader, Output: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(ctx)
	want := content.Range{Offset: 0, End: uint64(catalog.MinChunkSize)}
	if result.Outcome != JobPausedOutcome || reader.calls != 1 || reader.requested != want ||
		output.pauseCalls != 1 {
		t.Fatalf("outcome=%d calls=%d requested=%+v pauses=%d", result.Outcome,
			reader.calls, reader.requested, output.pauseCalls)
	}
}
