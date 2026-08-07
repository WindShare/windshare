package checkpointcleaner

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

func (run *cleanupRun) acquireSessionLocks(sessions outputcap.Directory) error {
	return run.discoverSessionLocks(sessions, path.Join(resumestate.ControlDirectoryName, resumestate.SessionsDirectoryName))
}

func (run *cleanupRun) discoverSessionLocks(directory outputcap.Directory, relative string) error {
	names, err := boundedNames(directory)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := run.discoverSessionLockEntry(directory, relative, name); err != nil {
			return err
		}
	}
	return nil
}

func (run *cleanupRun) discoverSessionLockEntry(
	directory outputcap.Directory,
	relative string,
	name string,
) error {
	if err := run.observeEntry(); err != nil {
		return err
	}
	kind, err := directory.ObserveEntry(name)
	if err != nil {
		return err
	}
	entryPath := path.Join(relative, name)
	if name == resumestate.SessionLockName {
		return run.acquireExistingSessionLock(directory, name, entryPath, kind)
	}
	if kind != outputcap.EntryDirectory {
		return nil
	}
	child, err := directory.OpenDirectory(name, true)
	if err != nil {
		return err
	}
	discoverErr := run.discoverSessionLocks(child, entryPath)
	return errors.Join(discoverErr, child.Close())
}

func (run *cleanupRun) acquireExistingSessionLock(
	directory outputcap.Directory,
	name string,
	entryPath string,
	kind outputcap.EntryKind,
) error {
	if kind != outputcap.EntryRegularFile {
		return cleanerOwnershipFault("classify retired session lock", outputcap.ErrUnsafeNamespace)
	}
	lock, created, err := directory.AcquireLock(name, true)
	if errors.Is(err, outputcap.ErrNamespaceLockBusy) {
		return ErrCheckpointCleanerBusy
	}
	if err != nil {
		return err
	}
	if created || lock == nil || lock.File() == nil {
		return errors.Join(ErrCheckpointCleanerOwnership, closeLock(lock))
	}
	parent, err := directory.Duplicate()
	if err != nil {
		return errors.Join(err, lock.Close())
	}
	run.sessionLocks = append(run.sessionLocks, cleanupLockRef{
		parent: parent, name: name, path: entryPath, lock: lock,
	})
	return nil
}

func (run *cleanupRun) observeEntry() error {
	run.observed++
	if run.observed > maxCleanerEntries {
		return ErrCheckpointCleanerLimit
	}
	return nil
}

func (run *cleanupRun) cleanLegacyNamespace(
	ctx context.Context,
	state *cleanerState,
	previousEncoded *[]byte,
	report *CheckpointCleanupReport,
) error {
	if run.coordinator == nil {
		return nil
	}
	controlNames, err := boundedNames(run.control)
	if err != nil {
		return err
	}
	if slices.Contains(controlNames, resumestate.SessionsDirectoryName) {
		if err := run.removeLegacySessions(ctx, state, previousEncoded, report); err != nil {
			return err
		}
	}
	if slices.Contains(controlNames, resumestate.ControlRecordName) {
		if err := run.removeNonDirectoryEntry(
			ctx, run.control, resumestate.ControlRecordName,
			path.Join(resumestate.ControlDirectoryName, resumestate.ControlRecordName),
			state, previousEncoded, report,
		); err != nil {
			return err
		}
	}
	return run.removeCoordinator(ctx, state, previousEncoded, report)
}

func (run *cleanupRun) removeLegacySessions(
	ctx context.Context,
	state *cleanerState,
	previousEncoded *[]byte,
	report *CheckpointCleanupReport,
) error {
	sessions, err := run.control.OpenDirectory(resumestate.SessionsDirectoryName, true)
	if err != nil {
		return err
	}
	relative := path.Join(resumestate.ControlDirectoryName, resumestate.SessionsDirectoryName)
	cleanupErr := run.removeTreeContents(ctx, sessions, relative, state, previousEncoded, report)
	if cleanupErr == nil {
		cleanupErr = run.removeSessionLocks(ctx, state, previousEncoded, report)
	}
	if cleanupErr == nil {
		cleanupErr = run.removeTreeContents(ctx, sessions, relative, state, previousEncoded, report)
	}
	closeErr := sessions.Close()
	if cleanupErr != nil || closeErr != nil {
		return errors.Join(cleanupErr, closeErr)
	}
	return run.removeDirectoryEntry(
		ctx, run.control, resumestate.SessionsDirectoryName, relative, state, previousEncoded, report,
	)
}

func (run *cleanupRun) removeTreeContents(
	ctx context.Context,
	directory outputcap.Directory,
	relative string,
	state *cleanerState,
	previousEncoded *[]byte,
	report *CheckpointCleanupReport,
) error {
	names, err := boundedNames(directory)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := run.removeTreeEntry(ctx, directory, relative, name, state, previousEncoded, report); err != nil {
			return err
		}
	}
	return nil
}

func (run *cleanupRun) removeTreeEntry(
	ctx context.Context,
	directory outputcap.Directory,
	relative string,
	name string,
	state *cleanerState,
	previousEncoded *[]byte,
	report *CheckpointCleanupReport,
) error {
	if name == resumestate.SessionLockName {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	kind, err := directory.ObserveEntry(name)
	if err != nil {
		return err
	}
	entryPath := path.Join(relative, name)
	if kind == outputcap.EntryDirectory {
		return run.removeTreeDirectory(ctx, directory, name, entryPath, state, previousEncoded, report)
	}
	if kind == outputcap.EntryAbsent {
		return nil
	}
	return run.removeNonDirectoryEntry(ctx, directory, name, entryPath, state, previousEncoded, report)
}

func (run *cleanupRun) removeTreeDirectory(
	ctx context.Context,
	parent outputcap.Directory,
	name string,
	relative string,
	state *cleanerState,
	previousEncoded *[]byte,
	report *CheckpointCleanupReport,
) error {
	child, err := parent.OpenDirectory(name, true)
	if err != nil {
		return err
	}
	cleanupErr := run.removeTreeContents(ctx, child, relative, state, previousEncoded, report)
	remaining, namesErr := boundedNames(child)
	closeErr := child.Close()
	if cleanupErr != nil || namesErr != nil || closeErr != nil {
		return errors.Join(cleanupErr, namesErr, closeErr)
	}
	if len(remaining) != 0 {
		return nil
	}
	return run.removeDirectoryEntry(ctx, parent, name, relative, state, previousEncoded, report)
}

func (run *cleanupRun) removeNonDirectoryEntry(
	ctx context.Context,
	parent outputcap.Directory,
	name string,
	relative string,
	state *cleanerState,
	previousEncoded *[]byte,
	report *CheckpointCleanupReport,
) error {
	return run.applyRemoval(ctx, relative, state, previousEncoded, report, func() error {
		entry, err := parent.OpenEntry(name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		removeErr := parent.RemoveEntry(name, entry)
		syncErr := parent.Sync()
		return errors.Join(removeErr, syncErr, entry.Close())
	})
}

func (run *cleanupRun) removeDirectoryEntry(
	ctx context.Context,
	parent outputcap.Directory,
	name string,
	relative string,
	state *cleanerState,
	previousEncoded *[]byte,
	report *CheckpointCleanupReport,
) error {
	return run.applyRemoval(ctx, relative, state, previousEncoded, report, func() error {
		child, err := parent.OpenDirectory(name, true)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		removeErr := parent.RemoveDirectory(name, child)
		syncErr := parent.Sync()
		return errors.Join(removeErr, syncErr, child.Close())
	})
}

func (run *cleanupRun) removeSessionLocks(
	ctx context.Context,
	state *cleanerState,
	previousEncoded *[]byte,
	report *CheckpointCleanupReport,
) error {
	for index := range run.sessionLocks {
		reference := &run.sessionLocks[index]
		if reference.lock == nil {
			continue
		}
		err := run.applyRemoval(ctx, reference.path, state, previousEncoded, report, func() error {
			removeErr := reference.parent.RemoveFile(reference.name, reference.lock.File())
			syncErr := reference.parent.Sync()
			closeErr := reference.lock.Close()
			reference.lock = nil
			parentCloseErr := reference.parent.Close()
			reference.parent = nil
			return errors.Join(removeErr, syncErr, closeErr, parentCloseErr)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (run *cleanupRun) removeCoordinator(
	ctx context.Context,
	state *cleanerState,
	previousEncoded *[]byte,
	report *CheckpointCleanupReport,
) error {
	if run.coordinator == nil {
		return nil
	}
	relative := path.Join(resumestate.ControlDirectoryName, resumestate.CoordinatorLockName)
	return run.applyRemoval(ctx, relative, state, previousEncoded, report, func() error {
		removeErr := run.control.RemoveFile(resumestate.CoordinatorLockName, run.coordinator.File())
		syncErr := run.control.Sync()
		closeErr := run.coordinator.Close()
		run.coordinator = nil
		return errors.Join(removeErr, syncErr, closeErr)
	})
}

func (run *cleanupRun) applyRemoval(
	ctx context.Context,
	relative string,
	state *cleanerState,
	previousEncoded *[]byte,
	report *CheckpointCleanupReport,
	remove func() error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := run.authorizeMutation(); err != nil {
		return err
	}
	step := CheckpointCleanupStep{
		Index: run.step, RelativePath: relative, Disposition: CheckpointCleanupRemove,
	}
	if run.cleaner.config.Fault != nil {
		if err := run.cleaner.config.Fault(step); err != nil {
			return err
		}
	}
	if err := remove(); err != nil {
		return err
	}
	run.step++
	state.Mutations++
	report.Scanned++
	report.Removed++
	report.Entries = append(report.Entries, CheckpointCleanupEntry{
		RelativePath: relative, Disposition: CheckpointCleanupRemove,
	})
	return run.persistState(state, previousEncoded)
}

func (run *cleanupRun) authorizeMutation() error {
	if err := run.revalidateCertifiedRoot(); err != nil {
		return err
	}
	status, err := checkpointstore.InspectOwnership(run.namespaceConfig())
	if err != nil || status != checkpointstore.OwnershipMatched {
		return errors.Join(ErrCheckpointCleanerOwnership, err)
	}
	return nil
}
