package transfer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer/fault"
)

func TestTransferJobUsesCatalogClientBrokerAndSparseFileLocalResume(t *testing.T) {
	share := transferID[catalog.ShareInstance](1)
	root := transferID[catalog.DirectoryID](2)
	directory := transferID[catalog.DirectoryID](3)
	emptyDirectory := transferID[catalog.DirectoryID](4)
	fileA := transferID[catalog.FileID](10)
	fileB := transferID[catalog.FileID](11)
	emptyFile := transferID[catalog.FileID](12)
	chunk := uint64(catalog.MinChunkSize)
	rootSnapshot := jobSnapshot(t, share, root, 20,
		jobEntry(t, fileA, "a.bin", 2*chunk), jobDirectoryEntry(t, directory, "folder"),
	)
	directorySnapshot := jobSnapshot(t, share, directory, 21,
		jobEntry(t, fileB, "b.bin", chunk), jobDirectoryEntry(t, emptyDirectory, "empty-dir"),
		jobEntry(t, emptyFile, "empty.txt", 0),
	)
	emptySnapshot := jobSnapshot(t, share, emptyDirectory, 22)
	client, wire := newJobCatalogClient(t, share, rootSnapshot, directorySnapshot, emptySnapshot)
	revisions := &jobRevisionClient{opened: make(map[catalog.FileID]OpenedRevision), failures: make(map[catalog.FileID]error)}
	for index, file := range []catalog.FileID{fileA, fileB, emptyFile} {
		descriptor := jobDescriptor(t, share, file, byte(30+index), []uint64{2 * chunk, chunk, 0}[index])
		opened, err := NewOpenedRevision(transferID[content.LeaseID](byte(40+index)), descriptor)
		if err != nil {
			t.Fatal(err)
		}
		revisions.opened[file] = opened
	}
	lane := &jobLane{indices: make(map[catalog.FileID][]uint64)}
	lanes, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](50), RaceWidth: 1})
	defer lanes.Close()
	if err := lanes.Add(LaneIdentity{ID: 1, Epoch: 1}, LaneRouteRelay, lane); err != nil {
		t.Fatal(err)
	}
	budget, _ := NewPlaintextBudget(8 * chunk)
	broker, err := NewBlockBroker(BlockBrokerConfig{ShareInstance: share, Lanes: lanes, MaxBytes: 4 * chunk, ProcessBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	output := newJobOutput(share)
	firstBlock, _ := content.NewRangeSet([]content.Range{{Offset: 0, End: chunk}})
	output.durable["a.bin"] = firstBlock
	rules, _ := NewSelectionRules(true, nil)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: client,
		Revisions: revisions, Blocks: broker, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != 3 || result.Progress.ConnectionSizeClass() != ConnectionSizeSmall ||
		result.Progress.DiscoveredFiles != 3 || result.Progress.DiscoveredBytes != 3*chunk ||
		result.Progress.VerifiedBytes != 3*chunk || result.Progress.NewlyVerifiedBytes != 2*chunk ||
		result.Progress.PublishedFiles != 3 || result.Progress.PublishedBytes != 3*chunk ||
		!result.Progress.CountersExact {
		t.Fatalf("result=%+v", result)
	}
	if !slices.Equal(revisions.order, []catalog.FileID{fileA, fileB, emptyFile}) {
		t.Fatalf("revision order=%v", revisions.order)
	}
	lane.mu.Lock()
	indicesA, indicesB, indicesEmpty := slices.Clone(lane.indices[fileA]), slices.Clone(lane.indices[fileB]), slices.Clone(lane.indices[emptyFile])
	lane.mu.Unlock()
	if !slices.Equal(indicesA, []uint64{1}) || !slices.Equal(indicesB, []uint64{0}) || len(indicesEmpty) != 0 {
		t.Fatalf("block indices a=%v b=%v empty=%v", indicesA, indicesB, indicesEmpty)
	}
	wire.mu.Lock()
	loads := slices.Clone(wire.loads)
	wire.mu.Unlock()
	wantLoads := []catalog.DirectoryID{
		root, root, root, directory, directory, directory,
		emptyDirectory, emptyDirectory, emptyDirectory,
	}
	if !slices.Equal(loads, wantLoads) {
		t.Fatalf("catalog loads=%v", loads)
	}
	if !slices.Equal(output.directories, []string{"", "folder", "folder/empty-dir"}) ||
		!slices.Equal(output.finalized, []string{"folder/empty-dir", "folder", ""}) || output.finished != DirectTreeOutcomeSuccess {
		t.Fatalf("directories=%v finalized=%v outcome=%v", output.directories, output.finalized, output.finished)
	}
	if client.CachedBytes() != 0 {
		t.Fatalf("job retained catalog source bytes=%d", client.CachedBytes())
	}
}

func TestTransferJobRejectsRegressiveCheckpointAndCatalogCycle(t *testing.T) {
	t.Run("regressive durable ranges", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](52)
		root := transferID[catalog.DirectoryID](53)
		file := transferID[catalog.FileID](54)
		chunk := uint64(catalog.MinChunkSize)
		descriptor := jobDescriptor(t, share, file, 55, 2*chunk)
		opened, _ := NewOpenedRevision(transferID[content.LeaseID](56), descriptor)
		output := newJobOutput(share)
		first, _ := content.NewRangeSet([]content.Range{{Offset: 0, End: chunk}})
		output.durable["file.bin"] = first
		output.transactionScript.dropPriorRanges = true
		rules, _ := NewSelectionRules(true, nil)
		job, _ := newTestTransferJob(t, testTransferJobConfig{
			ShareInstance: share, SyntheticRoot: root, Rules: rules,
			Catalog: failingCatalog{
				snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
					root: jobSnapshot(t, share, root, 57, jobEntry(t, file, "file.bin", 2*chunk)),
				},
				failures: make(map[catalog.DirectoryID]error),
			},
			Revisions: &jobRevisionClient{
				opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
			},
			Blocks: scriptedRangeReader{}, Materializer: output,
		})
		result := job.Run(context.Background())
		if result.Outcome != DirectTreeOutcomePaused || !output.aborted ||
			result.TerminationFault != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) ||
			result.Progress.VerifiedBytes != 2*chunk || result.Progress.NewlyVerifiedBytes != chunk ||
			result.Progress.PublishedFiles != 0 {
			t.Fatalf("regressive checkpoint result=%+v", result)
		}
	})

	t.Run("checkpoint claims unflushed future range", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](152)
		root := transferID[catalog.DirectoryID](153)
		file := transferID[catalog.FileID](154)
		chunk := uint64(catalog.MinChunkSize)
		exactSize := uint64(defaultFileReadWindowBlocks+1) * chunk
		descriptor := jobDescriptor(t, share, file, 155, exactSize)
		opened, _ := NewOpenedRevision(transferID[content.LeaseID](156), descriptor)
		output := newJobOutput(share)
		windowEnd := uint64(defaultFileReadWindowBlocks) * chunk
		future, _ := content.NewRangeSet([]content.Range{{Offset: windowEnd, End: exactSize}})
		output.transactionScript.checkpointExtra = future
		rules, _ := NewSelectionRules(true, nil)
		job, _ := newTestTransferJob(t, testTransferJobConfig{
			ShareInstance: share, SyntheticRoot: root, Rules: rules,
			Catalog: failingCatalog{
				snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
					root: jobSnapshot(t, share, root, 157, jobEntry(t, file, "file.bin", exactSize)),
				},
				failures: make(map[catalog.DirectoryID]error),
			},
			Revisions: &jobRevisionClient{
				opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
			},
			Blocks: scriptedRangeReader{}, Materializer: output,
		})
		result := job.Run(context.Background())
		if result.Outcome != DirectTreeOutcomePaused || !output.aborted ||
			result.TerminationFault != mustOutputFault(fault.ScopeOutputPause, fault.OutputContract) ||
			result.Progress.VerifiedBytes != windowEnd || result.Progress.NewlyVerifiedBytes != windowEnd ||
			result.Progress.VerifiedBytes == exactSize || result.Progress.PublishedFiles != 0 {
			t.Fatalf("future checkpoint result=%+v", result)
		}
	})

	t.Run("reused directory identity", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](58)
		root := transferID[catalog.DirectoryID](59)
		child := transferID[catalog.DirectoryID](60)
		rules, _ := NewSelectionRules(true, nil)
		output := newJobOutput(share)
		job, _ := newTestTransferJob(t, testTransferJobConfig{
			ShareInstance: share, SyntheticRoot: root, Rules: rules,
			Catalog: failingCatalog{
				snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
					root:  jobSnapshot(t, share, root, 61, jobDirectoryEntry(t, child, "child")),
					child: jobSnapshot(t, share, child, 62, jobDirectoryEntry(t, root, "cycle")),
				},
				failures: make(map[catalog.DirectoryID]error),
			},
			Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
		})
		result := job.Run(context.Background())
		if result.Outcome != DirectTreeOutcomePaused ||
			result.TerminationFault != mustCatalogFault(fault.ScopeSessionTerminal, fault.CatalogInvalidGeneration) {
			t.Fatalf("cyclic catalog result=%+v", result)
		}
	})
}

type failingCatalog struct {
	snapshots map[catalog.DirectoryID]catalog.DirectorySnapshot
	failures  map[catalog.DirectoryID]error
}

type countingJobCatalog struct {
	mu        sync.Mutex
	snapshots map[catalog.DirectoryID]catalog.DirectorySnapshot
	loads     map[catalog.DirectoryID]int
}

func (c *countingJobCatalog) LoadDirectory(_ context.Context, directory catalog.DirectoryID) (catalog.DirectorySnapshot, error) {
	c.mu.Lock()
	c.loads[directory]++
	snapshot := c.snapshots[directory]
	c.mu.Unlock()
	return snapshot, nil
}

func (c *countingJobCatalog) AcquireDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, func(), error) {
	snapshot, err := c.LoadDirectory(ctx, directory)
	return snapshot, func() {}, err
}

func (c *countingJobCatalog) OpenDirectoryPages(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	snapshot, err := c.LoadDirectory(ctx, directory)
	return snapshotPages(snapshot), err
}

func (c *countingJobCatalog) loadCount(directory catalog.DirectoryID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads[directory]
}

func (c failingCatalog) LoadDirectory(_ context.Context, directory catalog.DirectoryID) (catalog.DirectorySnapshot, error) {
	if err := c.failures[directory]; err != nil {
		return catalog.DirectorySnapshot{}, err
	}
	return c.snapshots[directory], nil
}

func (c failingCatalog) AcquireDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, func(), error) {
	snapshot, err := c.LoadDirectory(ctx, directory)
	return snapshot, func() {}, err
}

func (c failingCatalog) OpenDirectoryPages(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	snapshot, err := c.LoadDirectory(ctx, directory)
	if err != nil {
		return nil, err
	}
	return snapshotPages(snapshot), nil
}

func TestTransferJobDiscoveryFailurePreservesIndependentContentWork(t *testing.T) {
	share := transferID[catalog.ShareInstance](60)
	root := transferID[catalog.DirectoryID](61)
	failingDirectory := transferID[catalog.DirectoryID](62)
	openFailure := transferID[catalog.FileID](63)
	blockFailure := transferID[catalog.FileID](64)
	good := transferID[catalog.FileID](65)
	chunk := uint64(catalog.MinChunkSize)
	snapshot := jobSnapshot(t, share, root, 66,
		jobEntry(t, blockFailure, "block.bin", chunk), jobDirectoryEntry(t, failingDirectory, "broken"),
		jobEntry(t, good, "good.bin", chunk), jobEntry(t, openFailure, "open.bin", chunk),
	)
	revisions := &jobRevisionClient{
		opened:   make(map[catalog.FileID]OpenedRevision),
		failures: map[catalog.FileID]error{openFailure: sourceChangedFailure(errors.New("stale"))},
	}
	for index, file := range []catalog.FileID{blockFailure, good} {
		descriptor := jobDescriptor(t, share, file, byte(70+index), chunk)
		revisions.opened[file], _ = NewOpenedRevision(transferID[content.LeaseID](byte(75+index)), descriptor)
	}
	lane := &jobLane{indices: make(map[catalog.FileID][]uint64)}
	lanes, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](80), RaceWidth: 1})
	defer lanes.Close()
	_ = lanes.Add(LaneIdentity{ID: 1, Epoch: 1}, LaneRouteRelay, lane)
	budget, _ := NewPlaintextBudget(4 * chunk)
	broker, _ := NewBlockBroker(BlockBrokerConfig{ShareInstance: share, Lanes: lanes, MaxBytes: 2 * chunk, ProcessBudget: budget})
	defer broker.Close()
	output := newJobOutput(share)
	rules, _ := NewSelectionRules(true, nil)
	job, _ := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: snapshot}, failures: map[catalog.DirectoryID]error{
			failingDirectory: catalogDirectoryFailure(fault.CatalogUnavailable, errors.New("permission denied")),
		}},
		Revisions: revisions, Blocks: broker, Materializer: output,
	})
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartial || result.SucceededFiles != 2 ||
		len(result.Directories) != 1 || len(result.Files) != 1 {
		t.Fatalf("result=%+v", result)
	}
	if result.Progress.ConnectionSizeClass() != ConnectionSizeUnknown || result.Progress.Discovery != DiscoveryFailed {
		t.Fatalf("failed discovery measure=%+v", result.Progress)
	}
	if len(revisions.order) != 3 || len(output.transactions) != 2 ||
		output.intent.IsZero() || !slices.Equal(output.directories, []string{""}) {
		t.Fatalf("independent discovery work missing: revisions=%v transactions=%d intent=%v directories=%v", revisions.order, len(output.transactions), !output.intent.IsZero(), output.directories)
	}
}

func TestTransferJobKeepsAdmissionLowerBoundSeparateFromExactResultMeasure(t *testing.T) {
	share := transferID[catalog.ShareInstance](130)
	root := transferID[catalog.DirectoryID](131)
	entries := make([]catalog.Entry, 0, SmallTransferFileLimit+1)
	revisions := &jobRevisionClient{
		opened: make(map[catalog.FileID]OpenedRevision), failures: make(map[catalog.FileID]error),
	}
	for index := uint64(1); index <= SmallTransferFileLimit+1; index++ {
		file := transferID[catalog.FileID](byte(index))
		entries = append(entries, jobEntry(t, file, fmt.Sprintf("file-%02d", index), 1))
		descriptor := jobDescriptor(t, share, file, byte(index+40), 1)
		revisions.opened[file], _ = NewOpenedRevision(transferID[content.LeaseID](byte(index+80)), descriptor)
	}
	rules, _ := NewSelectionRules(true, nil)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: jobSnapshot(t, share, root, 1, entries...)},
			failures:  make(map[catalog.DirectoryID]error),
		},
		Revisions: revisions, Blocks: scriptedRangeReader{}, Materializer: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	updates := job.ProgressSnapshots()
	result := job.Run(context.Background())
	var admissionMeasure ReceiveProgressSnapshot
	for measure := range updates {
		admissionMeasure = measure
	}
	if result.Outcome != DirectTreeOutcomeSuccess || result.Progress.Discovery != DiscoveryComplete ||
		result.Progress.DiscoveredFiles != SmallTransferFileLimit+1 {
		t.Fatalf("exact result=%+v", result)
	}
	if admissionMeasure.ConnectionSizeClass() != ConnectionSizeLarge || admissionMeasure.DiscoveredFiles != SmallTransferFileLimit+1 ||
		admissionMeasure.Discovery != DiscoveryComplete {
		t.Fatalf("admission lower bound=%+v", admissionMeasure)
	}
}

func TestTransferJobOmittedRootChildrenPauseBeforeOutputAdmission(t *testing.T) {
	share := transferID[catalog.ShareInstance](132)
	root := transferID[catalog.DirectoryID](133)
	rules, _ := NewSelectionRules(true, nil)
	output := newJobOutput(share)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshotWithOmissions(t, share, root, 1, 2),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	updates := job.ProgressSnapshots()
	result := job.Run(context.Background())
	var admissionMeasure ReceiveProgressSnapshot
	for measure := range updates {
		admissionMeasure = measure
	}
	if result.Outcome != DirectTreeOutcomePaused || len(result.Directories) != 1 ||
		!errors.Is(result.Directories[0].Cause, ErrCatalogEntriesOmitted) || result.Progress.ConnectionSizeClass() != ConnectionSizeUnknown {
		t.Fatalf("result=%+v", result)
	}
	if result.TerminationFault != mustCatalogFault(fault.ScopeSessionTerminal, fault.CatalogInvalidGeneration) ||
		output.pauseCalls != 1 || output.completeCalls != 0 {
		// The root omission is a session-scoped pause, not an ordinary branch
		// completion.
		t.Fatalf("root omission termination=%v", result.TerminationCause)
	}
	if admissionMeasure.ConnectionSizeClass() != ConnectionSizeUnknown || admissionMeasure.Discovery != DiscoveryFailed {
		t.Fatalf("admission measure=%+v", admissionMeasure)
	}
}

func TestTransferJobResolvesPathSelectionInsideBoundedJobTraversal(t *testing.T) {
	share := transferID[catalog.ShareInstance](134)
	root := transferID[catalog.DirectoryID](135)
	folder := transferID[catalog.DirectoryID](136)
	unrelated := transferID[catalog.DirectoryID](137)
	file := transferID[catalog.FileID](138)
	source := &countingJobCatalog{
		snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
			root: jobSnapshot(t, share, root, 1,
				jobDirectoryEntry(t, folder, "folder"), jobDirectoryEntry(t, unrelated, "unrelated"),
			),
			folder:    jobSnapshot(t, share, folder, 2, jobEntry(t, file, "file.bin", 1)),
			unrelated: jobSnapshot(t, share, unrelated, 3, jobEntry(t, transferID[catalog.FileID](139), "ignored.bin", 1)),
		},
		loads: make(map[catalog.DirectoryID]int),
	}
	descriptor := jobDescriptor(t, share, file, 4, 1)
	opened, _ := NewOpenedRevision(transferID[content.LeaseID](140), descriptor)
	rules, _ := NewPathSelectionRules([]string{"folder/file.bin"})
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: source,
		Revisions: &jobRevisionClient{
			opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
		},
		Blocks: scriptedRangeReader{}, Materializer: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != 1 || result.Progress.DiscoveredFiles != 1 {
		t.Fatalf("result=%+v", result)
	}
	if source.loadCount(root) != 3 || source.loadCount(folder) != 3 || source.loadCount(unrelated) != 0 {
		t.Fatalf("catalog loads root=%d folder=%d unrelated=%d", source.loadCount(root), source.loadCount(folder), source.loadCount(unrelated))
	}
}

func TestTransferJobReportsMissingPathTargetAfterCompleteTraversal(t *testing.T) {
	share := transferID[catalog.ShareInstance](141)
	root := transferID[catalog.DirectoryID](142)
	rules, _ := NewPathSelectionRules([]string{"missing.bin"})
	output := newJobOutput(share)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: jobSnapshot(t, share, root, 1)},
			failures:  make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Materializer: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != DirectTreeOutcomePartial || result.TerminationCause != nil ||
		len(result.Directories) != 0 || !errors.Is(result.SelectionResolutionFailure, ErrSelectionTargetMissing) ||
		output.aborted || output.pauseCalls != 0 || output.completeCalls != 1 ||
		!slices.Equal(output.directories, []string{""}) || !slices.Equal(output.finalized, []string{""}) {
		t.Fatalf("result=%+v output aborted=%v", result, output.aborted)
	}
}

type sessionFailingBlocks struct{ err error }

func (s sessionFailingBlocks) ReadRange(context.Context, content.LeaseID, content.FileRevisionDescriptor, content.Range, RangeSink) error {
	return s.err
}

type crossCancelCatalog struct {
	root          catalog.DirectoryID
	rootSnapshot  catalog.DirectorySnapshot
	childSnapshot catalog.DirectorySnapshot
	childStarted  chan struct{}
	childDone     chan struct{}
	startOnce     sync.Once
}

func (source *crossCancelCatalog) LoadDirectory(ctx context.Context, directory catalog.DirectoryID) (catalog.DirectorySnapshot, error) {
	if directory == source.root {
		return source.rootSnapshot, nil
	}
	source.startOnce.Do(func() { close(source.childStarted) })
	select {
	case <-source.childDone:
	case <-ctx.Done():
		return catalog.DirectorySnapshot{}, ctx.Err()
	}
	return source.childSnapshot, nil
}

func (source *crossCancelCatalog) AcquireDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectorySnapshot, func(), error) {
	snapshot, err := source.LoadDirectory(ctx, directory)
	return snapshot, func() {}, err
}

func (source *crossCancelCatalog) OpenDirectoryPages(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	snapshot, err := source.LoadDirectory(ctx, directory)
	return snapshotPages(snapshot), err
}

type firstRangeSignalBlocks struct{ started chan<- struct{} }

func (blocks firstRangeSignalBlocks) ReadRange(
	ctx context.Context,
	_ content.LeaseID,
	_ content.FileRevisionDescriptor,
	requested content.Range,
	sink RangeSink,
) error {
	select {
	case blocks.started <- struct{}{}:
	default:
	}
	return sink.WriteRange(ctx, requested.Offset, make([]byte, requested.Length()))
}

func TestTransferJobTransfersCommittedSiblingBeforeDelayedDiscovery(t *testing.T) {
	share := transferID[catalog.ShareInstance](143)
	root := transferID[catalog.DirectoryID](144)
	child := transferID[catalog.DirectoryID](145)
	file := transferID[catalog.FileID](146)
	descriptor := jobDescriptor(t, share, file, 147, 1)
	opened, _ := NewOpenedRevision(transferID[content.LeaseID](148), descriptor)
	source := &crossCancelCatalog{
		root: root,
		rootSnapshot: jobSnapshot(t, share, root, 1,
			jobDirectoryEntry(t, child, "a-slow"), jobEntry(t, file, "z-file.bin", 1),
		),
		childSnapshot: jobSnapshot(t, share, child, 2),
		childStarted:  make(chan struct{}), childDone: make(chan struct{}),
	}
	rules, _ := NewSelectionRules(true, nil)
	rangeStarted := make(chan struct{}, 1)
	job, err := newTestTransferJob(t, testTransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: source,
		Revisions: &jobRevisionClient{
			opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
		},
		Blocks: firstRangeSignalBlocks{started: rangeStarted}, Materializer: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	resultCh := make(chan JobResult, 1)
	go func() { resultCh <- job.Run(context.Background()) }()
	select {
	case <-rangeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first committed sibling did not start while discovery was delayed")
	}
	select {
	case <-source.childStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("lexically earlier child was not reached while the later parent file transferred")
	}
	select {
	case <-resultCh:
		t.Fatal("job completed before delayed sibling discovery was released")
	default:
	}
	close(source.childDone)
	select {
	case result := <-resultCh:
		if result.Outcome != DirectTreeOutcomeSuccess || result.SucceededFiles != 1 || result.TerminationCause != nil {
			t.Fatalf("result=%+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("job did not finish after delayed sibling was released")
	}
}
