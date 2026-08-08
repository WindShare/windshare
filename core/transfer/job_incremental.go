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
	run                        *jobRun
	queue                      chan<- transferQueueItem
	request                    incrementalDirectoryRequest
	checkpoint                 nodeLedgerCheckpoint
	generation                 catalog.DirectoryGeneration
	sequence                   catalog.DirectoryGenerationValidator
	pageIndex                  uint32
	terminal                   bool
	commitments                []catalog.PageCommitment
	selection                  SelectionMeasure
	descendantAdmission        bool
	descendantDiscoveryPending bool
	opaqueSelectionFound       bool
}

func (r *jobRun) isolateIncrementalFailure(
	checkpoint nodeLedgerCheckpoint,
	directory catalog.DirectoryID,
	path string,
	err error,
) error {
	if recordErr := r.recordDiscoveryFailure(directory, path, err); recordErr != nil {
		if directory == r.job.root && path == "" && !isJobTerminalError(recordErr) {
			return catalogIntegrityFailure(err)
		}
		return recordErr
	}
	if directory == r.job.root && path == "" {
		// The synthetic root is the session's catalog authority. Without an
		// authenticated terminal generation, no durable namespace may settle.
		r.rootGeneration = catalog.DirectoryGeneration{}
		if rollbackErr := r.rollbackClaims(checkpoint); rollbackErr != nil {
			return dependencyContractFailure(rollbackErr)
		}
		return catalogIntegrityFailure(err)
	}
	if rollbackErr := r.rollbackClaims(checkpoint); rollbackErr != nil {
		return dependencyContractFailure(rollbackErr)
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
		return false, dependencyContractFailure(ErrNodeLedgerState)
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
	workerErr chan<- *lifecycleFailure,
	done chan<- struct{},
) {
	defer close(done)
	for item := range queue {
		if err := context.Cause(ctx); err != nil {
			reportTransferWorkerFailure(workerErr, cancel, cancellationFailure(ctx, err))
			return
		}
		var err error
		switch item.kind {
		case transferQueueFile:
			select {
			case <-item.enqueued:
			case <-ctx.Done():
				reportTransferWorkerFailure(workerErr, cancel, cancellationFailure(ctx, context.Cause(ctx)))
				return
			}
			r.traceFileLifecycle(TransferFileStarted, item.file, nil)
			err = r.transferPlannedFile(ctx, item.file)
		case transferQueueDirectoryFinalization:
			err = r.finalizeIncrementalDirectory(ctx, item.admission)
		default:
			err = dependencyContractFailure(ErrOutputContract)
		}
		if err != nil {
			reportTransferWorkerFailure(workerErr, cancel, err)
			return
		}
	}
}

func reportTransferWorkerFailure(
	workerErr chan<- *lifecycleFailure,
	cancel context.CancelCauseFunc,
	err error,
) {
	failure := admitInternalFailure(err)
	if failure == nil {
		return
	}
	select {
	case workerErr <- failure:
	default:
	}
	cancel(failure)
}

func (r *jobRun) discoverIncrementalDirectory(
	ctx context.Context,
	queue chan<- transferQueueItem,
	request incrementalDirectoryRequest,
) (selected bool, resultErr error) {
	if err := context.Cause(ctx); err != nil {
		return false, cancellationFailure(ctx, err)
	}
	checkpoint := r.checkpointClaims()
	if _, active := r.activeDirectories[request.directory]; active {
		return false, catalogIntegrityFailure(ErrCatalogIdentity)
	}
	r.activeDirectories[request.directory] = struct{}{}
	defer delete(r.activeDirectories, request.directory)

	cursor, rawOpenErr := r.job.catalog.OpenDirectoryPages(ctx, request.directory)
	err := normalizeCatalogBoundary(ctx, rawOpenErr)
	if err != nil {
		return false, r.isolateIncrementalFailure(checkpoint, request.directory, request.path, err)
	}
	if cursor == nil {
		return false, dependencyContractFailure(ErrCatalogCursorContract)
	}
	defer func() {
		closeErr := normalizeCatalogBoundary(context.Background(), cursor.Close())
		if closeErr != nil {
			closeErr = r.isolateIncrementalFailure(
				checkpoint, request.directory, request.path, closeErr,
			)
		}
		resultErr = joinLifecycleFailures(resultErr, closeErr)
	}()

	discovery := incrementalDirectoryDiscovery{
		run: r, queue: queue, request: request, checkpoint: checkpoint,
	}
	usable, readErr := discovery.readTerminalGeneration(ctx, cursor)
	if readErr != nil || !usable {
		return false, readErr
	}
	if request.mode == incrementalDiscoveryOpaqueMaterialization &&
		!discovery.matchesExpectedEvidence() {
		return false, catalogIntegrityFailure(ErrCatalogIdentity)
	}
	if request.path == "" && request.mode != incrementalDiscoveryOpaqueProbe &&
		!discovery.descendantDiscoveryPending {
		// A terminal leaf generation has already yielded its exact selection
		// measure. Output replay may still be interrupted, but it cannot make that
		// authenticated catalog cut incomplete after the fact.
		r.catalogTraversalComplete = true
	}
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferGenerationCommitted, DirectoryID: request.directory,
		DirectoryGeneration: discovery.generation,
	})
	if request.mode == incrementalDiscoveryOpaqueProbe {
		return discovery.replayGeneration(ctx, DirectoryAdmission{})
	}
	r.job.tracker.addSelection(discovery.selection)
	admission, proceed, admissionErr := discovery.admitDirectory(ctx)
	if admissionErr != nil || !proceed {
		return false, admissionErr
	}
	selectedSubtree, replayErr := discovery.replayGeneration(ctx, admission)
	if replayErr != nil {
		return false, replayErr
	}
	if request.path == "" && request.mode != incrementalDiscoveryOpaqueProbe {
		r.catalogTraversalComplete = true
	}
	if !admission.IsZero() {
		select {
		case queue <- transferQueueItem{
			kind: transferQueueDirectoryFinalization, admission: admission,
		}:
		case <-ctx.Done():
			return false, cancellationFailure(ctx, context.Cause(ctx))
		}
	}
	return selectedSubtree, nil
}

func (discovery *incrementalDirectoryDiscovery) readTerminalGeneration(
	ctx context.Context,
	cursor catalog.DirectoryPageCursor,
) (bool, error) {
	for {
		page, ok, rawNextErr := cursor.Next(ctx)
		err := normalizeCatalogBoundary(ctx, rawNextErr)
		if err != nil {
			return false, discovery.run.isolateIncrementalFailure(
				discovery.checkpoint, discovery.request.directory, discovery.request.path, err,
			)
		}
		if !ok {
			return discovery.finishPages()
		}
		discarded, acceptErr := discovery.acceptPage(ctx, page)
		if acceptErr != nil || discarded {
			return false, acceptErr
		}
	}
}

func (discovery *incrementalDirectoryDiscovery) acceptPage(
	ctx context.Context,
	page catalog.CatalogPage,
) (bool, error) {
	if !discovery.matchesPage(page) {
		return false, catalogIntegrityFailure(ErrCatalogIdentity)
	}
	if err := discovery.sequence.AcceptPage(page); err != nil {
		return false, catalogIntegrityFailure(errors.Join(ErrCatalogIdentity, err))
	}
	if len(discovery.commitments) >= discovery.run.job.replayPageCapacity {
		return false, resourceBudgetFailure(ErrGenerationReplayBudget)
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
			return cancellationFailure(ctx, err)
		}
		entry, exists := page.Entry(uint32(entryIndex))
		if !exists {
			return catalogIntegrityFailure(ErrCatalogIdentity)
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
		return catalogIntegrityFailure(ErrCatalogIdentity)
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
		return catalogIntegrityFailure(ErrCatalogIdentity)
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
	discovery.descendantDiscoveryPending = discovery.descendantDiscoveryPending || discovery.childNeedsDiscovery(
		directory, path, selected,
	)
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

func (discovery *incrementalDirectoryDiscovery) childNeedsDiscovery(
	directory catalog.DirectoryID,
	path string,
	selected bool,
) bool {
	switch discovery.request.mode {
	case incrementalDiscoveryOpaqueProbe:
		return !selected
	case incrementalDiscoveryOpaqueMaterialization:
		if selected {
			return true
		}
		_, retained := discovery.run.opaqueEvidence(directory)
		return retained
	default:
		return discovery.run.job.rules.ShouldDiscoverDirectoryAt(directory, path, selected)
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
		return dependencyContractFailure(err)
	}
	if discovery.request.path == "" {
		return catalogIntegrityFailure(ErrCatalogEntriesOmitted)
	}
	return nil
}

func (discovery *incrementalDirectoryDiscovery) finishPages() (bool, error) {
	if _, err := discovery.sequence.Finish(); err == nil {
		return true, nil
	} else {
		err = catalogIntegrityFailure(errors.Join(ErrCatalogIdentity, err))
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
	policy := lifecyclePolicyFor(err)
	if discovery.request.path == "" {
		// Root admission establishes the parent authority for every later file.
		return DirectoryAdmission{}, false, requireOutputPause(err)
	}
	if policy.jobTerminal() || policy.outputRequiresJobPause(discovery.run.output.Capabilities()) {
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
	if err := validateOutputDirectoryForScope(r.directoryAdmissionScope, directory); err != nil {
		return DirectoryAdmission{}, dependencyContractFailure(err)
	}
	admission, rawAdmissionErr := r.output.AdmitDirectory(ctx, directory)
	err := normalizeOutputBoundary(ctx, rawAdmissionErr)
	if err != nil {
		if !admission.IsZero() {
			if bindingErr := ValidateDirectoryAdmissionBinding(
				r.directoryAdmissionScope, admission, directory,
			); bindingErr != nil {
				err = joinLifecycleFailures(err, outputContractFault(bindingErr))
			}
			err = requireOutputPause(err)
		}
		r.traceDirectoryAdmission(directory, err)
		return DirectoryAdmission{}, err
	}
	if err := ValidateDirectoryAdmissionBinding(r.directoryAdmissionScope, admission, directory); err != nil {
		contractFailure := outputContractFault(err)
		r.traceDirectoryAdmission(directory, contractFailure)
		return DirectoryAdmission{}, contractFailure
	}
	r.traceDirectoryAdmission(directory, nil)
	return admission, nil
}

func (r *jobRun) traceDirectoryAdmission(directory OutputDirectory, failure error) {
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferDirectoryAdmitted, OutputSessionID: r.output.SessionID(),
		DirectoryID: directory.DirectoryID, DirectoryGeneration: directory.Generation,
		Fault: closedFault(failure), Failed: failure != nil,
	})
}

func (r *jobRun) finalizeIncrementalDirectory(ctx context.Context, admission DirectoryAdmission) error {
	settlement, rawSettlementErr := r.output.FinalizeDirectory(ctx, admission)
	err := normalizeOutputBoundary(ctx, rawSettlementErr)
	invalidSettlement := false
	if err != nil && settlement != (DirectorySettlement{}) {
		err = joinLifecycleFailures(err, outputContractFault(ErrOutputContract))
		invalidSettlement = true
	}
	if err == nil {
		err = validateDirectorySettlement(admission, settlement)
		invalidSettlement = err != nil
	}
	if invalidSettlement {
		r.settlementFailure = mergeLifecycleFailures(r.settlementFailure, err)
	}
	traceFault := closedFault(err)
	if settlement.Kind() == DirectoryIsolatedFailure {
		traceFault, _ = settlement.IsolatedFault()
	}
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferDirectoryFinalized, OutputSessionID: r.output.SessionID(),
		DirectoryID: admission.DirectoryID(), DirectoryGeneration: admission.Generation(),
		Fault: traceFault, Failed: err != nil || settlement.Kind() == DirectoryIsolatedFailure,
	})
	if err != nil {
		// Only a terminal settlement may isolate metadata failure. An error means
		// the backend could not prove a stable cut, so no descendant may continue.
		return requireOutputPause(err)
	}
	if settlement.Kind() == DirectoryFinalized {
		return nil
	}
	isolatedFault, _ := settlement.IsolatedFault()
	r.recordDirectoryFailure(DirectoryJobFailure{
		DirectoryID: admission.DirectoryID(), Path: admission.Path(),
		Stage: FailureDirectoryOutput, Fault: isolatedFault,
	})
	return nil
}
