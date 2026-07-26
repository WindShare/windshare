package transfer

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/windshare/windshare/core/catalog"
)

type plannedDirectory struct {
	directory  catalog.DirectoryID
	generation catalog.DirectoryGeneration
	path       string
	modified   catalog.ModifiedTime
	selected   bool
}

type plannedFile struct {
	file             catalog.FileID
	path             string
	entry            catalog.Entry
	parentDirectory  catalog.DirectoryID
	parentGeneration catalog.DirectoryGeneration
}

type jobRun struct {
	job                 *TransferJob
	output              OutputSession
	directories         []DirectoryJobFailure
	files               []FileJobFailure
	succeeded           uint64
	terminationCause    error
	settlementFailure   error
	settlement          JobSettlement
	admitted            bool
	needsAttention      bool
	selectionIdentity   SelectionIdentity
	resumeIntent        ResumeIntent
	claims              *selectionIdentityClaims
	matchedPaths        map[string]struct{}
	matchedDirectories  map[catalog.DirectoryID]struct{}
	matchedFiles        map[catalog.FileID]struct{}
	discoveryFailed     bool
	rootGeneration      catalog.DirectoryGeneration
	plannedDirectories  []plannedDirectory
	plannedFiles        []plannedFile
	requiredDirectories []plannedDirectory
}

func newJobRun(job *TransferJob) *jobRun {
	return &jobRun{
		job: job, claims: newSelectionIdentityClaims(job.root),
		matchedPaths:       make(map[string]struct{}),
		matchedDirectories: make(map[catalog.DirectoryID]struct{}),
		matchedFiles:       make(map[catalog.FileID]struct{}),
	}
}

func (r *jobRun) discoverSelection(ctx context.Context) (OutputSelection, bool, error) {
	rootSelected := r.job.rules.DirectorySelectedAt(r.job.root, "", r.job.rules.DefaultSelected())
	if err := r.discoverDirectory(ctx, r.job.root, "", catalog.ModifiedTime{}, rootSelected); err != nil {
		return OutputSelection{}, false, err
	}
	if r.discoveryFailed || r.rootGeneration.IsZero() {
		return OutputSelection{}, false, nil
	}
	if err := r.job.rules.missingTargetsError(
		r.matchedPaths, r.matchedDirectories, r.matchedFiles,
	); err != nil {
		return OutputSelection{}, false, err
	}
	selection, err := r.buildOutputSelection()
	return selection, err == nil, err
}

func (r *jobRun) discoverDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
	path string,
	modified catalog.ModifiedTime,
	selected bool,
) error {
	snapshot, release, err := r.job.catalog.AcquireDirectory(ctx, directory)
	if release == nil {
		return NewJobDependencyContractError(ErrCatalogLeaseContract)
	}
	release()
	if err != nil {
		return r.recordDiscoveryFailure(directory, path, err)
	}
	if snapshot.ShareInstance() != r.job.share || snapshot.DirectoryID() != directory ||
		snapshot.Generation().IsZero() || snapshot.PageCount() == 0 {
		return NewSessionFailure(ErrCatalogIdentity)
	}
	if path == "" {
		r.rootGeneration = snapshot.Generation()
	} else {
		r.plannedDirectories = append(r.plannedDirectories, plannedDirectory{
			directory: directory, generation: snapshot.Generation(), path: path,
			modified: modified, selected: selected,
		})
	}
	if r.job.rules.isSelectedDirectoryTarget(directory) {
		r.matchedDirectories[directory] = struct{}{}
	}
	if snapshot.OmittedCount() != 0 {
		r.job.tracker.failDiscovery()
		r.discoveryFailed = true
		r.directories = append(r.directories, DirectoryJobFailure{
			DirectoryID: directory, Path: path, Stage: FailureDirectoryDiscovery,
			Cause: ErrCatalogEntriesOmitted,
		})
	}
	if err := r.claimSnapshotEntries(ctx, snapshot, path); err != nil {
		return err
	}
	if err := r.collectSelectedFiles(ctx, snapshot, path, selected); err != nil {
		return err
	}
	return r.discoverChildDirectories(ctx, snapshot, path, selected)
}

func (r *jobRun) claimSnapshotEntries(
	ctx context.Context,
	snapshot catalog.DirectorySnapshot,
	parentPath string,
) error {
	return visitSnapshotEntries(ctx, snapshot, func(entry catalog.Entry) error {
		path, err := appendOutputPath(parentPath, entry.Name())
		if err != nil {
			return NewSessionFailure(ErrCatalogIdentity)
		}
		if err := r.claims.claim(entry.NodeID()); err != nil {
			return err
		}
		if r.job.rules.isPathTarget(path) {
			r.matchedPaths[path] = struct{}{}
		}
		if directory, ok := entry.DirectoryID(); ok && r.job.rules.isSelectedDirectoryTarget(directory) {
			r.matchedDirectories[directory] = struct{}{}
		}
		if file, ok := entry.FileID(); ok && r.job.rules.isSelectedFileTarget(file) {
			r.matchedFiles[file] = struct{}{}
		}
		return nil
	})
}

func (r *jobRun) collectSelectedFiles(
	ctx context.Context,
	snapshot catalog.DirectorySnapshot,
	parentPath string,
	selected bool,
) error {
	return visitSnapshotEntries(ctx, snapshot, func(entry catalog.Entry) error {
		file, isFile := entry.FileID()
		if !isFile {
			return nil
		}
		path, err := appendOutputPath(parentPath, entry.Name())
		if err != nil {
			return NewSessionFailure(ErrCatalogIdentity)
		}
		if !r.job.rules.FileSelectedAt(file, path, selected) {
			return nil
		}
		r.job.tracker.addFile(entry.ExpectedSize())
		r.plannedFiles = append(r.plannedFiles, plannedFile{
			file: file, path: path, entry: entry,
			parentDirectory: snapshot.DirectoryID(), parentGeneration: snapshot.Generation(),
		})
		return nil
	})
}

func (r *jobRun) discoverChildDirectories(
	ctx context.Context,
	snapshot catalog.DirectorySnapshot,
	parentPath string,
	inherited bool,
) error {
	return visitSnapshotEntries(ctx, snapshot, func(entry catalog.Entry) error {
		child, isDirectory := entry.DirectoryID()
		if !isDirectory {
			return nil
		}
		path, err := appendOutputPath(parentPath, entry.Name())
		if err != nil {
			return NewSessionFailure(ErrCatalogIdentity)
		}
		selected := r.job.rules.DirectorySelectedAt(child, path, inherited)
		if !r.job.rules.ShouldDiscoverDirectoryAt(child, path, selected) {
			return nil
		}
		return r.discoverDirectory(ctx, child, path, entry.ModifiedTime(), selected)
	})
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

func isDirectoryDiscoveryFailure(err error) bool {
	var failure DirectoryDiscoveryFailure
	return errors.As(err, &failure)
}

func (r *jobRun) buildOutputSelection() (OutputSelection, error) {
	required := make(map[string]struct{})
	for _, directory := range r.plannedDirectories {
		if directory.selected {
			markSelectionAncestors(required, directory.path)
		}
	}
	for _, file := range r.plannedFiles {
		markSelectionAncestors(required, selectionParentPath(file.path))
	}

	r.requiredDirectories = r.requiredDirectories[:0]
	directories := make([]OutputSelectionDirectory, 0, len(required))
	for _, directory := range r.plannedDirectories {
		if _, needed := required[directory.path]; !needed {
			continue
		}
		r.requiredDirectories = append(r.requiredDirectories, directory)
		directories = append(directories, OutputSelectionDirectory{
			Path: directory.path, DirectoryID: directory.directory, Generation: directory.generation,
			ModifiedTime: directory.modified,
		})
	}
	files := make([]OutputSelectionFile, 0, len(r.plannedFiles))
	for _, file := range r.plannedFiles {
		files = append(files, OutputSelectionFile{
			Path: file.path, FileID: file.file, ParentDirectoryID: file.parentDirectory,
			ParentGeneration: file.parentGeneration, ExpectedSize: file.entry.ExpectedSize(),
			ModifiedTime: file.entry.ModifiedTime(),
		})
	}
	sort.Slice(r.requiredDirectories, func(left, right int) bool {
		return r.requiredDirectories[left].path < r.requiredDirectories[right].path
	})
	sort.Slice(r.plannedFiles, func(left, right int) bool {
		return r.plannedFiles[left].path < r.plannedFiles[right].path
	})
	selection, err := NewOutputSelection(r.job.share, r.job.root, r.rootGeneration, directories, files)
	if err != nil {
		return OutputSelection{}, err
	}
	canonical, err := NewCanonicalSelectionV1(r.job.selectionRequest, selection)
	if err != nil {
		return OutputSelection{}, err
	}
	return canonical.BindPlan(selection)
}

func markSelectionAncestors(required map[string]struct{}, path string) {
	for path != "" {
		required[path] = struct{}{}
		path = selectionParentPath(path)
	}
}

func (r *jobRun) finalizeSelectionDirectories(ctx context.Context) error {
	for index := len(r.requiredDirectories) - 1; index >= 0; index-- {
		directory := r.requiredDirectories[index]
		err := r.output.FinalizeDirectory(ctx, OutputDirectory{
			Path: directory.path, ModifiedTime: directory.modified,
		})
		if err == nil {
			continue
		}
		if isJobTerminalError(err) || outputFailureRequiresJobPause(err, r.output.Capabilities()) {
			return err
		}
		r.directories = append(r.directories, DirectoryJobFailure{
			DirectoryID: directory.directory, Path: directory.path,
			Stage: FailureDirectoryOutput, Cause: err,
		})
	}
	return nil
}

func visitSnapshotEntries(
	ctx context.Context,
	snapshot catalog.DirectorySnapshot,
	visit func(catalog.Entry) error,
) error {
	for pageIndex := 0; pageIndex < snapshot.PageCount(); pageIndex++ {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		page, ok := snapshot.Page(uint32(pageIndex))
		if !ok {
			return NewSessionFailure(ErrCatalogIdentity)
		}
		for entryIndex := 0; entryIndex < page.EntryCount(); entryIndex++ {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			entry, ok := page.Entry(uint32(entryIndex))
			if !ok {
				return NewSessionFailure(ErrCatalogIdentity)
			}
			if err := visit(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

func appendOutputPath(parent, name string) (string, error) {
	path := name
	if parent != "" {
		path = strings.Join([]string{parent, name}, "/")
	}
	return catalog.CanonicalPath(path)
}
