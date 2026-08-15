package transfer

import (
	"context"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
)

// replayGeneration turns terminal catalog evidence into bounded work. The
// first pass keeps only page commitments; matching each replay page before its
// entries are used prevents a second cursor from changing an admitted generation.
func (discovery *incrementalDirectoryDiscovery) replayGeneration(
	ctx context.Context,
	admission DirectoryAdmission,
	materialization MaterializedDirectoryClaim,
) (bool, error) {
	if discovery.request.mode == incrementalDiscoveryOpaqueProbe {
		return discovery.replayOpaqueSelectionProbe(ctx)
	}
	selectedSubtree := discovery.request.selected || discovery.selection.DiscoveredFiles != 0
	for _, phase := range [...]generationReplayPhase{replayGenerationFiles, replayGenerationDirectories} {
		phaseSelected, err := discovery.replayGenerationPhase(ctx, admission, materialization, phase)
		if err != nil {
			return false, err
		}
		selectedSubtree = selectedSubtree || phaseSelected
	}
	return selectedSubtree, nil
}

func (discovery *incrementalDirectoryDiscovery) replayOpaqueSelectionProbe(
	ctx context.Context,
) (bool, error) {
	selectedSubtree := discovery.opaqueSelectionFound
	if !discovery.run.allOpaqueSelectionTargetsMatched() {
		phaseSelected, err := discovery.replayGenerationPhase(
			ctx, DirectoryAdmission{}, MaterializedDirectoryClaim{}, replayGenerationDirectories,
		)
		if err != nil {
			return false, err
		}
		selectedSubtree = selectedSubtree || phaseSelected
	}
	// Root admission owns the output namespace even when a complete search
	// proves that every explicit target is absent. Descendant evidence remains
	// limited to branches that actually contain selected nodes.
	if !selectedSubtree && discovery.request.path != "" {
		return false, nil
	}
	evidence := opaqueSelectionEvidence{
		generation: discovery.generation,
		terminal:   discovery.commitments[len(discovery.commitments)-1],
	}
	if err := discovery.run.retainOpaqueSelectionEvidence(discovery.request.directory, evidence); err != nil {
		return false, err
	}
	return true, nil
}

type generationReplayPhase uint8

const (
	replayGenerationFiles generationReplayPhase = iota + 1
	replayGenerationDirectories
)

func (discovery *incrementalDirectoryDiscovery) replayGenerationPhase(
	ctx context.Context,
	admission DirectoryAdmission,
	materialization MaterializedDirectoryClaim,
	phase generationReplayPhase,
) (selected bool, resultErr error) {
	cursor, rawOpenErr := discovery.run.job.catalog.OpenDirectoryPages(ctx, discovery.request.directory)
	err := normalizeCatalogBoundary(ctx, rawOpenErr)
	if err != nil {
		return false, discovery.handleReplayFailure(err)
	}
	if cursor == nil {
		return false, dependencyContractFailure(ErrCatalogCursorContract)
	}
	defer func() {
		closeErr := normalizeCatalogBoundary(context.Background(), cursor.Close())
		if closeErr != nil {
			closeErr = discovery.handleReplayFailure(closeErr)
		}
		resultErr = joinLifecycleFailures(resultErr, closeErr)
	}()

	selectedSubtree := false
	for index, commitment := range discovery.commitments {
		page, ok, rawNextErr := cursor.Next(ctx)
		err := normalizeCatalogBoundary(ctx, rawNextErr)
		if err != nil {
			return false, discovery.handleReplayFailure(err)
		}
		terminal := index == len(discovery.commitments)-1
		if !ok || !discovery.matchesReplayPage(page, uint32(index), commitment, terminal) {
			return false, catalogIntegrityFailure(ErrCatalogIdentity)
		}
		pageSelected, replayErr := discovery.replayPage(ctx, page, admission, materialization, phase)
		if replayErr != nil {
			return false, replayErr
		}
		selectedSubtree = selectedSubtree || pageSelected
		if discovery.request.mode == incrementalDiscoveryOpaqueProbe &&
			discovery.run.allOpaqueSelectionTargetsMatched() {
			return selectedSubtree, nil
		}
	}
	return selectedSubtree, nil
}

func (discovery *incrementalDirectoryDiscovery) matchesReplayPage(
	page catalog.CatalogPage,
	pageIndex uint32,
	commitment catalog.PageCommitment,
	terminal bool,
) bool {
	return page.ShareInstance() == discovery.run.job.share &&
		page.DirectoryID() == discovery.request.directory &&
		page.Generation() == discovery.generation &&
		page.PageIndex() == pageIndex &&
		page.Commitment() == commitment &&
		page.Terminal() == terminal && page.OmittedCount() == 0
}

func (discovery *incrementalDirectoryDiscovery) replayPage(
	ctx context.Context,
	page catalog.CatalogPage,
	admission DirectoryAdmission,
	materialization MaterializedDirectoryClaim,
	phase generationReplayPhase,
) (bool, error) {
	selectedSubtree := false
	for entryIndex := 0; entryIndex < page.EntryCount(); entryIndex++ {
		if err := context.Cause(ctx); err != nil {
			return false, cancellationFailure(ctx, err)
		}
		entry, exists := page.Entry(uint32(entryIndex))
		if !exists {
			return false, catalogIntegrityFailure(ErrCatalogIdentity)
		}
		selected, err := discovery.replayEntry(ctx, entry, admission, materialization, phase)
		if err != nil {
			return false, err
		}
		selectedSubtree = selectedSubtree || selected
		if discovery.request.mode == incrementalDiscoveryOpaqueProbe &&
			discovery.run.allOpaqueSelectionTargetsMatched() {
			return selectedSubtree, nil
		}
	}
	return selectedSubtree, nil
}

func (discovery *incrementalDirectoryDiscovery) replayEntry(
	ctx context.Context,
	entry catalog.Entry,
	admission DirectoryAdmission,
	materialization MaterializedDirectoryClaim,
	phase generationReplayPhase,
) (bool, error) {
	path, err := appendOutputPath(discovery.request.path, entry.Name())
	if err != nil {
		return false, catalogIntegrityFailure(ErrCatalogIdentity)
	}
	if file, isFile := entry.FileID(); isFile {
		if phase != replayGenerationFiles {
			return false, nil
		}
		if !discovery.run.job.rules.FileSelectedAt(file, path, discovery.request.selected) {
			return false, nil
		}
		return true, discovery.enqueueReplayFile(ctx, entry, file, path, admission, materialization)
	}
	directory, isDirectory := entry.DirectoryID()
	if !isDirectory {
		return false, catalogIntegrityFailure(ErrCatalogIdentity)
	}
	if phase != replayGenerationDirectories {
		return false, nil
	}
	selected := discovery.run.job.rules.DirectorySelectedAt(
		directory, path, discovery.request.selected,
	)
	if !discovery.run.job.rules.ShouldDiscoverDirectoryAt(directory, path, selected) {
		return false, nil
	}
	request := incrementalDirectoryRequest{
		directory: directory, path: path, modified: entry.ModifiedTime(),
		selected: selected, parentAdmission: admission,
		parentMaterialization: materialization,
	}
	switch discovery.request.mode {
	case incrementalDiscoveryOpaqueProbe:
		if selected {
			return true, nil
		}
		request.mode = incrementalDiscoveryOpaqueProbe
		request.parentAdmission = DirectoryAdmission{}
		return discovery.run.discoverIncrementalDirectory(ctx, discovery.queue, request)
	case incrementalDiscoveryOpaqueMaterialization:
		if selected {
			request.mode = incrementalDiscoveryStandard
			return discovery.run.discoverIncrementalDirectory(ctx, discovery.queue, request)
		}
		evidence, retained := discovery.run.opaqueEvidence(directory)
		if !retained {
			return false, nil
		}
		request.mode = incrementalDiscoveryOpaqueMaterialization
		request.expectedEvidence = evidence
		return discovery.run.discoverIncrementalDirectory(ctx, discovery.queue, request)
	default:
		if !selected && discovery.run.job.rules.requiresOpaqueSelectionSearch() {
			return discovery.run.discoverOpaqueSelectionBranch(ctx, discovery.queue, request)
		}
		return discovery.run.discoverIncrementalDirectory(ctx, discovery.queue, request)
	}
}

func (discovery *incrementalDirectoryDiscovery) enqueueReplayFile(
	ctx context.Context,
	entry catalog.Entry,
	file catalog.FileID,
	path string,
	admission DirectoryAdmission,
	parentMaterialization MaterializedDirectoryClaim,
) error {
	if admission.IsZero() {
		return dependencyContractFailure(ErrDirectoryAdmissionMismatch)
	}
	sourcePath, err := ordinaryoutput.NewSourceCatalogPath(path)
	if err != nil {
		return dependencyContractFailure(err)
	}
	node, err := OrdinaryOutputSourceNode(
		catalog.NodeKindFile, catalog.DirectoryID{}, file, sourcePath, ordinaryoutput.SourceNodeSelected,
	)
	if err != nil {
		return dependencyContractFailure(err)
	}
	projection := discovery.run.job.projector.Project(node)
	artifactPath, materialized := projection.ArtifactPath()
	if !materialized {
		if projection.Kind() == ordinaryoutput.ArtifactReject {
			return discovery.run.recordSelectedProjectionRejection(projection)
		}
		return dependencyContractFailure(ErrOutputContract)
	}
	plan := plannedFile{
		file: file, sourcePath: sourcePath, artifactPath: artifactPath,
		expectedSize: entry.ExpectedSize(), modified: entry.ModifiedTime(),
		parentDirectory: discovery.request.directory, parentGeneration: discovery.generation,
		parentAdmission: admission, parentMaterialization: parentMaterialization,
		selectionDecision: discovery.run.job.rules.selectedFileDecision(file, path),
	}
	enqueued := make(chan struct{})
	select {
	case discovery.queue <- transferQueueItem{kind: transferQueueFile, file: plan, enqueued: enqueued}:
		discovery.run.traceFileLifecycle(TransferFileEnqueued, plan, nil)
		close(enqueued)
		return nil
	case <-ctx.Done():
		return cancellationFailure(ctx, context.Cause(ctx))
	}
}

func (discovery *incrementalDirectoryDiscovery) handleReplayFailure(err error) error {
	recorded := discovery.run.recordDiscoveryFailure(
		discovery.request.directory, discovery.request.path, err,
	)
	if recorded != nil {
		if discovery.request.path == "" && !isJobTerminalError(recorded) {
			return catalogIntegrityFailure(err)
		}
		return recorded
	}
	if discovery.request.path == "" {
		return catalogIntegrityFailure(err)
	}
	// Authentication already committed the ledger claims. A replay-only branch
	// failure must not release identities that queued prefix work still owns.
	return nil
}
