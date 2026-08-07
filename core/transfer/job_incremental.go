package transfer

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
)

type incrementalDirectoryRequest struct {
	directory        catalog.DirectoryID
	path             string
	modified         catalog.ModifiedTime
	selected         bool
	parentAdmission  DirectoryAdmission
	mode             incrementalDiscoveryMode
	expectedEvidence opaqueSelectionEvidence
}

type incrementalDiscoveryMode uint8

const (
	incrementalDiscoveryStandard incrementalDiscoveryMode = iota
	incrementalDiscoveryOpaqueProbe
	incrementalDiscoveryOpaqueMaterialization
)

type incrementalDirectoryDiscovery struct {
	run                  *jobRun
	queue                chan<- transferQueueItem
	request              incrementalDirectoryRequest
	checkpoint           nodeLedgerCheckpoint
	generation           catalog.DirectoryGeneration
	sequence             catalog.DirectoryGenerationValidator
	pageIndex            uint32
	terminal             bool
	commitments          []catalog.PageCommitment
	selection            SelectionMeasure
	descendantAdmission  bool
	opaqueSelectionFound bool
}

func (r *jobRun) isolateIncrementalFailure(
	checkpoint nodeLedgerCheckpoint,
	directory catalog.DirectoryID,
	path string,
	err error,
) error {
	if recordErr := r.recordDiscoveryFailure(directory, path, err); recordErr != nil {
		if directory == r.job.root && path == "" && !isJobTerminalError(recordErr) {
			return NewSessionFailure(err)
		}
		return recordErr
	}
	if directory == r.job.root && path == "" {
		// The synthetic root is the session's catalog authority. Without an
		// authenticated terminal generation, no durable namespace may settle.
		r.rootGeneration = catalog.DirectoryGeneration{}
		if rollbackErr := r.rollbackClaims(checkpoint); rollbackErr != nil {
			return NewJobDependencyContractError(rollbackErr)
		}
		return NewSessionFailure(err)
	}
	if rollbackErr := r.rollbackClaims(checkpoint); rollbackErr != nil {
		return NewJobDependencyContractError(rollbackErr)
	}
	return nil
}

func (r *jobRun) discoverIncremental(ctx context.Context, queue chan<- transferQueueItem) error {
	rootSelected := r.job.rules.DirectorySelectedAt(r.job.root, "", r.job.rules.DefaultSelected())
	request := incrementalDirectoryRequest{directory: r.job.root, selected: rootSelected}
	if !rootSelected && r.job.rules.requiresOpaqueSelectionSearch() {
		_, err := r.discoverOpaqueSelectionBranch(ctx, queue, request)
		return err
	}
	_, err := r.discoverIncrementalDirectory(ctx, queue, incrementalDirectoryRequest{
		directory: r.job.root,
		selected:  rootSelected,
	})
	return err
}

func (r *jobRun) discoverOpaqueSelectionBranch(
	ctx context.Context,
	queue chan<- transferQueueItem,
	request incrementalDirectoryRequest,
) (bool, error) {
	request.mode = incrementalDiscoveryOpaqueProbe
	request.expectedEvidence = opaqueSelectionEvidence{}
	selected, err := r.discoverIncrementalDirectory(ctx, queue, request)
	materialize := selected || request.path == ""
	if err != nil || !materialize {
		return selected, err
	}
	evidence, retained := r.opaqueEvidence(request.directory)
	if !retained {
		return false, NewJobDependencyContractError(ErrNodeLedgerState)
	}
	request.mode = incrementalDiscoveryOpaqueMaterialization
	request.expectedEvidence = evidence
	_, err = r.discoverIncrementalDirectory(ctx, queue, request)
	return true, err
}

func (r *jobRun) transferQueueWorker(
	ctx context.Context,
	queue <-chan transferQueueItem,
	cancel context.CancelCauseFunc,
	workerErr chan<- error,
	done chan<- struct{},
) {
	defer close(done)
	for item := range queue {
		if err := context.Cause(ctx); err != nil {
			reportTransferWorkerFailure(workerErr, cancel, err)
			return
		}
		var err error
		switch item.kind {
		case transferQueueFile:
			select {
			case <-item.enqueued:
			case <-ctx.Done():
				reportTransferWorkerFailure(workerErr, cancel, context.Cause(ctx))
				return
			}
			r.traceFileLifecycle(TransferFileStarted, item.file, false)
			err = r.transferPlannedFile(ctx, item.file)
		case transferQueueDirectoryFinalization:
			err = r.finalizeIncrementalDirectory(ctx, item.directory)
		default:
			err = NewJobDependencyContractError(ErrOutputContract)
		}
		if err != nil {
			reportTransferWorkerFailure(workerErr, cancel, err)
			return
		}
	}
}

func reportTransferWorkerFailure(
	workerErr chan<- error,
	cancel context.CancelCauseFunc,
	err error,
) {
	select {
	case workerErr <- err:
	default:
	}
	cancel(err)
}

func (r *jobRun) discoverIncrementalDirectory(
	ctx context.Context,
	queue chan<- transferQueueItem,
	request incrementalDirectoryRequest,
) (bool, error) {
	if err := context.Cause(ctx); err != nil {
		return false, err
	}
	checkpoint := r.checkpointClaims()
	if _, active := r.activeDirectories[request.directory]; active {
		return false, NewSessionFailure(ErrCatalogIdentity)
	}
	r.activeDirectories[request.directory] = struct{}{}
	defer delete(r.activeDirectories, request.directory)

	cursor, err := r.job.catalog.OpenDirectoryPages(ctx, request.directory)
	if err != nil {
		return false, r.isolateIncrementalFailure(checkpoint, request.directory, request.path, err)
	}
	if cursor == nil {
		return false, NewJobDependencyContractError(ErrCatalogCursorContract)
	}
	defer func() { _ = cursor.Close() }()

	discovery := incrementalDirectoryDiscovery{
		run: r, queue: queue, request: request, checkpoint: checkpoint,
	}
	usable, err := discovery.readTerminalGeneration(ctx, cursor)
	_ = cursor.Close()
	if err != nil || !usable {
		return false, err
	}
	if request.mode == incrementalDiscoveryOpaqueMaterialization &&
		!discovery.matchesExpectedEvidence() {
		return false, NewSessionFailure(ErrCatalogIdentity)
	}
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferGenerationCommitted, DirectoryID: request.directory,
		DirectoryGeneration: discovery.generation,
	})
	if request.mode == incrementalDiscoveryOpaqueProbe {
		return discovery.replayGeneration(ctx, DirectoryAdmission{})
	}
	r.job.tracker.addSelection(discovery.selection)
	admission, proceed, err := discovery.admitDirectory(ctx)
	if err != nil || !proceed {
		return false, err
	}
	selectedSubtree, err := discovery.replayGeneration(ctx, admission)
	if err != nil {
		return false, err
	}
	if !admission.IsZero() {
		select {
		case queue <- transferQueueItem{
			kind: transferQueueDirectoryFinalization, directory: discovery.outputDirectory(),
		}:
		case <-ctx.Done():
			return false, context.Cause(ctx)
		}
	}
	return selectedSubtree, nil
}

func (discovery *incrementalDirectoryDiscovery) readTerminalGeneration(
	ctx context.Context,
	cursor catalog.DirectoryPageCursor,
) (bool, error) {
	for {
		page, ok, err := cursor.Next(ctx)
		if err != nil {
			return false, discovery.run.isolateIncrementalFailure(
				discovery.checkpoint, discovery.request.directory, discovery.request.path, err,
			)
		}
		if !ok {
			return discovery.finishPages()
		}
		discarded, err := discovery.acceptPage(ctx, page)
		if err != nil || discarded {
			return false, err
		}
	}
}

func (discovery *incrementalDirectoryDiscovery) acceptPage(
	ctx context.Context,
	page catalog.CatalogPage,
) (bool, error) {
	if !discovery.matchesPage(page) {
		return false, NewSessionFailure(ErrCatalogIdentity)
	}
	if err := discovery.sequence.AcceptPage(page); err != nil {
		return false, NewSessionFailure(errors.Join(ErrCatalogIdentity, err))
	}
	if len(discovery.commitments) >= discovery.run.job.replayPageCapacity {
		return false, NewJobResourceBudgetError(ErrGenerationReplayBudget)
	}
	if discovery.pageIndex == 0 {
		discovery.beginGeneration(page.Generation())
	}
	if err := discovery.acceptEntries(ctx, page); err != nil {
		return false, err
	}
	discovery.terminal = page.Terminal()
	discovery.commitments = append(discovery.commitments, page.Commitment())
	discovery.pageIndex++
	if discovery.terminal && page.OmittedCount() != 0 {
		return true, discovery.rejectOmittedGeneration()
	}
	return false, nil
}

func (discovery *incrementalDirectoryDiscovery) matchesPage(page catalog.CatalogPage) bool {
	return !discovery.terminal &&
		page.ShareInstance() == discovery.run.job.share &&
		page.DirectoryID() == discovery.request.directory &&
		page.PageIndex() == discovery.pageIndex &&
		!page.Generation().IsZero() &&
		(discovery.pageIndex == 0 || page.Generation() == discovery.generation)
}

func (discovery *incrementalDirectoryDiscovery) beginGeneration(generation catalog.DirectoryGeneration) {
	discovery.generation = generation
	if discovery.request.path == "" {
		discovery.run.rootGeneration = generation
	}
	// The synthetic root has no catalog entry of its own. Authentication of its
	// first page is therefore the point at which an explicit root target matches.
	if discovery.request.mode != incrementalDiscoveryOpaqueMaterialization &&
		discovery.run.job.rules.isSelectedDirectoryTarget(discovery.request.directory) {
		discovery.run.matchSelectedDirectory(discovery.request.directory)
	}
}

func (discovery *incrementalDirectoryDiscovery) acceptEntries(
	ctx context.Context,
	page catalog.CatalogPage,
) error {
	for entryIndex := 0; entryIndex < page.EntryCount(); entryIndex++ {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		entry, exists := page.Entry(uint32(entryIndex))
		if !exists {
			return NewSessionFailure(ErrCatalogIdentity)
		}
		if err := discovery.acceptEntry(entry); err != nil {
			return err
		}
	}
	return nil
}

func (discovery *incrementalDirectoryDiscovery) acceptEntry(entry catalog.Entry) error {
	entryPath, err := appendOutputPath(discovery.request.path, entry.Name())
	if err != nil {
		return NewSessionFailure(ErrCatalogIdentity)
	}
	if discovery.request.mode != incrementalDiscoveryOpaqueMaterialization {
		if err := discovery.run.claimNode(entry.NodeID()); err != nil {
			return err
		}
	}
	if discovery.request.mode != incrementalDiscoveryOpaqueMaterialization &&
		discovery.run.job.rules.isPathTarget(entryPath) {
		discovery.run.matchedPaths[entryPath] = struct{}{}
	}
	if fileID, isFile := entry.FileID(); isFile {
		discovery.acceptFile(entry, fileID, entryPath)
		return nil
	}
	child, isDirectory := entry.DirectoryID()
	if !isDirectory {
		return NewSessionFailure(ErrCatalogIdentity)
	}
	discovery.acceptDirectory(child, entryPath)
	return nil
}

func (discovery *incrementalDirectoryDiscovery) acceptFile(
	entry catalog.Entry,
	fileID catalog.FileID,
	path string,
) {
	if discovery.request.mode != incrementalDiscoveryOpaqueMaterialization &&
		discovery.run.job.rules.isSelectedFileTarget(fileID) {
		discovery.run.matchSelectedFile(fileID)
	}
	selected := discovery.run.job.rules.FileSelectedAt(fileID, path, discovery.request.selected)
	if discovery.request.mode == incrementalDiscoveryOpaqueProbe {
		discovery.opaqueSelectionFound = discovery.opaqueSelectionFound || selected
		return
	}
	if !selected {
		return
	}
	discovery.selection.addDiscoveredFile(entry.ExpectedSize())
}

func (discovery *incrementalDirectoryDiscovery) acceptDirectory(
	directory catalog.DirectoryID,
	path string,
) {
	if discovery.request.mode != incrementalDiscoveryOpaqueMaterialization &&
		discovery.run.job.rules.isSelectedDirectoryTarget(directory) {
		discovery.run.matchSelectedDirectory(directory)
	}
	selected := discovery.run.job.rules.DirectorySelectedAt(directory, path, discovery.request.selected)
	if discovery.request.mode == incrementalDiscoveryOpaqueProbe {
		discovery.opaqueSelectionFound = discovery.opaqueSelectionFound || selected
		return
	}
	if !discovery.run.job.rules.ShouldDiscoverDirectoryAt(directory, path, selected) {
		return
	}
	_, hasOpaqueEvidence := discovery.run.opaqueEvidence(directory)
	if selected || discovery.run.job.rules.hasPathDescendant(path) ||
		discovery.request.mode == incrementalDiscoveryOpaqueMaterialization && hasOpaqueEvidence {
		discovery.descendantAdmission = true
	}
}

func (discovery *incrementalDirectoryDiscovery) rejectOmittedGeneration() error {
	discovery.run.job.tracker.failDiscovery()
	discovery.run.discoveryFailed = true
	discovery.run.recordDirectoryFailure(DirectoryJobFailure{
		DirectoryID: discovery.request.directory,
		Path:        discovery.request.path,
		Stage:       FailureDirectoryDiscovery,
		Cause:       ErrCatalogEntriesOmitted,
	})
	if discovery.request.path == "" {
		// Omitted root children invalidate the authority for every descendant.
		discovery.run.rootGeneration = catalog.DirectoryGeneration{}
	}
	if err := discovery.run.rollbackClaims(discovery.checkpoint); err != nil {
		return NewJobDependencyContractError(err)
	}
	if discovery.request.path == "" {
		return NewSessionFailure(ErrCatalogEntriesOmitted)
	}
	return nil
}

func (discovery *incrementalDirectoryDiscovery) finishPages() (bool, error) {
	if _, err := discovery.sequence.Finish(); err == nil {
		return true, nil
	} else {
		err = NewSessionFailure(errors.Join(ErrCatalogIdentity, err))
		return false, discovery.run.isolateIncrementalFailure(
			discovery.checkpoint, discovery.request.directory, discovery.request.path, err,
		)
	}
}

func (discovery *incrementalDirectoryDiscovery) needsAdmission() bool {
	if discovery.request.mode == incrementalDiscoveryOpaqueProbe {
		return false
	}
	if discovery.request.mode == incrementalDiscoveryOpaqueMaterialization {
		return true
	}
	return discovery.request.path == "" || discovery.request.selected ||
		discovery.selection.DiscoveredFiles != 0 || discovery.descendantAdmission
}

func (discovery *incrementalDirectoryDiscovery) matchesExpectedEvidence() bool {
	if len(discovery.commitments) == 0 {
		return false
	}
	return discovery.request.expectedEvidence == (opaqueSelectionEvidence{
		generation: discovery.generation,
		terminal:   discovery.commitments[len(discovery.commitments)-1],
	})
}

func (discovery *incrementalDirectoryDiscovery) admitDirectory(
	ctx context.Context,
) (DirectoryAdmission, bool, error) {
	if !discovery.needsAdmission() {
		return DirectoryAdmission{}, true, nil
	}
	directory := discovery.outputDirectory()
	admission, err := discovery.run.admitIncrementalDirectory(ctx, directory)
	if err == nil {
		return admission, true, nil
	}
	if discovery.request.path == "" {
		// Root admission establishes the parent authority for every later file.
		return DirectoryAdmission{}, false, NewOutputSessionError(err, true)
	}
	inspection := inspectLifecycleError(err)
	if inspection.directoryAdmissionMismatch {
		// A foreign generation receipt is a backend contract breach, not a branch
		// failure that can safely be isolated.
		err = outputContractFault(err)
	}
	if inspection.jobTerminal() || inspection.outputRequiresJobPause(discovery.run.output.Capabilities()) {
		return DirectoryAdmission{}, false, err
	}
	discovery.run.recordIncrementalAdmissionFailure(
		discovery.request.directory, discovery.request.path, err,
	)
	return DirectoryAdmission{}, false, nil
}

func (discovery *incrementalDirectoryDiscovery) outputDirectory() OutputDirectory {
	return OutputDirectory{
		DirectoryID: discovery.request.directory, Generation: discovery.generation,
		ParentAdmission: discovery.request.parentAdmission,
		Path:            discovery.request.path, ModifiedTime: discovery.request.modified,
	}
}

func (r *jobRun) recordIncrementalAdmissionFailure(
	directory catalog.DirectoryID,
	path string,
	err error,
) {
	r.discoveryFailed = true
	r.job.tracker.failDiscovery()
	r.recordDirectoryFailure(DirectoryJobFailure{
		DirectoryID: directory,
		Path:        path,
		Stage:       FailureDirectoryOutput,
		Cause:       err,
	})
}

func (r *jobRun) admitIncrementalDirectory(ctx context.Context, directory OutputDirectory) (DirectoryAdmission, error) {
	if err := validateOutputDirectory(directory); err != nil {
		return DirectoryAdmission{}, err
	}
	admission, err := r.output.AdmitDirectory(ctx, directory)
	if err != nil {
		r.traceDirectoryAdmission(directory, true)
		return DirectoryAdmission{}, err
	}
	if ValidateDirectoryAdmissionBinding(admission, directory) != nil {
		r.traceDirectoryAdmission(directory, true)
		return DirectoryAdmission{}, outputContractFault(nil)
	}
	r.traceDirectoryAdmission(directory, false)
	return admission, nil
}

func (r *jobRun) traceDirectoryAdmission(directory OutputDirectory, failed bool) {
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferDirectoryAdmitted, OutputSessionID: r.output.SessionID(),
		DirectoryID: directory.DirectoryID, DirectoryGeneration: directory.Generation, Failed: failed,
	})
}

func (r *jobRun) finalizeIncrementalDirectory(ctx context.Context, directory OutputDirectory) error {
	err := r.output.FinalizeDirectory(ctx, directory)
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferDirectoryFinalized, OutputSessionID: r.output.SessionID(),
		DirectoryID: directory.DirectoryID, DirectoryGeneration: directory.Generation, Failed: err != nil,
	})
	if err == nil {
		return nil
	}
	if directory.Path == "" {
		// Finalizing root closes the output authority, so it can never be
		// downgraded to an isolated metadata failure.
		return NewOutputSessionError(err, true)
	}
	inspection := inspectLifecycleError(err)
	if inspection.jobTerminal() || inspection.outputRequiresJobPause(r.output.Capabilities()) {
		return err
	}
	r.recordDirectoryFailure(DirectoryJobFailure{
		DirectoryID: directory.DirectoryID, Path: directory.Path,
		Stage: FailureDirectoryOutput, Cause: err,
	})
	return nil
}
