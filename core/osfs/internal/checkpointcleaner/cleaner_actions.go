package checkpointcleaner

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"path"

	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/legacyresume"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func (run *cleanupRun) cleanLegacyNamespace(
	ctx context.Context,
	state *cleanerState,
	previousEncoded *[]byte,
	report *CheckpointCleanupReport,
) error {
	if !run.maintenance || run.coordinator == nil {
		return nil
	}
	if run.legacy.sessionsDirectory {
		if err := run.removeLegacySessions(ctx, state, previousEncoded, report); err != nil {
			return err
		}
	}
	for _, name := range run.legacy.controlTemporary {
		if err := run.removeNonDirectoryEntry(
			ctx, run.control, name, path.Join(legacyresume.ControlDirectory, name),
			state, previousEncoded, report,
		); err != nil {
			return err
		}
	}
	if run.legacy.controlRecord {
		if err := run.removeNonDirectoryEntry(
			ctx, run.control, legacyresume.ControlRecord,
			path.Join(legacyresume.ControlDirectory, legacyresume.ControlRecord),
			state, previousEncoded, report,
		); err != nil {
			return err
		}
		run.legacy.controlRecord = false
		run.ownershipProof = nil
	}
	return run.removeCoordinator(ctx, state, previousEncoded, report)
}

func (run *cleanupRun) removeLegacySessions(
	ctx context.Context,
	state *cleanerState,
	previousEncoded *[]byte,
	report *CheckpointCleanupReport,
) error {
	sessions, err := run.control.OpenDirectory(legacyresume.SessionsDirectory, true)
	if err != nil {
		return err
	}
	relative := path.Join(legacyresume.ControlDirectory, legacyresume.SessionsDirectory)
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
		ctx, run.control, legacyresume.SessionsDirectory, relative, state, previousEncoded, report,
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
	if err := ctx.Err(); err != nil {
		return err
	}
	kind, exact, err := directory.ClassifyExactEntry(name)
	if err != nil {
		return err
	}
	entryPath := path.Join(relative, name)
	if kind == outputcap.EntryAbsent {
		return nil
	}
	if !exact {
		return cleanerOwnershipFault("revalidate legacy cleanup path", outputcap.ErrUnsafeNamespace)
	}
	if !run.approvedEntry(entryPath, kind) {
		return cleanerOwnershipFault("reject unobserved legacy cleanup path", outputcap.ErrUnsafeNamespace)
	}
	if name == legacyresume.SessionLock {
		return nil
	}
	if kind == outputcap.EntryDirectory {
		return run.removeTreeDirectory(ctx, directory, name, entryPath, state, previousEncoded, report)
	}
	if kind != outputcap.EntryRegularFile {
		return cleanerOwnershipFault("revalidate legacy cleanup file", outputcap.ErrUnsafeNamespace)
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
	entry, err := parent.OpenEntry(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if entry.Kind() != outputcap.EntryDirectory {
		return errors.Join(ErrCheckpointCleanerOwnership, entry.Close())
	}
	child, err := parent.OpenPinnedDirectory(entry, true)
	if err != nil {
		return errors.Join(err, entry.Close())
	}
	if reference := run.sessionLockForDirectory(relative); reference != nil {
		same, compareErr := child.SameDirectory(reference.parent)
		lockErr := run.revalidateLock(child, legacyresume.SessionLock, reference.lock)
		if compareErr != nil || lockErr != nil || !same {
			return errors.Join(ErrCheckpointCleanerOwnership, compareErr, lockErr, child.Close(), entry.Close())
		}
	}
	cleanupErr := run.removeTreeContents(ctx, child, relative, state, previousEncoded, report)
	remaining, namesErr := boundedNames(child)
	if cleanupErr != nil || namesErr != nil {
		return errors.Join(cleanupErr, namesErr, child.Close(), entry.Close())
	}
	if len(remaining) != 0 {
		return errors.Join(run.validateRemainingPlan(child, relative, remaining), child.Close(), entry.Close())
	}
	removeErr := run.applyRemoval(ctx, relative, state, previousEncoded, report, func() error {
		matches, err := parent.EntryMatches(name, entry)
		if err != nil || !matches {
			return errors.Join(outputcap.ErrUnsafeNamespace, err)
		}
		return errors.Join(parent.RemoveDirectory(name, child), parent.Sync())
	})
	return errors.Join(removeErr, child.Close(), entry.Close())
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
		if entry.Kind() != outputcap.EntryRegularFile {
			return errors.Join(outputcap.ErrUnsafeNamespace, entry.Close())
		}
		removeErr := parent.RemoveEntry(name, entry)
		return errors.Join(removeErr, parent.Sync(), entry.Close())
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
	entry, err := parent.OpenEntry(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if entry.Kind() != outputcap.EntryDirectory {
		return errors.Join(ErrCheckpointCleanerOwnership, entry.Close())
	}
	child, err := parent.OpenPinnedDirectory(entry, true)
	if err != nil {
		return errors.Join(err, entry.Close())
	}
	remaining, namesErr := boundedNames(child)
	if namesErr != nil {
		return errors.Join(namesErr, child.Close(), entry.Close())
	}
	if len(remaining) != 0 {
		return errors.Join(ErrCheckpointCleanerOwnership, child.Close(), entry.Close())
	}
	removeErr := run.applyRemoval(ctx, relative, state, previousEncoded, report, func() error {
		matches, err := parent.EntryMatches(name, entry)
		if err != nil || !matches {
			return errors.Join(outputcap.ErrUnsafeNamespace, err)
		}
		return errors.Join(parent.RemoveDirectory(name, child), parent.Sync())
	})
	return errors.Join(removeErr, child.Close(), entry.Close())
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
	relative := path.Join(legacyresume.ControlDirectory, legacyresume.CoordinatorLock)
	return run.applyRemoval(ctx, relative, state, previousEncoded, report, func() error {
		removeErr := run.control.RemoveFile(legacyresume.CoordinatorLock, run.coordinator.File())
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
	if _, ok := run.approved[relative]; !ok {
		return cleanerOwnershipFault("reject unplanned legacy cleanup mutation", outputcap.ErrUnsafeNamespace)
	}
	if err := run.authorizeMutation(*previousEncoded); err != nil {
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
	// Fault hooks model the cut immediately before mutation. Revalidating after
	// the hook also keeps tests honest when they simulate a name replacement in
	// that window.
	if err := run.authorizeMutation(*previousEncoded); err != nil {
		return err
	}
	if state.Mutations == ^uint64(0) {
		return ErrCheckpointCleanerState
	}
	if err := remove(); err != nil {
		return err
	}
	delete(run.approved, relative)
	run.step++
	state.Mutations++
	report.Removed++
	report.Entries = append(report.Entries, CheckpointCleanupEntry{
		RelativePath: relative, Disposition: CheckpointCleanupRemove,
	})
	return run.persistState(state, previousEncoded)
}

func (run *cleanupRun) validateRemainingPlan(
	directory outputcap.Directory,
	relative string,
	names []string,
) error {
	for _, name := range names {
		kind, exact, err := directory.ClassifyExactEntry(name)
		if err != nil {
			return err
		}
		if !exact || !run.approvedEntry(path.Join(relative, name), kind) {
			return cleanerOwnershipFault("reject changed legacy cleanup tree", outputcap.ErrUnsafeNamespace)
		}
	}
	return nil
}

func (run *cleanupRun) sessionLockForDirectory(relative string) *cleanupLockRef {
	wanted := path.Join(relative, legacyresume.SessionLock)
	for index := range run.sessionLocks {
		reference := &run.sessionLocks[index]
		if reference.path == wanted && reference.lock != nil && reference.parent != nil {
			return reference
		}
	}
	return nil
}

func (run *cleanupRun) authorizeMutation(expectedState []byte) error {
	if len(expectedState) == 0 || run.cleanupLock == nil || run.coordinator == nil {
		return ErrCheckpointCleanerOwnership
	}
	if err := run.revalidateCertifiedRoot(); err != nil {
		return err
	}
	if err := run.revalidateControl(); err != nil {
		return err
	}
	if err := run.revalidateCheckpointNamespace(); err != nil {
		return err
	}
	if run.legacy.controlRecord {
		currentProof, err := readBoundedRecord(
			run.control, legacyresume.ControlRecord, legacyresume.MaxOwnershipRecordBytes,
		)
		if err != nil || len(run.ownershipProof) == 0 || !bytes.Equal(currentProof, run.ownershipProof) {
			return errors.Join(ErrCheckpointCleanerOwnership, err)
		}
	} else if len(run.ownershipProof) != 0 {
		return ErrCheckpointCleanerOwnership
	}
	currentState, err := checkpointstore.ReadFile(run.namespace, FileCheckpointCleanupState)
	if err != nil || !bytes.Equal(currentState, expectedState) {
		return errors.Join(ErrCheckpointCleanerOwnership, err)
	}
	if err := run.revalidateLock(run.namespace, FileCheckpointCleanupLock, run.cleanupLock); err != nil {
		return err
	}
	if err := run.revalidateLock(run.control, legacyresume.CoordinatorLock, run.coordinator); err != nil {
		return err
	}
	// A held handle is insufficient after a name swap: the retired runtime
	// acquires these locks by name. Revalidate every remaining name before each
	// destructive step so cleanup never proceeds beside a newly active session.
	for index := range run.sessionLocks {
		reference := &run.sessionLocks[index]
		if reference.lock == nil {
			continue
		}
		if err := run.revalidateLock(reference.parent, reference.name, reference.lock); err != nil {
			return err
		}
	}
	return nil
}

func (run *cleanupRun) revalidateLock(
	parent outputcap.Directory,
	name string,
	expected outputcap.Lock,
) error {
	if parent == nil || expected == nil || expected.File() == nil {
		return ErrCheckpointCleanerOwnership
	}
	current, err := parent.OpenFile(name, true, false)
	if err != nil {
		return errors.Join(ErrCheckpointCleanerOwnership, err)
	}
	same, compareErr := current.SameFile(expected.File())
	closeErr := current.Close()
	if compareErr != nil || closeErr != nil || !same {
		return errors.Join(ErrCheckpointCleanerOwnership, compareErr, closeErr)
	}
	return nil
}
