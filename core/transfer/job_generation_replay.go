package transfer

import (
	"context"

	"github.com/windshare/windshare/core/catalog"
)

// replayGeneration turns terminal catalog evidence into bounded work. The
// first pass keeps only page commitments; matching each replay page before its
// entries are used prevents a second cursor from changing an admitted generation.
func (discovery *incrementalDirectoryDiscovery) replayGeneration(
	ctx context.Context,
	admission DirectoryAdmission,
) (bool, error) {
	if discovery.request.mode == incrementalDiscoveryOpaqueProbe {
		return discovery.replayOpaqueSelectionProbe(ctx)
	}
	selectedSubtree := discovery.request.selected || discovery.selection.DiscoveredFiles != 0
	for _, phase := range [...]generationReplayPhase{replayGenerationFiles, replayGenerationDirectories} {
		phaseSelected, err := discovery.replayGenerationPhase(ctx, admission, phase)
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
			ctx, DirectoryAdmission{}, replayGenerationDirectories,
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
	phase generationReplayPhase,
) (bool, error) {
	cursor, err := discovery.run.job.catalog.OpenDirectoryPages(ctx, discovery.request.directory)
	if err != nil {
		return false, discovery.handleReplayFailure(err)
	}
	if cursor == nil {
		return false, NewJobDependencyContractError(ErrCatalogCursorContract)
	}
	defer func() { _ = cursor.Close() }()

	selectedSubtree := false
	for index, commitment := range discovery.commitments {
		page, ok, err := cursor.Next(ctx)
		if err != nil {
			return false, discovery.handleReplayFailure(err)
		}
		terminal := index == len(discovery.commitments)-1
		if !ok || !discovery.matchesReplayPage(page, uint32(index), commitment, terminal) {
			return false, NewSessionFailure(ErrCatalogIdentity)
		}
		pageSelected, err := discovery.replayPage(ctx, page, admission, phase)
		if err != nil {
			return false, err
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
	phase generationReplayPhase,
) (bool, error) {
	selectedSubtree := false
	for entryIndex := 0; entryIndex < page.EntryCount(); entryIndex++ {
		if err := context.Cause(ctx); err != nil {
			return false, err
		}
		entry, exists := page.Entry(uint32(entryIndex))
		if !exists {
			return false, NewSessionFailure(ErrCatalogIdentity)
		}
		selected, err := discovery.replayEntry(ctx, entry, admission, phase)
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
	phase generationReplayPhase,
) (bool, error) {
	path, err := appendOutputPath(discovery.request.path, entry.Name())
	if err != nil {
		return false, NewSessionFailure(ErrCatalogIdentity)
	}
	if file, isFile := entry.FileID(); isFile {
		if phase != replayGenerationFiles {
			return false, nil
		}
		if !discovery.run.job.rules.FileSelectedAt(file, path, discovery.request.selected) {
			return false, nil
		}
		return true, discovery.enqueueReplayFile(ctx, entry, file, path, admission)
	}
	directory, isDirectory := entry.DirectoryID()
	if !isDirectory {
		return false, NewSessionFailure(ErrCatalogIdentity)
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
) error {
	if admission.IsZero() {
		return NewJobDependencyContractError(ErrDirectoryAdmissionMismatch)
	}
	plan := plannedFile{
		file: file, path: path, expectedSize: entry.ExpectedSize(), modified: entry.ModifiedTime(),
		parentDirectory: discovery.request.directory, parentGeneration: discovery.generation,
		parentAdmission: admission, selectionDecision: discovery.run.job.rules.selectedFileDecision(file, path),
	}
	enqueued := make(chan struct{})
	select {
	case discovery.queue <- transferQueueItem{kind: transferQueueFile, file: plan, enqueued: enqueued}:
		discovery.run.traceFileLifecycle(TransferFileEnqueued, plan, false)
		close(enqueued)
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (discovery *incrementalDirectoryDiscovery) handleReplayFailure(err error) error {
	recorded := discovery.run.recordDiscoveryFailure(
		discovery.request.directory, discovery.request.path, err,
	)
	if recorded != nil {
		if discovery.request.path == "" && !isJobTerminalError(recorded) {
			return NewSessionFailure(err)
		}
		return recorded
	}
	if discovery.request.path == "" {
		return NewSessionFailure(err)
	}
	// Authentication already committed the ledger claims. A replay-only branch
	// failure must not release identities that queued prefix work still owns.
	return nil
}
