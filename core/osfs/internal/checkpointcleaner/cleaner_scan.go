package checkpointcleaner

import (
	"context"
	"errors"
	"path"

	"github.com/windshare/windshare/core/osfs/internal/legacyresume"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

const legacyLookingRootPrefix = ".wsresume-output-"

type cleanupLockRef struct {
	parent outputcap.Directory
	name   string
	path   string
	lock   outputcap.Lock
}

type legacyObservation struct {
	controlRecord     bool
	controlTemporary  []string
	coordinatorLock   bool
	sessionsDirectory bool
}

func (observation legacyObservation) present() bool {
	return observation.controlRecord || len(observation.controlTemporary) != 0 ||
		observation.coordinatorLock || observation.sessionsDirectory
}

func (observation legacyObservation) requiresOwnershipRecord() bool {
	return len(observation.controlTemporary) != 0 || observation.sessionsDirectory
}

type cleanupRun struct {
	cleaner        *OneShotCheckpointCleaner
	guard          outputcap.PublicOperationGuard
	root           outputcap.Directory
	rootBinding    []byte
	certification  string
	durability     transfer.DurabilityLevel
	control        outputcap.Directory
	namespace      outputcap.Directory
	cleanupLock    outputcap.Lock
	coordinator    outputcap.Lock
	sessionLocks   []cleanupLockRef
	ownershipProof []byte
	approved       map[string]outputcap.EntryKind
	legacy         legacyObservation
	maintenance    bool
	step           uint32
	observed       uint64
}

type cleanupStateAdmission uint8

const (
	cleanupStateIdle cleanupStateAdmission = iota
	cleanupStateResume
	cleanupStateFresh
)

func (cleaner *OneShotCheckpointCleaner) prepareRun(
	ctx context.Context,
	report *CheckpointCleanupReport,
) (*cleanupRun, error) {
	run, err := cleaner.acquireCertifiedRun()
	if err != nil {
		return run, err
	}
	observed, err := run.observeLegacyNamespace(ctx, report)
	if err != nil || !observed {
		return run, err
	}
	state, _, stateFound, err := run.openExistingCleanerState()
	if err != nil {
		return run, err
	}
	admission := admitCleanupState(state, stateFound, run.legacy)
	if admission == cleanupStateIdle {
		return run, nil
	}
	authorized, err := run.acquireLegacyMaintenanceAuthority(admission, report)
	if err != nil || !authorized {
		return run, err
	}
	run.maintenance = true
	if run.legacy.sessionsDirectory {
		if err := run.inspectAndLockLegacySessions(ctx, report); err != nil {
			return run, err
		}
	}
	return run, nil
}

func (cleaner *OneShotCheckpointCleaner) acquireCertifiedRun() (*cleanupRun, error) {
	guard, err := cleaner.config.Platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	if guard == nil {
		return nil, ErrCheckpointCleanerOwnership
	}
	root := guard.Root()
	if root == nil {
		_ = guard.Close()
		return nil, ErrCheckpointCleanerOwnership
	}
	run := &cleanupRun{
		cleaner:  cleaner,
		guard:    guard,
		root:     root,
		approved: make(map[string]outputcap.EntryKind),
	}
	if err := run.bindCertifiedIdentity(); err != nil {
		return run, err
	}
	return run, nil
}

func (run *cleanupRun) bindCertifiedIdentity() error {
	rootBinding, err := run.cleaner.config.Platform.RootBinding()
	if err != nil {
		return errors.Join(ErrCheckpointCleanerOwnership, err)
	}
	run.rootBinding = rootBinding.Bytes()
	run.certification = string(run.cleaner.config.Platform.Certification())
	run.durability = run.cleaner.config.Platform.Durability()
	if legacyresume.ValidateExpectedOwnership(run.expectedOwnership()) != nil ||
		rootBinding.IsZero() ||
		rootBinding.Certification() != run.cleaner.config.Platform.Certification() {
		return ErrCheckpointCleanerOwnership
	}
	return run.revalidateCertifiedRoot()
}

func (run *cleanupRun) observeLegacyNamespace(
	ctx context.Context,
	report *CheckpointCleanupReport,
) (bool, error) {
	controlPresent, err := run.inspectRootEntries(ctx, report)
	if err != nil || report.NeedsAttention() || !controlPresent {
		return false, err
	}
	control, err := run.root.OpenDirectory(legacyresume.ControlDirectory, true)
	if err != nil {
		return false, err
	}
	run.control = control
	if err := run.revalidateControl(); err != nil {
		return false, err
	}
	if err := run.inspectControlEntries(ctx, report); err != nil || report.NeedsAttention() {
		return false, err
	}
	return true, nil
}

func admitCleanupState(
	state cleanerState,
	stateFound bool,
	observation legacyObservation,
) cleanupStateAdmission {
	if !observation.present() && (!stateFound || state.Complete) {
		return cleanupStateIdle
	}
	if stateFound && !state.Complete {
		return cleanupStateResume
	}
	return cleanupStateFresh
}

func (run *cleanupRun) acquireLegacyMaintenanceAuthority(
	admission cleanupStateAdmission,
	report *CheckpointCleanupReport,
) (bool, error) {
	switch admission {
	case cleanupStateResume:
		return run.acquireResumedLegacyAuthority(report)
	case cleanupStateFresh:
		return run.acquireFreshLegacyAuthority(report)
	default:
		return false, nil
	}
}

func (run *cleanupRun) acquireResumedLegacyAuthority(
	report *CheckpointCleanupReport,
) (bool, error) {
	if run.legacy.coordinatorLock {
		if err := run.acquireCoordinator(); err != nil {
			return false, err
		}
	} else if run.legacy.present() {
		addAttention(
			report,
			path.Join(legacyresume.ControlDirectory, legacyresume.CoordinatorLock),
			cleanupDetailConflict,
		)
		return false, nil
	}
	// Sessions and control temporaries are removed before the ownership record.
	// Their presence without it cannot be a cleaner crash cut, so persisted
	// maintenance state must not authorize a newly introduced tree.
	if !run.legacy.controlRecord && run.legacy.requiresOwnershipRecord() {
		addAttention(
			report,
			path.Join(legacyresume.ControlDirectory, legacyresume.ControlRecord),
			cleanupDetailConflict,
		)
		return false, nil
	}
	if run.legacy.controlRecord && !run.validateLegacyOwnership(report) {
		return false, nil
	}
	return true, nil
}

func (run *cleanupRun) acquireFreshLegacyAuthority(
	report *CheckpointCleanupReport,
) (bool, error) {
	if !run.legacy.controlRecord {
		addAttention(
			report,
			path.Join(legacyresume.ControlDirectory, legacyresume.ControlRecord),
			cleanupDetailUnknown,
		)
		return false, nil
	}
	if !run.legacy.coordinatorLock {
		addAttention(
			report,
			path.Join(legacyresume.ControlDirectory, legacyresume.CoordinatorLock),
			cleanupDetailConflict,
		)
		return false, nil
	}
	if err := run.acquireCoordinator(); err != nil {
		return false, err
	}
	if !run.validateLegacyOwnership(report) {
		return false, nil
	}
	if err := run.ensureCheckpointNamespace(); err != nil {
		return false, err
	}
	if err := run.acquireCleanupLock(); err != nil {
		return false, err
	}
	return true, nil
}

func (run *cleanupRun) expectedOwnership() legacyresume.ExpectedOwnership {
	return legacyresume.ExpectedOwnership{
		Backend: run.cleaner.config.BackendID, RootIdentity: run.rootBinding,
		Certification: run.certification, Durability: run.durability,
	}
}
