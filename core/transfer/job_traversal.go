package transfer

import (
	"context"
	"errors"
	"strings"

	"github.com/windshare/windshare/core/catalog"
)

type jobRun struct {
	job                *TransferJob
	directories        []DirectoryJobFailure
	files              []FileJobFailure
	succeeded          uint64
	abortCause         error
	claims             *selectionIdentityClaims
	matchedPaths       map[string]struct{}
	matchedDirectories map[catalog.DirectoryID]struct{}
	matchedFiles       map[catalog.FileID]struct{}
	discoveryFailed    bool
}

// directoryOutputFrame decouples authenticated discovery from output side
// effects. Discovery-only ancestors stay virtual until successful selected
// content proves that their output path is needed.
type directoryOutputFrame struct {
	parent    *directoryOutputFrame
	directory catalog.DirectoryID
	path      string
	modified  catalog.ModifiedTime
	root      bool

	ensureAttempted bool
	available       bool
	ensured         bool
	ancestorBlocked bool
}

func newRootOutputFrame() *directoryOutputFrame {
	return &directoryOutputFrame{root: true, ensureAttempted: true, available: true}
}

func (frame *directoryOutputFrame) child(
	directory catalog.DirectoryID,
	path string,
	modified catalog.ModifiedTime,
) *directoryOutputFrame {
	return &directoryOutputFrame{
		parent: frame, directory: directory, path: path, modified: modified,
		ancestorBlocked: frame.blocked(),
	}
}

func (frame *directoryOutputFrame) ensure(ctx context.Context, run *jobRun) (bool, error) {
	if frame.ensureAttempted {
		return frame.available, nil
	}
	parentAvailable, err := frame.parent.ensure(ctx, run)
	if err != nil {
		return false, err
	}
	if !parentAvailable {
		frame.ensureAttempted = true
		return false, nil
	}
	available, ensured, err := run.ensureChildDirectory(
		ctx, frame.directory, frame.path, frame.modified, true,
	)
	if err != nil {
		return false, err
	}
	frame.ensureAttempted = true
	frame.available = available
	frame.ensured = ensured
	return available, nil
}

func (frame *directoryOutputFrame) blocked() bool {
	return frame.ancestorBlocked || (frame.ensureAttempted && !frame.available)
}

func (frame *directoryOutputFrame) finalize(ctx context.Context, run *jobRun) error {
	if frame.root || !frame.ensured {
		return nil
	}
	return run.finalizeChildDirectory(ctx, frame.directory, frame.path, frame.modified, true)
}

func (r *jobRun) walkDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
	frame *directoryOutputFrame,
	selected bool,
) error {
	snapshot, release, err := r.job.catalog.AcquireDirectory(ctx, directory)
	if release == nil {
		return NewJobDependencyContractError(ErrCatalogLeaseContract)
	}
	release()
	if err != nil {
		return r.recordDiscoveryFailure(directory, frame.path, err)
	}
	if snapshot.ShareInstance() != r.job.share || snapshot.DirectoryID() != directory || snapshot.PageCount() == 0 {
		return NewSessionFailure(ErrCatalogIdentity)
	}
	if r.job.rules.isSelectedDirectoryTarget(directory) {
		r.matchedDirectories[directory] = struct{}{}
	}
	if snapshot.OmittedCount() != 0 {
		r.job.tracker.failDiscovery()
		r.discoveryFailed = true
		r.directories = append(r.directories, DirectoryJobFailure{
			DirectoryID: directory, Path: frame.path, Stage: FailureDirectoryDiscovery, Err: ErrCatalogEntriesOmitted,
		})
	}
	// A committed snapshot is scanned without cloning its O(width) entries.
	// Claiming and measuring the whole generation before output preserves
	// fail-closed identity validation and an exact direct-file measure even when
	// the first selected file later aborts.
	if err := r.claimSnapshotEntries(ctx, snapshot, frame.path); err != nil {
		return err
	}
	if err := r.measureDirectoryFiles(ctx, snapshot, frame.path, selected); err != nil {
		return err
	}
	if selected {
		if _, err := frame.ensure(ctx, r); err != nil {
			return err
		}
	}
	if err := r.transferDirectoryFiles(ctx, snapshot, frame, selected); err != nil {
		return err
	}
	return r.walkChildDirectories(ctx, snapshot, frame, selected)
}

func (r *jobRun) claimSnapshotEntries(
	ctx context.Context,
	snapshot catalog.DirectorySnapshot,
	parentPath string,
) error {
	return visitSnapshotEntries(ctx, snapshot, func(entry catalog.Entry) error {
		if _, err := appendOutputPath(parentPath, entry.Name()); err != nil {
			return NewSessionFailure(ErrCatalogIdentity)
		}
		if err := r.claims.claim(entry.NodeID()); err != nil {
			return err
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

func (r *jobRun) recordDiscoveryFailure(directory catalog.DirectoryID, path string, err error) error {
	if isJobTerminalError(err) || !isDirectoryDiscoveryFailure(err) {
		return err
	}
	r.job.tracker.failDiscovery()
	r.discoveryFailed = true
	r.directories = append(r.directories, DirectoryJobFailure{
		DirectoryID: directory, Path: path, Stage: FailureDirectoryDiscovery, Err: err,
	})
	return nil
}

func (r *jobRun) measureDirectoryFiles(
	ctx context.Context,
	snapshot catalog.DirectorySnapshot,
	path string,
	selected bool,
) error {
	return visitSnapshotEntries(ctx, snapshot, func(entry catalog.Entry) error {
		file, isFile := entry.FileID()
		if !isFile {
			return nil
		}
		filePath, err := appendOutputPath(path, entry.Name())
		if err != nil {
			return NewSessionFailure(ErrCatalogIdentity)
		}
		if r.job.rules.isPathTarget(filePath) {
			r.matchedPaths[filePath] = struct{}{}
		}
		if r.job.rules.FileSelectedAt(file, filePath, selected) {
			r.job.tracker.addFile(entry.ExpectedSize())
		}
		return nil
	})
}

func isDirectoryDiscoveryFailure(err error) bool {
	var failure DirectoryDiscoveryFailure
	return errors.As(err, &failure)
}

func (r *jobRun) transferDirectoryFiles(
	ctx context.Context,
	snapshot catalog.DirectorySnapshot,
	frame *directoryOutputFrame,
	selected bool,
) error {
	return visitSnapshotEntries(ctx, snapshot, func(entry catalog.Entry) error {
		file, isFile := entry.FileID()
		if !isFile {
			return nil
		}
		path, err := appendOutputPath(frame.path, entry.Name())
		if err != nil {
			return NewSessionFailure(ErrCatalogIdentity)
		}
		if !r.job.rules.FileSelectedAt(file, path, selected) || frame.blocked() {
			return nil
		}
		return r.transferFile(ctx, frame, path, entry)
	})
}

func (r *jobRun) walkChildDirectories(
	ctx context.Context,
	snapshot catalog.DirectorySnapshot,
	frame *directoryOutputFrame,
	selected bool,
) error {
	return visitSnapshotEntries(ctx, snapshot, func(entry catalog.Entry) error {
		return r.walkChildDirectory(ctx, entry, frame, selected)
	})
}

func (r *jobRun) walkChildDirectory(
	ctx context.Context,
	entry catalog.Entry,
	parent *directoryOutputFrame,
	inherited bool,
) error {
	child, isDirectory := entry.DirectoryID()
	if !isDirectory {
		return nil
	}
	path, err := appendOutputPath(parent.path, entry.Name())
	if err != nil {
		return NewSessionFailure(ErrCatalogIdentity)
	}
	if r.job.rules.isPathTarget(path) {
		r.matchedPaths[path] = struct{}{}
	}
	selected := r.job.rules.DirectorySelectedAt(child, path, inherited)
	if !r.job.rules.ShouldDiscoverDirectoryAt(child, path, selected) {
		return nil
	}
	frame := parent.child(child, path, entry.ModifiedTime())
	if err := r.walkDirectory(ctx, child, frame, selected); err != nil {
		return err
	}
	return frame.finalize(ctx, r)
}

func (r *jobRun) ensureChildDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
	path string,
	modified catalog.ModifiedTime,
	outputAvailable bool,
) (bool, bool, error) {
	if !outputAvailable {
		return false, false, nil
	}
	err := r.job.output.EnsureDirectory(ctx, OutputDirectory{Path: path, ModifiedTime: modified})
	if err == nil {
		return true, true, nil
	}
	if isJobTerminalError(err) || outputFailureRequiresJobAbort(err, r.job.output.Capabilities()) {
		return false, false, err
	}
	r.directories = append(r.directories, DirectoryJobFailure{
		DirectoryID: directory, Path: path, Stage: FailureDirectoryOutput, Err: err,
	})
	return false, false, nil
}

func (r *jobRun) finalizeChildDirectory(
	ctx context.Context,
	directory catalog.DirectoryID,
	path string,
	modified catalog.ModifiedTime,
	ensured bool,
) error {
	if !ensured {
		return nil
	}
	err := r.job.output.FinalizeDirectory(ctx, OutputDirectory{Path: path, ModifiedTime: modified})
	if err == nil {
		return nil
	}
	if isJobTerminalError(err) || outputFailureRequiresJobAbort(err, r.job.output.Capabilities()) {
		return err
	}
	r.directories = append(r.directories, DirectoryJobFailure{
		DirectoryID: directory, Path: path, Stage: FailureDirectoryOutput, Err: err,
	})
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
