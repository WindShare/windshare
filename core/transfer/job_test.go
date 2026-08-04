package transfer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/session/protocolsession"
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
	if err := lanes.Add(LaneIdentity{ID: 1, Epoch: 1}, lane); err != nil {
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
	job, err := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: client,
		Revisions: revisions, Blocks: broker, Output: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != JobSucceeded || result.SucceededFiles != 3 || result.Measure.Class() != SelectionSmall ||
		result.Measure.DiscoveredFiles != 3 || result.Measure.DiscoveredBytes != 3*chunk {
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
	if len(loads) != 3 || loads[0] != root || loads[1] != directory || loads[2] != emptyDirectory {
		t.Fatalf("catalog loads=%v", loads)
	}
	if !slices.Equal(output.directories, []string{"folder", "folder/empty-dir"}) ||
		!slices.Equal(output.finalized, []string{"folder/empty-dir", "folder"}) || output.finished != JobSucceeded {
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
		job, _ := NewTransferJob(TransferJobConfig{
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
			Blocks: scriptedRangeReader{}, Output: output,
		})
		result := job.Run(context.Background())
		if result.Outcome != JobPausedOutcome || !output.aborted ||
			!errors.Is(result.TerminationCause, ErrOutputContract) {
			t.Fatalf("regressive checkpoint result=%+v", result)
		}
	})

	t.Run("reused directory identity", func(t *testing.T) {
		share := transferID[catalog.ShareInstance](58)
		root := transferID[catalog.DirectoryID](59)
		child := transferID[catalog.DirectoryID](60)
		rules, _ := NewSelectionRules(true, nil)
		output := newJobOutput(share)
		job, _ := NewTransferJob(TransferJobConfig{
			ShareInstance: share, SyntheticRoot: root, Rules: rules,
			Catalog: failingCatalog{
				snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
					root:  jobSnapshot(t, share, root, 61, jobDirectoryEntry(t, child, "child")),
					child: jobSnapshot(t, share, child, 62, jobDirectoryEntry(t, root, "cycle")),
				},
				failures: make(map[catalog.DirectoryID]error),
			},
			Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: output,
		})
		result := job.Run(context.Background())
		if result.Outcome != JobPausedOutcome || !errors.Is(result.TerminationCause, ErrCatalogIdentity) {
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

type jobDirectoryFailure struct{ error }

func (jobDirectoryFailure) DirectoryFailure() {}

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
	revisions := &jobRevisionClient{opened: make(map[catalog.FileID]OpenedRevision), failures: map[catalog.FileID]error{openFailure: errors.New("stale")}}
	for index, file := range []catalog.FileID{blockFailure, good} {
		descriptor := jobDescriptor(t, share, file, byte(70+index), chunk)
		revisions.opened[file], _ = NewOpenedRevision(transferID[content.LeaseID](byte(75+index)), descriptor)
	}
	lane := &jobLane{indices: make(map[catalog.FileID][]uint64)}
	lanes, _ := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](80), RaceWidth: 1})
	defer lanes.Close()
	_ = lanes.Add(LaneIdentity{ID: 1, Epoch: 1}, lane)
	budget, _ := NewPlaintextBudget(4 * chunk)
	broker, _ := NewBlockBroker(BlockBrokerConfig{ShareInstance: share, Lanes: lanes, MaxBytes: 2 * chunk, ProcessBudget: budget})
	defer broker.Close()
	output := newJobOutput(share)
	rules, _ := NewSelectionRules(true, nil)
	job, _ := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog:   failingCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: snapshot}, failures: map[catalog.DirectoryID]error{failingDirectory: jobDirectoryFailure{errors.New("permission denied")}}},
		Revisions: revisions, Blocks: broker, Output: output,
	})
	result := job.Run(context.Background())
	if result.Outcome != JobCompletedWithErrors || result.SucceededFiles != 2 ||
		len(result.Directories) != 1 || len(result.Files) != 1 {
		t.Fatalf("result=%+v", result)
	}
	if result.Measure.Class() != SelectionUnknown || result.Measure.DiscoveryTerminalSuccess {
		t.Fatalf("failed discovery measure=%+v", result.Measure)
	}
	if len(revisions.order) != 3 || len(output.transactions) != 2 || len(output.admitted) != 1 {
		t.Fatalf("independent discovery work missing: revisions=%v transactions=%d admissions=%d", revisions.order, len(output.transactions), len(output.admitted))
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
	job, err := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: jobSnapshot(t, share, root, 1, entries...)},
			failures:  make(map[catalog.DirectoryID]error),
		},
		Revisions: revisions, Blocks: scriptedRangeReader{}, Output: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	updates := job.SelectionMeasures()
	result := job.Run(context.Background())
	var admissionMeasure SelectionMeasure
	for measure := range updates {
		admissionMeasure = measure
	}
	if result.Outcome != JobSucceeded || !result.Measure.DiscoveryTerminalSuccess ||
		result.Measure.DiscoveredFiles != SmallTransferFileLimit+1 {
		t.Fatalf("exact result=%+v", result)
	}
	if admissionMeasure.Class() != SelectionLarge || admissionMeasure.DiscoveredFiles != SmallTransferFileLimit+1 ||
		!admissionMeasure.DiscoveryTerminalSuccess {
		t.Fatalf("admission lower bound=%+v", admissionMeasure)
	}
}

func TestTransferJobOmittedChildrenRemainUnknownAndCompleteWithErrors(t *testing.T) {
	share := transferID[catalog.ShareInstance](132)
	root := transferID[catalog.DirectoryID](133)
	rules, _ := NewSelectionRules(true, nil)
	job, err := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshotWithOmissions(t, share, root, 1, 2),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	updates := job.SelectionMeasures()
	result := job.Run(context.Background())
	var admissionMeasure SelectionMeasure
	for measure := range updates {
		admissionMeasure = measure
	}
	if result.Outcome != JobCompletedWithErrors || len(result.Directories) != 1 ||
		!errors.Is(result.Directories[0].Cause, ErrCatalogEntriesOmitted) || result.Measure.Class() != SelectionUnknown {
		t.Fatalf("result=%+v", result)
	}
	if admissionMeasure.Class() != SelectionUnknown || admissionMeasure.DiscoveryTerminalSuccess {
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
	job, err := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: source,
		Revisions: &jobRevisionClient{
			opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
		},
		Blocks: scriptedRangeReader{}, Output: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != JobSucceeded || result.SucceededFiles != 1 || result.Measure.DiscoveredFiles != 1 {
		t.Fatalf("result=%+v", result)
	}
	if source.loadCount(root) != 1 || source.loadCount(folder) != 1 || source.loadCount(unrelated) != 0 {
		t.Fatalf("catalog loads root=%d folder=%d unrelated=%d", source.loadCount(root), source.loadCount(folder), source.loadCount(unrelated))
	}
}

func TestTransferJobReportsMissingPathTargetAfterCompleteTraversal(t *testing.T) {
	share := transferID[catalog.ShareInstance](141)
	root := transferID[catalog.DirectoryID](142)
	rules, _ := NewPathSelectionRules([]string{"missing.bin"})
	output := newJobOutput(share)
	job, err := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: jobSnapshot(t, share, root, 1)},
			failures:  make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != JobPausedOutcome || !errors.Is(result.TerminationCause, ErrSelectionTargetMissing) ||
		output.aborted || output.pauseCalls != 0 || output.completeCalls != 0 {
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
	doneOnce      sync.Once
}

func (source *crossCancelCatalog) LoadDirectory(_ context.Context, directory catalog.DirectoryID) (catalog.DirectorySnapshot, error) {
	if directory == source.root {
		return source.rootSnapshot, nil
	}
	source.startOnce.Do(func() { close(source.childStarted) })
	source.doneOnce.Do(func() { close(source.childDone) })
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

type childSynchronizedFatalBlocks struct {
	childDone <-chan struct{}
	err       error
}

func (blocks childSynchronizedFatalBlocks) ReadRange(
	context.Context,
	content.LeaseID,
	content.FileRevisionDescriptor,
	content.Range,
	RangeSink,
) error {
	<-blocks.childDone
	return blocks.err
}

func TestTransferJobCompletesCatalogDiscoveryBeforeFirstRange(t *testing.T) {
	share := transferID[catalog.ShareInstance](143)
	root := transferID[catalog.DirectoryID](144)
	child := transferID[catalog.DirectoryID](145)
	file := transferID[catalog.FileID](146)
	descriptor := jobDescriptor(t, share, file, 147, 1)
	opened, _ := NewOpenedRevision(transferID[content.LeaseID](148), descriptor)
	source := &crossCancelCatalog{
		root: root,
		rootSnapshot: jobSnapshot(t, share, root, 1,
			jobEntry(t, file, "file.bin", 1), jobDirectoryEntry(t, child, "later"),
		),
		childSnapshot: jobSnapshot(t, share, child, 2),
		childStarted:  make(chan struct{}), childDone: make(chan struct{}),
	}
	fatal := NewSessionFailure(errors.New("content authority ended"))
	rules, _ := NewSelectionRules(true, nil)
	job, err := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules, Catalog: source,
		Revisions: &jobRevisionClient{
			opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error),
		},
		Blocks: childSynchronizedFatalBlocks{childDone: source.childDone, err: fatal}, Output: newJobOutput(share),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != JobPausedOutcome || !errors.Is(result.TerminationCause, fatal) {
		t.Fatalf("result=%+v", result)
	}
	select {
	case <-source.childDone:
	default:
		t.Fatal("file range started before catalog discovery completed")
	}
}

func TestTransferJobSessionFailureAbortsJob(t *testing.T) {
	share := transferID[catalog.ShareInstance](90)
	root := transferID[catalog.DirectoryID](91)
	file := transferID[catalog.FileID](92)
	laterFile := transferID[catalog.FileID](96)
	chunk := uint64(catalog.MinChunkSize)
	snapshot := jobSnapshot(t, share, root, 93,
		jobEntry(t, file, "file.bin", chunk), jobEntry(t, laterFile, "later.bin", chunk),
	)
	descriptor := jobDescriptor(t, share, file, 94, chunk)
	opened, _ := NewOpenedRevision(transferID[content.LeaseID](95), descriptor)
	revisions := &jobRevisionClient{opened: map[catalog.FileID]OpenedRevision{file: opened}, failures: make(map[catalog.FileID]error)}
	output := newJobOutput(share)
	rules, _ := NewSelectionRules(true, nil)
	terminal := NewSessionFailure(errors.New("authenticated terminal"))
	job, _ := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog:   failingCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: snapshot}, failures: make(map[catalog.DirectoryID]error)},
		Revisions: revisions, Blocks: sessionFailingBlocks{err: terminal}, Output: output,
	})
	result := job.Run(context.Background())
	if result.Outcome != JobPausedOutcome || !errors.Is(result.TerminationCause, terminal) || !output.aborted || output.finished != 0 ||
		result.Measure.Class() != SelectionSmall || !result.Measure.DiscoveryTerminalSuccess ||
		result.Measure.DiscoveredFiles != 2 || result.Measure.DiscoveredBytes != 2*chunk {
		t.Fatalf("result=%+v output=%+v", result, output)
	}
	if second := job.Run(context.Background()); second.Outcome != JobPausedOutcome || !errors.Is(second.TerminationCause, ErrTransferJobRun) {
		t.Fatalf("second run=%+v", second)
	}
}

type retiredCountingRangeReader struct{ calls int }

func (reader *retiredCountingRangeReader) ReadRange(
	context.Context,
	content.LeaseID,
	content.FileRevisionDescriptor,
	content.Range,
	RangeSink,
) error {
	reader.calls++
	return nil
}

func TestTransferJobAcceptsRecoveredImmediateRetirementWithoutContentOrSecondFileSettlement(t *testing.T) {
	share := transferID[catalog.ShareInstance](96)
	output := newJobOutput(share)
	revisions := &jobRevisionClient{}
	blocks := &retiredCountingRangeReader{}
	job, file := branchJob(t, output, revisions, blocks)
	opened := revisions.opened[file]
	locator, err := NewPathOutputLocator("file.bin")
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewOutputFileTarget(output.BackendID(), output.SessionID(), opened.Descriptor, locator)
	if err != nil {
		t.Fatal(err)
	}
	var identity OutputObjectIdentity
	digest := sha256.Sum256([]byte("recovered-retiring-output"))
	copy(identity[:], digest[:])
	binding, err := BindOutputFileTarget(target, identity)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := NewRetiredFileSettlement(binding)
	if err != nil {
		t.Fatal(err)
	}
	output.immediate["file.bin"] = retired

	result := job.Run(context.Background())
	if result.Outcome != JobCompletedWithErrors || result.Settlement.Kind() != JobClosed ||
		result.TerminationCause != nil || result.SettlementFailure != nil || result.SucceededFiles != 0 {
		t.Fatalf("result = %+v", result)
	}
	if len(result.Files) != 1 || !errors.Is(result.Files[0].Cause, ErrOutputRetired) ||
		result.Files[0].SettlementFailure != nil {
		t.Fatalf("retired file result = %+v", result.Files)
	}
	settledBinding, bound := result.Files[0].Settlement.OutputBinding()
	if result.Files[0].Settlement.Kind() != FileRetired || !bound || settledBinding != binding {
		t.Fatalf("retired settlement = %+v", result.Files[0].Settlement)
	}
	if blocks.calls != 0 || len(output.transactions) != 0 || output.pauseCalls != 0 ||
		output.completeCalls != 1 || output.finished != JobCompletedWithErrors {
		t.Fatalf(
			"range calls=%d transactions=%d pause=%d complete=%d outcome=%v",
			blocks.calls, len(output.transactions), output.pauseCalls, output.completeCalls, output.finished,
		)
	}
	if !slices.Equal(revisions.released, []content.LeaseID{opened.LeaseID}) {
		t.Fatalf("released leases = %v, want %v", revisions.released, opened.LeaseID)
	}
}

func TestTransferJobRetiresOnlyWithTypedPermanentSourceAuthority(t *testing.T) {
	tests := []struct {
		name        string
		blocks      RangeReader
		configure   func(*jobOutput)
		wantOutcome JobOutcome
		wantPause   FilePauseReason
		wantRetire  FileRetireReason
	}{
		{
			name:        "unclassified range failure pauses",
			blocks:      scriptedRangeReader{err: errors.New("retryable transport failure")},
			wantOutcome: JobPausedOutcome, wantPause: FilePauseTransportFailure,
		},
		{
			name:   "output sink failure cannot impersonate a permanent source failure",
			blocks: scriptedRangeReader{},
			configure: func(output *jobOutput) {
				output.transactionScript.writeErr = NewOutputSessionError(errors.New("output temporarily unavailable"), false)
			},
			wantOutcome: JobPausedOutcome, wantPause: FilePauseOutputFailure,
		},
		{
			name: "permanent marker cannot override output fault scope",
			blocks: scriptedRangeReader{err: errors.Join(
				NewIsolatedPermanentSourceFailure(errors.New("source denied file")),
				NewOutputFault(OutputFaultFile, OutputFaultStateIO, errors.New("checkpoint unavailable")),
			)},
			wantOutcome: JobPausedOutcome, wantPause: FilePauseOutputFailure,
		},
		{
			name:        "typed permanent source failure retires",
			blocks:      scriptedRangeReader{err: NewIsolatedPermanentSourceFailure(errors.New("source denied file"))},
			wantOutcome: JobCompletedWithErrors,
			wantRetire:  FileRetireIsolatedPermanentSourceFailure,
		},
		{
			name:        "invalidated revision retires with its precise reason",
			blocks:      scriptedRangeReader{err: content.ErrRevisionDrift},
			wantOutcome: JobCompletedWithErrors,
			wantRetire:  FileRetireInvalidatedRevision,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			share := transferID[catalog.ShareInstance](97)
			output := newJobOutput(share)
			if test.configure != nil {
				test.configure(output)
			}
			job, _ := branchJob(t, output, &jobRevisionClient{}, test.blocks)
			result := job.Run(context.Background())
			transaction := output.transactions["file.bin"]
			if transaction == nil || result.Outcome != test.wantOutcome || len(result.Files) != 1 {
				t.Fatalf("result=%+v transaction=%+v", result, transaction)
			}
			if test.wantPause != 0 {
				if !slices.Equal(transaction.pauseReasons, []FilePauseReason{test.wantPause}) ||
					len(transaction.retireReasons) != 0 || result.Files[0].Settlement.Kind() != FilePaused {
					t.Fatalf("pause=%v retire=%v failure=%+v", transaction.pauseReasons, transaction.retireReasons, result.Files[0])
				}
				return
			}
			if !slices.Equal(transaction.retireReasons, []FileRetireReason{test.wantRetire}) ||
				len(transaction.pauseReasons) != 0 || result.Files[0].Settlement.Kind() != FileRetired {
				t.Fatalf("pause=%v retire=%v failure=%+v", transaction.pauseReasons, transaction.retireReasons, result.Files[0])
			}
		})
	}
}

func TestTransferJobRevisionSessionFailureStopsBeforeNextFile(t *testing.T) {
	const fileCount = 128
	share := transferID[catalog.ShareInstance](253)
	root := transferID[catalog.DirectoryID](254)
	chunk := uint64(catalog.MinChunkSize)
	terminal := NewSessionFailure(errors.New("receiver runtime closed"))
	entries := make([]catalog.Entry, 0, fileCount)
	failures := make(map[catalog.FileID]error, fileCount)
	for index := range fileCount {
		file := transferID[catalog.FileID](byte(index + 1))
		entries = append(entries, jobEntry(t, file, fmt.Sprintf("file-%03d.bin", index), chunk))
		failures[file] = terminal
	}
	revisions := &jobRevisionClient{opened: make(map[catalog.FileID]OpenedRevision), failures: failures}
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	output := newJobOutput(share)
	job, err := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{
				root: jobSnapshot(t, share, root, 252, entries...),
			},
			failures: make(map[catalog.DirectoryID]error),
		},
		Revisions: revisions, Blocks: scriptedRangeReader{}, Output: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := job.Run(context.Background())
	if result.Outcome != JobPausedOutcome || !errors.Is(result.TerminationCause, terminal) ||
		len(result.Files) != 0 || len(revisions.order) != 1 || !output.aborted || output.finished != 0 {
		t.Fatalf(
			"outcome=%v abort=%v file failures=%d revision attempts=%d output aborted=%v finished=%v",
			result.Outcome, result.TerminationCause, len(result.Files), len(revisions.order), output.aborted, output.finished,
		)
	}
}

func TestTransferJobCancellationDuringDiscoveryIsAborted(t *testing.T) {
	share := transferID[catalog.ShareInstance](100)
	root := transferID[catalog.DirectoryID](101)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	output := newJobOutput(share)
	rules, _ := NewSelectionRules(true, nil)
	job, _ := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog:   failingCatalog{failures: map[catalog.DirectoryID]error{root: context.Canceled}},
		Revisions: &jobRevisionClient{}, Blocks: sessionFailingBlocks{}, Output: output,
	})
	result := job.Run(ctx)
	if result.Outcome != JobPausedOutcome || !errors.Is(result.TerminationCause, context.Canceled) {
		t.Fatalf("result=%+v", result)
	}
}

func TestSessionFailureMarkerAndOutputErrorScope(t *testing.T) {
	if !isSessionFailure(NewSessionFailure(errors.New("x"))) || isSessionFailure(errors.New("x")) {
		t.Fatal("session failure marker classification failed")
	}
	fatal := NewOutputSessionError(errors.New("journal"), true)
	nonfatal := NewOutputSessionError(errors.New("file"), false)
	capabilities := newJobOutput(transferID[catalog.ShareInstance](1)).Capabilities()
	if !outputFailureRequiresJobPause(fatal, capabilities) || outputFailureRequiresJobPause(nonfatal, capabilities) {
		t.Fatal("output error scope classification failed")
	}
	if !outputFailureExplicitlyRequiresJobPause(fatal) || outputFailureExplicitlyRequiresJobPause(nonfatal) {
		t.Fatal("explicit output error scope classification failed")
	}
	if (&OutputSessionError{cause: errors.New("x")}).Error() == "" {
		t.Fatal("output error lost diagnostic")
	}
	if (&SessionFailureError{cause: errors.New("x")}).Error() == "" {
		t.Fatal("session error lost diagnostic")
	}
	(&SessionFailureError{}).SessionFailure()
}

type scriptedRangeReader struct{ err error }

func (r scriptedRangeReader) ReadRange(ctx context.Context, _ content.LeaseID, _ content.FileRevisionDescriptor, requested content.Range, sink RangeSink) error {
	if r.err != nil {
		return r.err
	}
	return sink.WriteRange(ctx, requested.Offset, make([]byte, requested.Length()))
}

func branchJob(t *testing.T, output *jobOutput, revisions *jobRevisionClient, blocks RangeReader) (*TransferJob, catalog.FileID) {
	t.Helper()
	share := output.share
	root := transferID[catalog.DirectoryID](111)
	file := transferID[catalog.FileID](112)
	size := uint64(catalog.MinChunkSize)
	snapshot := jobSnapshot(t, share, root, 113, jobEntry(t, file, "file.bin", size))
	if revisions.opened == nil {
		revisions.opened = make(map[catalog.FileID]OpenedRevision)
	}
	if revisions.failures == nil {
		revisions.failures = make(map[catalog.FileID]error)
	}
	if _, exists := revisions.opened[file]; !exists && revisions.failures[file] == nil {
		descriptor := jobDescriptor(t, share, file, 114, size)
		revisions.opened[file], _ = NewOpenedRevision(transferID[content.LeaseID](115), descriptor)
	}
	rules, _ := NewSelectionRules(true, nil)
	job, err := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog:   failingCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: snapshot}, failures: make(map[catalog.DirectoryID]error)},
		Revisions: revisions, Blocks: blocks, Output: output,
	})
	if err != nil {
		t.Fatal(err)
	}
	return job, file
}

func TestTransferJobValidationEmptySelectionAndFailureBranches(t *testing.T) {
	if _, err := NewOpenedRevision(content.LeaseID{}, content.FileRevisionDescriptor{}); !errors.Is(err, ErrRevisionIdentity) {
		t.Fatalf("invalid opened revision error=%v", err)
	}
	if _, err := NewTransferJob(TransferJobConfig{}); !errors.Is(err, ErrInvalidTransferJob) {
		t.Fatalf("invalid job error=%v", err)
	}
	share := transferID[catalog.ShareInstance](120)
	root := transferID[catalog.DirectoryID](121)
	emptyRules, _ := NewSelectionRules(false, nil)
	emptyOutput := newJobOutput(share)
	emptyJob, err := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: emptyRules,
		Catalog: failingCatalog{
			snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: jobSnapshot(t, share, root, 1)},
			failures:  make(map[catalog.DirectoryID]error),
		},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: emptyOutput,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result := emptyJob.Run(context.Background()); result.Outcome != JobSucceeded || result.Measure.Class() != SelectionSmall || emptyOutput.finished != JobSucceeded {
		t.Fatalf("empty result=%+v", result)
	}

	tests := []struct {
		name             string
		configure        func(*jobOutput, *jobRevisionClient, catalog.FileID)
		blocks           RangeReader
		wantOutcome      JobOutcome
		wantFailureStage FailureStage
	}{
		{name: "begin file", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.beginErr = NewOutputSessionError(errors.New("file create"), false)
		}, blocks: scriptedRangeReader{}, wantOutcome: JobCompletedWithErrors, wantFailureStage: FailureFileOutput},
		{name: "fatal begin", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.beginErr = NewOutputSessionError(errors.New("journal"), true)
		}, blocks: scriptedRangeReader{}, wantOutcome: JobPausedOutcome},
		{name: "canceled begin", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.beginErr = context.Canceled
		}, blocks: scriptedRangeReader{}, wantOutcome: JobPausedOutcome},
		{name: "nil transaction contract", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.nilTransaction = true
		}, blocks: scriptedRangeReader{}, wantOutcome: JobPausedOutcome},
		{name: "write", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.writeErr = errors.New("disk full")
		}, blocks: scriptedRangeReader{}, wantOutcome: JobPausedOutcome, wantFailureStage: FailureBlockTransfer},
		{name: "checkpoint", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.checkpointErr = errors.New("sync failed")
		}, blocks: scriptedRangeReader{}, wantOutcome: JobPausedOutcome, wantFailureStage: FailureFileOutput},
		{name: "fatal checkpoint cannot be skipped", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.checkpointErr = NewOutputSessionError(errors.New("backend lost"), true)
		}, blocks: scriptedRangeReader{}, wantOutcome: JobPausedOutcome},
		{name: "canceled checkpoint", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.checkpointErr = context.Canceled
		}, blocks: scriptedRangeReader{}, wantOutcome: JobPausedOutcome},
		{name: "checkpoint contract", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.omitCheckpoint = true
		}, blocks: scriptedRangeReader{}, wantOutcome: JobPausedOutcome},
		{name: "commit", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.commitErr = errors.New("publish failed")
		}, blocks: scriptedRangeReader{}, wantOutcome: JobPausedOutcome, wantFailureStage: FailureFileOutput},
		{name: "release", configure: func(_ *jobOutput, revisions *jobRevisionClient, _ catalog.FileID) {
			revisions.releaseErr = errors.New("release failed")
		}, blocks: scriptedRangeReader{}, wantOutcome: JobCompletedWithErrors, wantFailureStage: FailureLeaseRelease},
		{name: "canceled release", configure: func(_ *jobOutput, revisions *jobRevisionClient, _ catalog.FileID) {
			revisions.releaseErr = context.Canceled
		}, blocks: scriptedRangeReader{}, wantOutcome: JobPausedOutcome},
		{name: "finish", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.finishErr = errors.New("finalize failed")
		}, blocks: scriptedRangeReader{}, wantOutcome: JobPausedOutcome},
		{name: "invalid retire settlement", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.retireSettlement = FilePublished
		}, blocks: scriptedRangeReader{err: NewIsolatedPermanentSourceFailure(errors.New("block failed"))}, wantOutcome: JobPausedOutcome},
		{name: "unknown retire settlement", configure: func(output *jobOutput, _ *jobRevisionClient, _ catalog.FileID) {
			output.transactionScript.retireSettlement = FileSettlementKind(99)
		}, blocks: scriptedRangeReader{err: NewIsolatedPermanentSourceFailure(errors.New("block failed"))}, wantOutcome: JobPausedOutcome},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := newJobOutput(share)
			revisions := &jobRevisionClient{}
			job, file := branchJob(t, output, revisions, test.blocks)
			test.configure(output, revisions, file)
			result := job.Run(context.Background())
			if result.Outcome != test.wantOutcome {
				t.Fatalf("outcome=%v result=%+v", result.Outcome, result)
			}
			if test.wantFailureStage != 0 && (len(result.Files) == 0 || result.Files[0].Stage != test.wantFailureStage) {
				t.Fatalf("file failures=%+v", result.Files)
			}
		})
	}
}

func TestTransferJobRevisionIdentitySessionReleaseAndStreamSkipBranches(t *testing.T) {
	share := transferID[catalog.ShareInstance](130)
	output := newJobOutput(share)
	revisions := &jobRevisionClient{opened: make(map[catalog.FileID]OpenedRevision), failures: make(map[catalog.FileID]error)}
	job, selectedFile := branchJob(t, output, revisions, scriptedRangeReader{})
	wrongFile := transferID[catalog.FileID](131)
	wrongDescriptor := jobDescriptor(t, share, wrongFile, 132, uint64(catalog.MinChunkSize))
	revisions.opened[selectedFile], _ = NewOpenedRevision(transferID[content.LeaseID](133), wrongDescriptor)
	result := job.Run(context.Background())
	if result.Outcome != JobCompletedWithErrors || len(result.Files) != 1 || result.Files[0].Stage != FailureRevisionIdentity {
		t.Fatalf("identity result=%+v", result)
	}

	output = newJobOutput(share)
	revisions = &jobRevisionClient{releaseErr: NewSessionFailure(errors.New("session closed"))}
	job, _ = branchJob(t, output, revisions, scriptedRangeReader{})
	if result = job.Run(context.Background()); result.Outcome != JobPausedOutcome {
		t.Fatalf("session release result=%+v", result)
	}

	streamCapabilities, _ := NewOutputCapabilities(OutputCapabilities{Durability: DurabilityNone, Mode: OutputSingleFileStream})
	output = newJobOutput(share)
	output.capabilitiesOverride = &streamCapabilities
	revisions = &jobRevisionClient{}
	job, _ = branchJob(t, output, revisions, scriptedRangeReader{err: errors.New("file unavailable")})
	if result = job.Run(context.Background()); result.Outcome != JobPausedOutcome {
		t.Fatalf("unstarted stream result=%+v", result)
	}
}

func TestTransferJobDirectoryOutputAndCatalogIdentityBranches(t *testing.T) {
	share := transferID[catalog.ShareInstance](140)
	root := transferID[catalog.DirectoryID](141)
	child := transferID[catalog.DirectoryID](142)
	rootSnapshot := jobSnapshot(t, share, root, 143, jobDirectoryEntry(t, child, "child"))
	childSnapshot := jobSnapshot(t, share, child, 144)
	rules, _ := NewSelectionRules(true, nil)
	for _, test := range []struct {
		name        string
		ensureErr   error
		finalizeErr error
		want        JobOutcome
	}{
		{name: "ensure failure before session open", ensureErr: errors.New("mkdir denied"), want: JobPausedOutcome},
		{name: "finalize isolated", finalizeErr: errors.New("mtime denied"), want: JobCompletedWithErrors},
		{name: "ensure fatal", ensureErr: NewOutputSessionError(errors.New("backend lost"), true), want: JobPausedOutcome},
		{name: "ensure canceled", ensureErr: context.Canceled, want: JobPausedOutcome},
		{name: "finalize deadline", finalizeErr: context.DeadlineExceeded, want: JobPausedOutcome},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := newJobOutput(share)
			output.ensureErr, output.finalizeErr = test.ensureErr, test.finalizeErr
			job, _ := NewTransferJob(TransferJobConfig{
				ShareInstance: share, SyntheticRoot: root, Rules: rules,
				Catalog:   failingCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: rootSnapshot, child: childSnapshot}, failures: make(map[catalog.DirectoryID]error)},
				Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: output,
			})
			if result := job.Run(context.Background()); result.Outcome != test.want {
				t.Fatalf("result=%+v", result)
			}
		})
	}
	foreignSnapshot := jobSnapshot(t, share, child, 145)
	output := newJobOutput(share)
	job, _ := NewTransferJob(TransferJobConfig{
		ShareInstance: share, SyntheticRoot: root, Rules: rules,
		Catalog:   failingCatalog{snapshots: map[catalog.DirectoryID]catalog.DirectorySnapshot{root: foreignSnapshot}, failures: make(map[catalog.DirectoryID]error)},
		Revisions: &jobRevisionClient{}, Blocks: scriptedRangeReader{}, Output: output,
	})
	if result := job.Run(context.Background()); result.Outcome != JobPausedOutcome || result.TerminationCause == nil {
		t.Fatalf("foreign snapshot result=%+v", result)
	}
}
