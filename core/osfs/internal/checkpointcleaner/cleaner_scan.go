package checkpointcleaner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

const legacyRootPrefix = ".wsresume-output-"

type cleanupLockRef struct {
	parent outputcap.Directory
	name   string
	path   string
	lock   outputcap.Lock
}

type cleanupRun struct {
	cleaner      *OneShotCheckpointCleaner
	guard        outputcap.PublicOperationGuard
	root         outputcap.Directory
	rootBinding  resumestate.OutputRootBinding
	control      outputcap.Directory
	namespace    outputcap.Directory
	cleanupLock  outputcap.Lock
	coordinator  outputcap.Lock
	sessionLocks []cleanupLockRef
	step         uint32
	observed     uint64
}

func (cleaner *OneShotCheckpointCleaner) prepareRun(
	ctx context.Context,
) (*cleanupRun, []string, error) {
	guard, err := cleaner.config.Platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, nil, err
	}
	if guard == nil || guard.Root() == nil {
		if guard != nil {
			_ = guard.Close()
		}
		return nil, nil, ErrCheckpointCleanerOwnership
	}
	run := &cleanupRun{cleaner: cleaner, guard: guard, root: guard.Root()}
	rootBinding, err := cleaner.config.Platform.RootBinding()
	if err != nil || rootBinding.IsZero() {
		return run, nil, errors.Join(ErrCheckpointCleanerOwnership, err)
	}
	run.rootBinding = rootBinding
	if err := run.revalidateCertifiedRoot(); err != nil {
		return run, nil, err
	}
	attention, err := run.inspectRootEntries(ctx)
	if err != nil || len(attention) != 0 {
		return run, attention, err
	}
	attention, err = run.prepareControlAndOwnership()
	if err != nil || len(attention) != 0 {
		return run, attention, err
	}
	if err := run.acquireCleanupLock(); err != nil {
		return run, nil, err
	}
	attention, err = run.prepareLegacyLocks()
	if err != nil || len(attention) != 0 {
		return run, attention, err
	}
	return run, nil, nil
}

func (run *cleanupRun) namespaceConfig() checkpointstore.NamespaceConfig {
	return checkpointstore.NamespaceConfig{
		Root: run.root, BackendID: run.cleaner.config.BackendID,
		RootIdentity: run.rootBinding.Bytes(),
	}
}

func (run *cleanupRun) revalidateCertifiedRoot() error {
	if run == nil || run.root == nil || run.cleaner == nil || run.cleaner.config.Platform == nil {
		return ErrCheckpointCleanerOwnership
	}
	current, err := run.cleaner.config.Platform.RootBinding()
	if err != nil || current != run.rootBinding {
		return errors.Join(ErrCheckpointCleanerOwnership, err)
	}
	same, err := run.root.SameDirectory(run.cleaner.config.Platform.Root())
	if err != nil || !same {
		return errors.Join(ErrCheckpointCleanerOwnership, err)
	}
	return nil
}

func (run *cleanupRun) inspectRootEntries(ctx context.Context) ([]string, error) {
	names, err := boundedNames(run.root)
	if err != nil {
		return nil, err
	}
	var attention []string
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(name, legacyRootPrefix) {
			// A root-level prefix was never inside WindShare's control namespace.
			// Even a valid V1 marker cannot manufacture ownership of this entry.
			attention = append(attention, "unowned legacy-looking root entry: "+name)
		}
	}
	return attention, nil
}

func (run *cleanupRun) prepareControlAndOwnership() ([]string, error) {
	kind, err := run.root.ObserveEntry(resumestate.ControlDirectoryName)
	if err != nil {
		return nil, err
	}
	if kind == outputcap.EntryAbsent {
		if err := checkpointstore.BootstrapOwnership(run.namespaceConfig()); err != nil {
			return nil, err
		}
	} else if kind != outputcap.EntryDirectory {
		return []string{"output control namespace is not a directory"}, nil
	}
	control, err := run.root.OpenDirectory(resumestate.ControlDirectoryName, true)
	if err != nil {
		return nil, err
	}
	run.control = control
	status, err := checkpointstore.InspectOwnership(run.namespaceConfig())
	if err != nil {
		return nil, err
	}
	switch status {
	case checkpointstore.OwnershipMatched:
	case checkpointstore.OwnershipMismatch:
		return []string{"FileCheckpointV1 ownership marker does not match the certified root"}, nil
	case checkpointstore.OwnershipRecoverable:
		if err := checkpointstore.BootstrapOwnership(run.namespaceConfig()); err != nil {
			return nil, err
		}
	case checkpointstore.OwnershipAbsent:
		attention, err := run.authorizeOwnershipBootstrap()
		if err != nil || len(attention) != 0 {
			return attention, err
		}
		if err := checkpointstore.BootstrapOwnership(run.namespaceConfig()); err != nil {
			return nil, err
		}
	default:
		return nil, ErrCheckpointCleanerOwnership
	}
	namespace, err := checkpointstore.OpenOwnedNamespace(run.namespaceConfig())
	if err != nil {
		return nil, err
	}
	run.namespace = namespace
	return nil, nil
}

func (run *cleanupRun) authorizeOwnershipBootstrap() ([]string, error) {
	names, err := boundedNames(run.control)
	if err != nil {
		return nil, err
	}
	if safeEmptyCheckpointNamespace(run.control, names) {
		return nil, nil
	}
	if !slices.Contains(names, resumestate.ControlRecordName) ||
		!slices.Contains(names, resumestate.CoordinatorLockName) {
		return []string{"ownership marker is absent beside unproven output-control content"}, nil
	}
	if attention := unexpectedControlNames(names); len(attention) != 0 {
		return attention, nil
	}
	if err := run.acquireCoordinator(); err != nil {
		return nil, err
	}
	encoded, err := outputnamespace.ReadRecord(
		run.control, resumestate.ControlRecordName, resumestate.MaxControlStateBytes,
	)
	if err != nil {
		return nil, err
	}
	legacy, err := resumestate.DecodeControl(encoded)
	platform := run.cleaner.config.Platform
	if err != nil || legacy.Backend() != run.cleaner.config.BackendID ||
		legacy.OutputRoot() != run.rootBinding || legacy.Certification() != platform.Certification() ||
		legacy.Durability() != platform.Durability() {
		return []string{"legacy control state does not prove ownership of the certified root"}, nil
	}
	return nil, nil
}

func safeEmptyCheckpointNamespace(control outputcap.Directory, names []string) bool {
	if len(names) == 0 {
		return true
	}
	if len(names) != 1 || names[0] != resumestate.CheckpointsDirectoryName {
		return false
	}
	checkpointRoot, err := control.OpenDirectory(resumestate.CheckpointsDirectoryName, true)
	if err != nil {
		return false
	}
	defer checkpointRoot.Close()
	children, err := boundedNames(checkpointRoot)
	return err == nil && len(children) == 0
}

func (run *cleanupRun) acquireCleanupLock() error {
	lock, _, err := run.namespace.AcquireLock(FileCheckpointCleanupLock, false)
	if errors.Is(err, outputcap.ErrNamespaceLockBusy) {
		return ErrCheckpointCleanerBusy
	}
	if err != nil {
		return err
	}
	if lock == nil || lock.File() == nil {
		if lock != nil {
			_ = lock.Close()
		}
		return ErrCheckpointCleanerOwnership
	}
	run.cleanupLock = lock
	return nil
}

func (run *cleanupRun) prepareLegacyLocks() ([]string, error) {
	names, err := boundedNames(run.control)
	if err != nil {
		return nil, err
	}
	if attention := unexpectedControlNames(names); len(attention) != 0 {
		return attention, nil
	}
	legacyPresent := slices.Contains(names, resumestate.ControlRecordName) ||
		slices.Contains(names, resumestate.CoordinatorLockName) ||
		slices.Contains(names, resumestate.SessionsDirectoryName)
	if !legacyPresent {
		return nil, nil
	}
	if !slices.Contains(names, resumestate.CoordinatorLockName) {
		return []string{"legacy output namespace has no coordinator lock"}, nil
	}
	if err := run.acquireCoordinator(); err != nil {
		return nil, err
	}
	if err := run.revalidateCertifiedRoot(); err != nil {
		return nil, err
	}
	status, err := checkpointstore.InspectOwnership(run.namespaceConfig())
	if err != nil || status != checkpointstore.OwnershipMatched {
		return nil, errors.Join(ErrCheckpointCleanerOwnership, err)
	}
	if slices.Contains(names, resumestate.SessionsDirectoryName) {
		sessions, err := run.control.OpenDirectory(resumestate.SessionsDirectoryName, true)
		if err != nil {
			return nil, err
		}
		defer sessions.Close()
		if err := run.acquireSessionLocks(sessions); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func unexpectedControlNames(names []string) []string {
	allowed := map[string]struct{}{
		resumestate.CheckpointsDirectoryName: {},
		resumestate.ControlRecordName:        {},
		resumestate.CoordinatorLockName:      {},
		resumestate.SessionsDirectoryName:    {},
	}
	var attention []string
	for _, name := range names {
		if _, ok := allowed[name]; !ok {
			attention = append(attention, "unclassified output-control entry: "+name)
		}
	}
	return attention
}

func (run *cleanupRun) acquireCoordinator() error {
	if run.coordinator != nil {
		return nil
	}
	lock, created, err := run.control.AcquireLock(resumestate.CoordinatorLockName, true)
	if errors.Is(err, outputcap.ErrNamespaceLockBusy) {
		return ErrCheckpointCleanerBusy
	}
	if err != nil {
		return err
	}
	if created || lock == nil || lock.File() == nil {
		if lock != nil {
			_ = lock.Close()
		}
		return ErrCheckpointCleanerOwnership
	}
	run.coordinator = lock
	return nil
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

func (run *cleanupRun) Close() error {
	if run == nil {
		return nil
	}
	var result error
	for index := len(run.sessionLocks) - 1; index >= 0; index-- {
		result = errors.Join(
			result,
			closeLock(run.sessionLocks[index].lock),
			closeDirectory(run.sessionLocks[index].parent),
		)
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

func closeGuard(guard outputcap.PublicOperationGuard) error {
	if guard == nil {
		return nil
	}
	return guard.Close()
}

func cleanerOwnershipFault(operation string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrCheckpointCleanerOwnership, operation, err)
}
