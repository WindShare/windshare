package checkpointcleaner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/legacyresume"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func (run *cleanupRun) ensureCheckpointNamespace() error {
	if run.namespace != nil {
		return nil
	}
	kind, exact, err := run.control.ClassifyExactEntry(legacyresume.CheckpointDirectory)
	if err != nil {
		return err
	}
	if kind != outputcap.EntryAbsent {
		if !exact || kind != outputcap.EntryDirectory {
			return ErrCheckpointCleanerOwnership
		}
		namespace, err := run.control.OpenDirectory(legacyresume.CheckpointDirectory, true)
		if err != nil {
			return err
		}
		run.namespace = namespace
		return nil
	}
	namespace, err := run.control.CreateDirectory(legacyresume.CheckpointDirectory, true)
	if errors.Is(err, outputcap.ErrNamespaceCollision) {
		kind, exact, classifyErr := run.control.ClassifyExactEntry(legacyresume.CheckpointDirectory)
		if classifyErr != nil || !exact || kind != outputcap.EntryDirectory {
			return errors.Join(ErrCheckpointCleanerOwnership, classifyErr)
		}
		namespace, err = run.control.OpenDirectory(legacyresume.CheckpointDirectory, true)
	}
	if err != nil {
		return errors.Join(err, closeDirectory(namespace))
	}
	if namespace == nil {
		return ErrCheckpointCleanerOwnership
	}
	if err := errors.Join(namespace.Sync(), run.control.Sync()); err != nil {
		return errors.Join(err, namespace.Close())
	}
	run.namespace = namespace
	return nil
}

func (run *cleanupRun) acquireCleanupLock() error {
	if run.cleanupLock != nil {
		return nil
	}
	if run.namespace == nil {
		return ErrCheckpointCleanerOwnership
	}
	lock, created, err := run.namespace.AcquireLock(FileCheckpointCleanupLock, false)
	if errors.Is(err, outputcap.ErrNamespaceLockBusy) {
		return ErrCheckpointCleanerBusy
	}
	if err != nil {
		return err
	}
	if lock == nil || lock.File() == nil {
		return errors.Join(ErrCheckpointCleanerOwnership, closeLock(lock))
	}
	if created {
		if err := run.namespace.Sync(); err != nil {
			return errors.Join(err, lock.Close())
		}
	}
	size, sizeErr := lock.File().Size()
	if sizeErr != nil || size != 0 {
		return errors.Join(ErrCheckpointCleanerOwnership, sizeErr, lock.Close())
	}
	run.cleanupLock = lock
	return nil
}

func (run *cleanupRun) acquireCoordinator() error {
	if run.coordinator != nil {
		return nil
	}
	lock, created, err := run.control.AcquireLock(legacyresume.CoordinatorLock, true)
	if errors.Is(err, outputcap.ErrNamespaceLockBusy) {
		return ErrCheckpointCleanerBusy
	}
	if err != nil {
		return err
	}
	if created || lock == nil || lock.File() == nil {
		return errors.Join(ErrCheckpointCleanerOwnership, closeLock(lock))
	}
	size, sizeErr := lock.File().Size()
	if sizeErr != nil || size != 0 {
		return errors.Join(ErrCheckpointCleanerOwnership, sizeErr, lock.Close())
	}
	run.coordinator = lock
	return nil
}

func (run *cleanupRun) validateLegacyOwnership(report *CheckpointCleanupReport) bool {
	encoded, err := readBoundedRecord(run.control, legacyresume.ControlRecord, legacyresume.MaxOwnershipRecordBytes)
	if err != nil {
		addAttention(report, path.Join(legacyresume.ControlDirectory, legacyresume.ControlRecord), cleanupDetailUnknown)
		return false
	}
	ownership, err := legacyresume.DecodeOwnership(encoded)
	if err != nil || !ownership.Matches(run.expectedOwnership()) {
		addAttention(report, path.Join(legacyresume.ControlDirectory, legacyresume.ControlRecord), cleanupDetailUnknown)
		return false
	}
	run.ownershipProof = append(run.ownershipProof[:0], encoded...)
	return true
}

func (run *cleanupRun) approve(relative string, kind outputcap.EntryKind) {
	if run == nil || relative == "" || kind == outputcap.EntryAbsent {
		return
	}
	run.approved[relative] = kind
}

func (run *cleanupRun) approvedEntry(relative string, kind outputcap.EntryKind) bool {
	if run == nil || run.approved == nil {
		return false
	}
	expected, ok := run.approved[relative]
	return ok && expected == kind
}

func hasFoldPrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix)
}

func (run *cleanupRun) revalidateCertifiedRoot() error {
	if run == nil || run.root == nil || run.cleaner == nil || run.cleaner.config.Platform == nil {
		return ErrCheckpointCleanerOwnership
	}
	current, err := run.cleaner.config.Platform.RootBinding()
	if err != nil || !bytes.Equal(current.Bytes(), run.rootBinding) ||
		string(current.Certification()) != run.certification ||
		string(run.cleaner.config.Platform.Certification()) != run.certification ||
		run.cleaner.config.Platform.Durability() != run.durability {
		return errors.Join(ErrCheckpointCleanerOwnership, err)
	}
	same, err := run.root.SameDirectory(run.cleaner.config.Platform.Root())
	if err != nil || !same {
		return errors.Join(ErrCheckpointCleanerOwnership, err)
	}
	return nil
}

func (run *cleanupRun) revalidateControl() error {
	if run.control == nil {
		return ErrCheckpointCleanerOwnership
	}
	current, err := run.root.OpenDirectory(legacyresume.ControlDirectory, true)
	if err != nil {
		return errors.Join(ErrCheckpointCleanerOwnership, err)
	}
	same, compareErr := current.SameDirectory(run.control)
	return errors.Join(func() error {
		if compareErr != nil || !same {
			return errors.Join(ErrCheckpointCleanerOwnership, compareErr)
		}
		return nil
	}(), current.Close())
}

func (run *cleanupRun) revalidateCheckpointNamespace() error {
	if run.namespace == nil || run.control == nil {
		return ErrCheckpointCleanerOwnership
	}
	current, err := run.control.OpenDirectory(legacyresume.CheckpointDirectory, true)
	if err != nil {
		return errors.Join(ErrCheckpointCleanerOwnership, err)
	}
	same, compareErr := current.SameDirectory(run.namespace)
	if compareErr != nil || !same {
		return errors.Join(ErrCheckpointCleanerOwnership, compareErr, current.Close())
	}
	return current.Close()
}

func boundedNames(directory outputcap.Directory) ([]string, error) {
	if directory == nil {
		return nil, ErrCheckpointCleanerOwnership
	}
	names, err := directory.Names(maxCleanerEntries + 1)
	if err != nil {
		return nil, err
	}
	if len(names) > maxCleanerEntries {
		return nil, ErrCheckpointCleanerLimit
	}
	slices.Sort(names)
	return names, nil
}

func readBoundedRecord(directory outputcap.Directory, name string, limit int) ([]byte, error) {
	if directory == nil || name == "" || limit <= 0 {
		return nil, ErrCheckpointCleanerOwnership
	}
	kind, exact, err := directory.ClassifyExactEntry(name)
	if err != nil {
		return nil, err
	}
	if kind == outputcap.EntryAbsent {
		return nil, fs.ErrNotExist
	}
	if !exact || kind != outputcap.EntryRegularFile {
		return nil, outputcap.ErrUnsafeNamespace
	}
	file, err := directory.OpenObservedFile(name, true)
	if err != nil {
		return nil, errors.Join(err, closeFile(file))
	}
	size, sizeErr := file.Size()
	if sizeErr != nil || size == 0 || size > uint64(limit) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, sizeErr, file.Close())
	}
	encoded := make([]byte, int(size))
	read, readErr := file.ReadAt(encoded, 0)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, errors.Join(readErr, file.Close())
	}
	if read != len(encoded) {
		return nil, errors.Join(io.ErrUnexpectedEOF, file.Close())
	}
	return encoded, file.Close()
}

func (run *cleanupRun) Close() error {
	if run == nil {
		return nil
	}
	var result error
	for _, reference := range slices.Backward(run.sessionLocks) {
		result = errors.Join(result, closeLock(reference.lock), closeDirectory(reference.parent))
	}
	run.sessionLocks = nil
	result = errors.Join(result, closeLock(run.coordinator), closeLock(run.cleanupLock),
		closeDirectory(run.namespace), closeDirectory(run.control), closeGuard(run.guard))
	run.coordinator, run.cleanupLock = nil, nil
	run.namespace, run.control, run.guard = nil, nil, nil
	return result
}

func closeLock(lock outputcap.Lock) error {
	if lock == nil {
		return nil
	}
	return lock.Close()
}

func closeDirectory(directory outputcap.Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}

func closeFile(file outputcap.FileIdentity) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func closeGuard(guard outputcap.PublicOperationGuard) error {
	if guard == nil {
		return nil
	}
	return guard.Close()
}

func cleanerOwnershipFault(operation string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrCheckpointCleanerOwnership, operation, err)
}
