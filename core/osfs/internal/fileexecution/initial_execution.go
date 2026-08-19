package fileexecution

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

type initialCheckpointAdmissionKind uint8

const (
	initialCheckpointRetry initialCheckpointAdmissionKind = iota + 1
	initialCheckpointSelected
	initialCheckpointInstalled
)

// initialCheckpointAdmission is returned only after the operation-scoped store
// has selected authority. Object mutation therefore cannot race ahead of the
// lineage claim that authorizes it.
type initialCheckpointAdmission struct {
	kind       initialCheckpointAdmissionKind
	resolution CheckpointResolution
	candidate  checkpointmodel.Record
}

func (engine *Engine) beginInitialCheckpoint(
	ctx context.Context,
	sequence uint64,
	file transfer.MaterializationFile,
	key CheckpointKey,
	destination FileDestination,
) (transfer.FileStart, error) {
	admission, err := engine.admitInitialCheckpoint(ctx, key)
	if err != nil {
		return transfer.FileStart{}, joinFailures(ctx, err, closeDestination(destination))
	}
	engine.emit(engine.checkpointDecisionTrace(sequence, admission.resolution.Decision()))
	sequence = engine.nextSequence()
	switch admission.kind {
	case initialCheckpointSelected:
		return engine.beginResolved(ctx, sequence, file, key, destination, admission.resolution)
	case initialCheckpointInstalled:
		return engine.materializeInitialCheckpoint(
			ctx, sequence, file, destination, admission.candidate,
		)
	default:
		return transfer.FileStart{}, joinFailures(
			ctx, checkpointBindingError(ErrPortContract), closeDestination(destination),
		)
	}
}

func (engine *Engine) admitInitialCheckpoint(
	ctx context.Context,
	key CheckpointKey,
) (initialCheckpointAdmission, error) {
	for range MaximumObjectAllocationAttempts {
		object, available, err := engine.availableObjectID(ctx, key.exactSize)
		if err != nil {
			return initialCheckpointAdmission{}, err
		}
		if !available {
			continue
		}
		candidate, err := newInitialRecord(key, object)
		if err != nil {
			return initialCheckpointAdmission{}, err
		}
		admission, err := engine.installInitialCheckpoint(ctx, key, candidate)
		if err != nil {
			return initialCheckpointAdmission{}, err
		}
		if admission.kind == initialCheckpointRetry {
			continue
		}
		return admission, nil
	}
	return initialCheckpointAdmission{}, newOutputFault(
		fault.ScopeOutputPause, fault.OutputResourceBudget, ErrObjectAllocation,
	)
}

func (engine *Engine) availableObjectID(
	ctx context.Context,
	exactSize uint64,
) (checkpointmodel.ObjectID, bool, error) {
	object, err := engine.allocateObjectID()
	if err != nil {
		return checkpointmodel.ObjectID{}, false, err
	}
	observation, observeErr := engine.platform.ObserveOwnedObject(ctx, object, exactSize)
	if observeErr != nil || !observation.validFor(object) {
		return checkpointmodel.ObjectID{}, false, joinFailures(
			ctx, bindingError(ErrInvalidObservation), collaboratorError(ctx, observeErr),
		)
	}
	return object, observation.Condition() == OwnedAbsent, nil
}

func (engine *Engine) installInitialCheckpoint(
	ctx context.Context,
	key CheckpointKey,
	candidate checkpointmodel.Record,
) (initialCheckpointAdmission, error) {
	// InstallInitial classifies and installs under the operation authority lock;
	// object creation is deliberately downstream of this call.
	initial, err := engine.checkpoints.InstallInitial(ctx, key, candidate)
	if errors.Is(err, ErrCheckpointObjectClaimed) {
		return initialCheckpointAdmission{kind: initialCheckpointRetry}, nil
	}
	if errors.Is(err, ErrCheckpointRecordCapacity) {
		return initialCheckpointAdmission{}, newOutputFault(
			fault.ScopeOutputPause, fault.OutputResourceBudget, err,
		)
	}
	if err != nil {
		return initialCheckpointAdmission{}, err
	}
	if !initial.valid() {
		return initialCheckpointAdmission{}, checkpointBindingError(ErrPortContract)
	}
	resolution := initial.Resolution()
	if !initial.Installed() {
		return initialCheckpointAdmission{
			kind: initialCheckpointSelected, resolution: resolution,
		}, nil
	}
	selected, exact := resolution.Record()
	if !exact || selected.RecordID() != candidate.RecordID() || !recordEqual(selected, candidate) {
		return initialCheckpointAdmission{}, checkpointBindingError(ErrCheckpointBinding)
	}
	return initialCheckpointAdmission{
		kind: initialCheckpointInstalled, resolution: resolution,
		candidate: candidate,
	}, nil
}

func (engine *Engine) materializeInitialCheckpoint(
	ctx context.Context,
	sequence uint64,
	file transfer.MaterializationFile,
	destination FileDestination,
	candidate checkpointmodel.Record,
) (transfer.FileStart, error) {
	object := candidate.OwnedObjectID()
	ownedFile, observation, createErr := engine.platform.CreateOwnedFile(
		ctx, destination, object, candidate.ExactSize(),
	)
	if !observation.validFor(object) {
		return transfer.FileStart{}, joinFailures(
			ctx, bindingError(ErrInvalidObservation), collaboratorError(ctx, createErr),
			closeOwnedFile(ownedFile), closeDestination(destination),
		)
	}
	if observation.Condition() != OwnedReady || ownedFile == nil || ownedFile.ObjectID() != object {
		return transfer.FileStart{}, joinFailures(
			ctx, collaboratorError(ctx, createErr), bindingError(ErrTargetOwnershipUnknown),
			closeOwnedFile(ownedFile), closeDestination(destination),
		)
	}
	if err := ownedFile.Sync(); err != nil {
		return transfer.FileStart{}, joinFailures(
			ctx, collaboratorError(ctx, err), closeOwnedFile(ownedFile), closeDestination(destination),
		)
	}
	verified, err := checkpointmodel.PromoteInitialCandidate(candidate)
	if err != nil {
		return transfer.FileStart{}, joinFailures(
			ctx, err, closeOwnedFile(ownedFile), closeDestination(destination),
		)
	}
	if _, err := engine.storeRecord(ctx, &candidate, verified); err != nil {
		return transfer.FileStart{}, joinFailures(
			ctx, err, closeOwnedFile(ownedFile), closeDestination(destination),
		)
	}
	engine.emit(engine.traceEvent(
		sequence, TraceCreateOwnedFile, TraceSucceeded, 0, verified.Phase(), fault.Fault{},
	))
	return engine.transactionStart(file, destination, ownedFile, verified)
}
