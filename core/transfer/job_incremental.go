package transfer

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/catalogwalk"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
)

type incrementalDirectoryRequest struct {
	directory             catalog.DirectoryID
	path                  string
	modified              catalog.ModifiedTime
	selected              bool
	parentAdmission       DirectoryAdmission
	parentMaterialization MaterializedDirectoryClaim
	mode                  incrementalDiscoveryMode
	expectedEvidence      opaqueSelectionEvidence
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
	commitments                []catalog.PageCommitment
	selection                  discoveredSelection
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
	discovery := incrementalDirectoryDiscovery{
		run: r, queue: queue, request: request, checkpoint: checkpoint,
		selection: newDiscoveredSelection(),
	}
	meter, ok := catalogwalk.NewMeter(r.job.catalogWalkLimits)
	if !ok {
		return false, dependencyContractFailure(ErrGenerationReplayBudget)
	}
	usable, readErr := discovery.readTerminalGeneration(ctx, cursor, meter)
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
	if request.mode == incrementalDiscoveryOpaqueProbe {
		r.job.trace(TransferLifecycleTrace{Stage: TransferGenerationCommitted})
		return discovery.replayGeneration(ctx, DirectoryAdmission{}, MaterializedDirectoryClaim{})
	}
	r.job.progress.addDiscovery(discovery.selection)
	r.job.trace(TransferLifecycleTrace{Stage: TransferGenerationCommitted})
	admission, materialization, proceed, admissionErr := discovery.admitDirectory(ctx)
	if admissionErr != nil || !proceed {
		return false, admissionErr
	}
	selectedSubtree, replayErr := discovery.replayGeneration(ctx, admission, materialization)
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
	meter *catalogwalk.Meter,
) (bool, error) {
	result, err := catalogwalk.ReadTerminalGeneration(
		ctx,
		cursor,
		discovery.run.job.share,
		discovery.request.directory,
		meter,
		discovery.acceptEntry,
	)
	if err != nil {
		failure := normalizeCatalogBoundary(ctx, err)
		switch {
		case errors.Is(err, catalogwalk.ErrTerminalGenerationIntegrity):
			failure = catalogIntegrityFailure(errors.Join(ErrCatalogIdentity, err))
		case errors.Is(err, catalogwalk.ErrInvalidTerminalGenerationWalk):
			failure = dependencyContractFailure(err)
		}
		return false, discovery.run.isolateIncrementalFailure(
			discovery.checkpoint, discovery.request.directory, discovery.request.path, failure,
		)
	}
	if result.Exhausted.Valid() {
		failure := resourceBudgetFailure(ErrGenerationReplayBudget)
		return false, discovery.run.isolateIncrementalFailure(
			discovery.checkpoint, discovery.request.directory, discovery.request.path, failure,
		)
	}
	if !result.Complete {
		return false, discovery.rejectOmittedGeneration()
	}
	discovery.generation = result.Directory.Generation()
	discovery.commitments = result.PageCommitments()
	if len(discovery.commitments) == 0 {
		failure := catalogIntegrityFailure(ErrCatalogIdentity)
		return false, discovery.run.isolateIncrementalFailure(
			discovery.checkpoint, discovery.request.directory, discovery.request.path, failure,
		)
	}
	discovery.beginGeneration(discovery.generation)
	return true, nil
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
	discovery.selection.addFile(entry.ExpectedSize())
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
	discovery.run.job.progress.failDiscovery()
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

func (discovery *incrementalDirectoryDiscovery) needsAdmission() bool {
	if discovery.request.mode == incrementalDiscoveryOpaqueProbe {
		return false
	}
	if discovery.request.mode == incrementalDiscoveryOpaqueMaterialization {
		return true
	}
	return discovery.request.path == "" || discovery.request.selected ||
		discovery.selection.files != 0 || discovery.descendantAdmission
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
) (DirectoryAdmission, MaterializedDirectoryClaim, bool, error) {
	if !discovery.needsAdmission() {
		return DirectoryAdmission{}, MaterializedDirectoryClaim{}, true, nil
	}
	request, projection, projectionErr := discovery.outputDirectoryRequest()
	if projectionErr != nil {
		return DirectoryAdmission{}, MaterializedDirectoryClaim{}, false, dependencyContractFailure(projectionErr)
	}
	// Projection is the admission discriminant: a rejected source deliberately
	// carries no materialization request, but that absence never grants reference authority.
	_, materialized := request.Directory()
	switch projection.Kind() {
	case ordinaryoutput.ArtifactReject:
		return DirectoryAdmission{}, MaterializedDirectoryClaim{}, false,
			discovery.run.recordSelectedProjectionRejection(projection)
	case ordinaryoutput.ArtifactTraverseOnly:
		if !materialized {
			return DirectoryAdmission{}, MaterializedDirectoryClaim{}, true, nil
		}
	case ordinaryoutput.ArtifactMaterialize:
		if !materialized {
			return DirectoryAdmission{}, MaterializedDirectoryClaim{}, false,
				dependencyContractFailure(ErrOutputContract)
		}
	default:
		return DirectoryAdmission{}, MaterializedDirectoryClaim{}, false,
			dependencyContractFailure(ErrOutputContract)
	}
	admission, err := discovery.run.admitIncrementalDirectory(ctx, request)
	if err == nil {
		if _, projectedArtifact := request.Projection().ArtifactPath(); !projectedArtifact {
			return admission, MaterializedDirectoryClaim{}, true, nil
		}
		claim, claimErr := NewMaterializedDirectoryClaim(admission, request)
		if claimErr != nil {
			return DirectoryAdmission{}, MaterializedDirectoryClaim{}, false, dependencyContractFailure(claimErr)
		}
		return admission, claim, true, nil
	}
	policy := lifecyclePolicyFor(err)
	if discovery.request.path == "" {
		// Root admission establishes the parent authority for every later file.
		return DirectoryAdmission{}, MaterializedDirectoryClaim{}, false, requireOutputPause(err)
	}
	if policy.jobTerminal() || policy.outputRequiresJobPause(discovery.run.output.Capabilities()) {
		return DirectoryAdmission{}, MaterializedDirectoryClaim{}, false, err
	}
	discovery.run.recordIncrementalAdmissionFailure(
		discovery.request.directory, projectedFailurePath(request.Projection()), err,
	)
	return DirectoryAdmission{}, MaterializedDirectoryClaim{}, false, nil
}

func (discovery *incrementalDirectoryDiscovery) outputDirectoryRequest() (
	DirectoryMaterializationRequest,
	ordinaryoutput.ArtifactPathProjection,
	error,
) {
	sourcePath, err := sourceCatalogPath(discovery.request.path)
	if err != nil {
		return DirectoryMaterializationRequest{}, ordinaryoutput.ArtifactPathProjection{}, err
	}
	role := ordinaryoutput.SourceNodeConnectsSelection
	if discovery.request.path != "" && discovery.request.selected {
		role = ordinaryoutput.SourceNodeSelected
	}
	return projectDirectoryMaterializationRequest(
		discovery.run.job.coordinates,
		AuthenticatedSourceDirectory{
			DirectoryID: discovery.request.directory, Generation: discovery.generation,
			ParentAdmission: discovery.request.parentAdmission,
			SourcePath:      sourcePath, ModifiedTime: discovery.request.modified,
		},
		role,
		discovery.request.parentMaterialization,
	)
}

// A selected node rejected for identity, kind, or frozen ancestry is a source
// generation drift, not an unrelated branch to ignore. It intentionally creates
// no item failure because a rejected node has no authorized artifact coordinate.
func (r *jobRun) recordSelectedProjectionRejection(
	projection ordinaryoutput.ArtifactPathProjection,
) error {
	if projection.Kind() != ordinaryoutput.ArtifactReject {
		return dependencyContractFailure(ErrOutputContract)
	}
	switch projection.RejectReason() {
	case ordinaryoutput.ArtifactRejectWrongKind,
		ordinaryoutput.ArtifactRejectWrongIdentity,
		ordinaryoutput.ArtifactRejectUnrelatedSource:
		failure := catalogDirectoryFailure(fault.CatalogDirectoryStale, ErrFrozenSourceDrift)
		r.failureMu.Lock()
		r.retainSourceDriftFailure(failure)
		r.discoveryFault = fault.Join(r.discoveryFault, failure.policy.value)
		r.failureMu.Unlock()
		r.discoveryFailed = true
		r.job.progress.failDiscovery()
		return nil
	default:
		return dependencyContractFailure(ErrOutputContract)
	}
}

func projectedFailurePath(projection ordinaryoutput.ArtifactPathProjection) string {
	path, materialized := projection.ArtifactPath()
	if !materialized {
		return ""
	}
	return path.String()
}

func (r *jobRun) recordIncrementalAdmissionFailure(
	directory catalog.DirectoryID,
	path string,
	err error,
) {
	r.discoveryFailed = true
	r.job.progress.failDiscovery()
	r.recordDirectoryFailure(DirectoryJobFailure{
		DirectoryID: directory,
		Path:        path,
		Stage:       FailureDirectoryOutput,
		Cause:       err,
	})
}

func (r *jobRun) admitIncrementalDirectory(ctx context.Context, request DirectoryMaterializationRequest) (DirectoryAdmission, error) {
	directory, materialized := request.Directory()
	if !materialized {
		return DirectoryAdmission{}, dependencyContractFailure(ErrInvalidDirectoryAdmission)
	}
	if err := validateMaterializationDirectoryForScope(r.directoryAdmissionScope, directory); err != nil {
		return DirectoryAdmission{}, dependencyContractFailure(err)
	}
	admission, rawAdmissionErr := r.output.AdmitDirectory(ctx, request)
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
		r.traceDirectoryAdmission(request, err)
		return DirectoryAdmission{}, err
	}
	if err := ValidateDirectoryAdmissionBinding(r.directoryAdmissionScope, admission, directory); err != nil {
		contractFailure := outputContractFault(err)
		r.traceDirectoryAdmission(request, contractFailure)
		return DirectoryAdmission{}, contractFailure
	}
	r.traceDirectoryAdmission(request, nil)
	return admission, nil
}

func (r *jobRun) traceDirectoryAdmission(_ DirectoryMaterializationRequest, failure error) {
	r.job.trace(TransferLifecycleTrace{
		Stage: TransferDirectoryAdmitted, OutputSessionID: r.output.SessionID(),
		Fault:        closedFault(failure),
		Interruption: closedInterruption(failure),
		Failed:       failure != nil,
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
		Fault:        traceFault,
		Interruption: closedInterruption(err),
		Failed:       err != nil || settlement.Kind() == DirectoryIsolatedFailure,
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
		DirectoryID: admission.DirectoryID(), Path: r.artifactPathForAdmission(admission),
		Stage: FailureDirectoryOutput, Fault: isolatedFault,
	})
	return nil
}

func (r *jobRun) artifactPathForAdmission(admission DirectoryAdmission) string {
	if admission.IsZero() {
		return ""
	}
	return admission.Path()
}
