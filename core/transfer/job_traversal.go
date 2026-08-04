package transfer

import (
	"context"
	"errors"
	"strings"

	"github.com/windshare/windshare/core/catalog"
)

type plannedDirectory struct {
	directory  catalog.DirectoryID
	generation catalog.DirectoryGeneration
	path       string
	modified   catalog.ModifiedTime
}

type plannedFile struct {
	file             catalog.FileID
	path             string
	expectedSize     uint64
	modified         catalog.ModifiedTime
	parentDirectory  catalog.DirectoryID
	parentGeneration catalog.DirectoryGeneration
}

type jobRun struct {
	job                *TransferJob
	output             OutputSession
	directories        []DirectoryJobFailure
	files              []FileJobFailure
	succeeded          uint64
	terminationCause   error
	settlementFailure  error
	settlement         JobSettlement
	admitted           bool
	needsAttention     bool
	selectionIdentity  SelectionIdentity
	resumeIntent       ResumeIntent
	matchedPaths       map[string]struct{}
	matchedDirectories map[catalog.DirectoryID]struct{}
	matchedFiles       map[catalog.FileID]struct{}
	activeDirectories  map[catalog.DirectoryID]struct{}
	discoveryFailed    bool
	rootGeneration     catalog.DirectoryGeneration
	spool              *selectionSpool
}

func newJobRun(job *TransferJob) (*jobRun, error) {
	spool, err := newSelectionSpool(job.share)
	if err != nil {
		return nil, err
	}
	if err := spool.claim(job.root.NodeID()); err != nil {
		_ = spool.Close()
		return nil, err
	}
	return &jobRun{
		job: job, spool: spool,
		matchedPaths:       make(map[string]struct{}),
		matchedDirectories: make(map[catalog.DirectoryID]struct{}),
		matchedFiles:       make(map[catalog.FileID]struct{}),
		activeDirectories:  make(map[catalog.DirectoryID]struct{}),
	}, nil
}

func (r *jobRun) close() {
	if r.spool != nil {
		_ = r.spool.Close()
	}
}

func (r *jobRun) discoverSelection(ctx context.Context) (OutputSelection, bool, error) {
	rootSelected := r.job.rules.DirectorySelectedAt(r.job.root, "", r.job.rules.DefaultSelected())
	if _, err := r.discoverDirectory(ctx, r.job.root, "", catalog.ModifiedTime{}, rootSelected); err != nil {
		return OutputSelection{}, false, err
	}
	if r.rootGeneration.IsZero() {
		return OutputSelection{}, false, nil
	}
	// A failed authenticated subtree is terminal discovery with an isolated
	// failure, but it cannot prove that an explicit target inside that subtree is
	// missing. Failure-free walks still reject every unmatched explicit target.
	if !r.discoveryFailed {
		if err := r.job.rules.missingTargetsError(
			r.matchedPaths, r.matchedDirectories, r.matchedFiles,
		); err != nil {
			return OutputSelection{}, false, err
		}
	}
	if err := r.spool.Freeze(ctx); err != nil {
		return OutputSelection{}, false, err
	}
	selection, err := newOutputSelectionFromPlan(
		r.job.share, r.job.root, r.rootGeneration, r.spool,
	)
	if err != nil {
		return OutputSelection{}, false, err
	}
	canonical, err := NewCanonicalSelectionV1(r.job.selectionRequest, selection)
	if err != nil {
		return OutputSelection{}, false, err
	}
	selection, err = canonical.BindPlan(selection)
	return selection, err == nil, err
}

func (r *jobRun) discoverDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
	path string,
	modified catalog.ModifiedTime,
	selected bool,
) (selectedSubtree bool, resultErr error) {
	checkpoint, err := r.spool.checkpoint()
	if err != nil {
		return false, err
	}
	if _, active := r.activeDirectories[directory]; active {
		return false, NewSessionFailure(ErrCatalogIdentity)
	}
	r.activeDirectories[directory] = struct{}{}
	defer delete(r.activeDirectories, directory)

	cursor, err := r.job.catalog.OpenDirectoryPages(ctx, directory)
	if err != nil {
		return false, r.isolateDiscoveryFailure(checkpoint, directory, path, err)
	}
	if cursor == nil {
		return false, NewJobDependencyContractError(ErrCatalogCursorContract)
	}
	defer func() { resultErr = errors.Join(resultErr, cursor.Close()) }()

	var generation catalog.DirectoryGeneration
	var reference selectionDirectoryReference
	var haveDirectoryReference bool
	var pageIndex uint32
	var terminal bool
	for {
		page, ok, err := cursor.Next(ctx)
		if err != nil {
			return false, r.isolateDiscoveryFailure(checkpoint, directory, path, err)
		}
		if !ok {
			break
		}
		if terminal || page.ShareInstance() != r.job.share || page.DirectoryID() != directory ||
			page.PageIndex() != pageIndex || page.Generation().IsZero() ||
			(pageIndex > 0 && page.Generation() != generation) {
			return false, NewSessionFailure(ErrCatalogIdentity)
		}
		if pageIndex == 0 {
			generation = page.Generation()
			if path == "" {
				r.rootGeneration = generation
			} else {
				reference, err = r.spool.appendDirectory(plannedDirectory{
					directory: directory, generation: generation, path: path, modified: modified,
				})
				if err != nil {
					return false, err
				}
				haveDirectoryReference = true
			}
			if r.job.rules.isSelectedDirectoryTarget(directory) {
				r.matchedDirectories[directory] = struct{}{}
			}
		}
		for entryIndex := 0; entryIndex < page.EntryCount(); entryIndex++ {
			if cause := context.Cause(ctx); cause != nil {
				return false, cause
			}
			entry, ok := page.Entry(uint32(entryIndex))
			if !ok {
				return false, NewSessionFailure(ErrCatalogIdentity)
			}
			entrySelected, err := r.discoverEntry(ctx, entry, directory, generation, path, selected)
			if err != nil {
				return false, err
			}
			selectedSubtree = selectedSubtree || entrySelected
		}
		if page.Terminal() && page.OmittedCount() != 0 {
			r.job.tracker.failDiscovery()
			r.discoveryFailed = true
			r.directories = append(r.directories, DirectoryJobFailure{
				DirectoryID: directory, Path: path, Stage: FailureDirectoryDiscovery,
				Cause: ErrCatalogEntriesOmitted,
			})
		}
		terminal = page.Terminal()
		pageIndex++
	}
	if pageIndex == 0 || !terminal {
		return false, NewSessionFailure(ErrCatalogIdentity)
	}
	selectedSubtree = selectedSubtree || selected
	if haveDirectoryReference && selectedSubtree {
		if err := r.spool.requireDirectory(reference); err != nil {
			return false, err
		}
	}
	return selectedSubtree, nil
}

func (r *jobRun) discoverEntry(
	ctx context.Context,
	entry catalog.Entry,
	parentDirectory catalog.DirectoryID,
	parentGeneration catalog.DirectoryGeneration,
	parentPath string,
	inherited bool,
) (bool, error) {
	path, err := appendOutputPath(parentPath, entry.Name())
	if err != nil {
		return false, NewSessionFailure(ErrCatalogIdentity)
	}
	if err := r.spool.claim(entry.NodeID()); err != nil {
		return false, err
	}
	if r.job.rules.isPathTarget(path) {
		r.matchedPaths[path] = struct{}{}
	}
	if file, ok := entry.FileID(); ok {
		if r.job.rules.isSelectedFileTarget(file) {
			r.matchedFiles[file] = struct{}{}
		}
		if !r.job.rules.FileSelectedAt(file, path, inherited) {
			return false, nil
		}
		r.job.tracker.addFile(entry.ExpectedSize())
		if err := r.spool.appendFile(plannedFile{
			file: file, path: path, expectedSize: entry.ExpectedSize(), modified: entry.ModifiedTime(),
			parentDirectory: parentDirectory, parentGeneration: parentGeneration,
		}); err != nil {
			return false, err
		}
		return true, nil
	}
	child, ok := entry.DirectoryID()
	if !ok {
		return false, NewSessionFailure(ErrCatalogIdentity)
	}
	if r.job.rules.isSelectedDirectoryTarget(child) {
		r.matchedDirectories[child] = struct{}{}
	}
	selected := r.job.rules.DirectorySelectedAt(child, path, inherited)
	if !r.job.rules.ShouldDiscoverDirectoryAt(child, path, selected) {
		return false, nil
	}
	return r.discoverDirectory(ctx, child, path, entry.ModifiedTime(), selected)
}

func (r *jobRun) recordDiscoveryFailure(directory catalog.DirectoryID, path string, err error) error {
	if isJobTerminalError(err) || !isDirectoryDiscoveryFailure(err) {
		return err
	}
	r.job.tracker.failDiscovery()
	r.discoveryFailed = true
	r.directories = append(r.directories, DirectoryJobFailure{
		DirectoryID: directory, Path: path, Stage: FailureDirectoryDiscovery, Cause: err,
	})
	return nil
}

func (r *jobRun) isolateDiscoveryFailure(
	checkpoint selectionSpoolCheckpoint,
	directory catalog.DirectoryID,
	path string,
	err error,
) error {
	if err := r.recordDiscoveryFailure(directory, path, err); err != nil {
		return err
	}
	if directory == r.job.root && path == "" {
		// The root generation does not exist as terminal authority until its final
		// page arrives. A recoverable prefix failure may be reported, but it cannot
		// open an output namespace for an unauthenticated root snapshot.
		r.rootGeneration = catalog.DirectoryGeneration{}
	}
	// A terminal page is the commit evidence for a generation. Rolling the DFS
	// suffix back preserves independent siblings without admitting authenticated
	// prefixes whose selected parent generation never reached that evidence.
	return r.spool.rollback(checkpoint)
}

func isDirectoryDiscoveryFailure(err error) bool {
	var failure DirectoryDiscoveryFailure
	return errors.As(err, &failure)
}

func (r *jobRun) finalizeSelectionDirectories(ctx context.Context) error {
	return r.spool.VisitDirectoriesReverse(func(directory plannedDirectory) error {
		err := r.output.FinalizeDirectory(ctx, OutputDirectory{
			Path: directory.path, ModifiedTime: directory.modified,
		})
		if err == nil {
			return nil
		}
		if isJobTerminalError(err) || outputFailureRequiresJobPause(err, r.output.Capabilities()) {
			return err
		}
		r.directories = append(r.directories, DirectoryJobFailure{
			DirectoryID: directory.directory, Path: directory.path,
			Stage: FailureDirectoryOutput, Cause: err,
		})
		return nil
	})
}

func appendOutputPath(parent, name string) (string, error) {
	path := name
	if parent != "" {
		path = strings.Join([]string{parent, name}, "/")
	}
	return catalog.CanonicalPath(path)
}
