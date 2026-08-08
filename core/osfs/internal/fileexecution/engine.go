package fileexecution

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"sync/atomic"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

type Engine struct {
	intent      transfer.TransferIntent
	binding     checkpointmodel.Binding
	sessionID   transfer.OutputSessionID
	directories DirectoryAuthority
	platform    Platform
	checkpoints CheckpointRepository
	random      io.Reader
	trace       TraceSink

	operationSequence atomic.Uint64
	pathCanonicalizer func(string) (string, error)
}

var _ outputsession.FileExecutor = (*Engine)(nil)

func New(config Config) (*Engine, error) {
	if config.Intent.IsZero() || config.Intent.Format() != transfer.OutputNativeTree ||
		!config.Ownership.Valid() || config.Intent.BackendID() != config.Ownership.Backend() ||
		config.SessionID.IsZero() || config.Directories == nil || config.Platform == nil ||
		config.Checkpoints == nil {
		return nil, ErrInvalidConfiguration
	}
	binding, err := checkpointmodel.NewBinding(config.Ownership, config.Intent.Digest())
	if err != nil {
		return nil, errors.Join(ErrInvalidConfiguration, err)
	}
	randomSource := config.Random
	if randomSource == nil {
		randomSource = rand.Reader
	}
	return &Engine{
		intent: config.Intent, binding: binding, sessionID: config.SessionID,
		directories: config.Directories, platform: config.Platform,
		checkpoints: config.Checkpoints, random: randomSource, trace: config.Trace,
		pathCanonicalizer: catalog.CanonicalPath,
	}, nil
}

func (engine *Engine) BeginFile(
	ctx context.Context,
	claim outputsession.FileClaim,
) (outputsession.FileBeginObservation, error) {
	operationID := engine.nextOperationID()
	if engine == nil || ctx == nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange},
			fileContractError(ErrInvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange}, err
	}
	key, err := engine.checkpointKey(claim)
	if err != nil {
		err = bindingError(err)
		engine.emit(engine.traceEvent(
			claim.ID(), operationID, TraceBeginFile, TraceNoChange, 0, 0, traceFault(err),
		))
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange}, err
	}
	destination, err := engine.directories.BindFile(ctx, claim)
	if err != nil {
		err = collaboratorError(ctx, err)
		engine.emit(engine.traceEvent(
			claim.ID(), operationID, TraceBeginFile, TraceNoChange, 0, 0, traceFault(err),
		))
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange}, err
	}
	if destination == nil || destination.ClaimID() != claim.ID() || destination.Target() != claim.File().Target {
		closeErr := closeDestination(destination)
		err = bindingError(errors.Join(ErrPortContract, closeErr))
		engine.emit(engine.traceEvent(
			claim.ID(), operationID, TraceBeginFile, TraceNoChange, 0, 0, traceFault(err),
		))
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange}, err
	}
	record, found, err := engine.lookupRecord(ctx, key)
	if err != nil {
		err = joinFailures(ctx, err, collaboratorError(ctx, destination.Close()))
		engine.emit(engine.traceEvent(
			claim.ID(), operationID, TraceBeginFile, TraceNoChange, 0, 0, traceFault(err),
		))
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange}, err
	}
	if found {
		return engine.beginExisting(ctx, operationID, claim, destination, record)
	}
	return engine.beginNew(ctx, operationID, claim, key, destination)
}

func (engine *Engine) beginNew(
	ctx context.Context,
	operationID uint64,
	claim outputsession.FileClaim,
	key CheckpointKey,
	destination FileDestination,
) (outputsession.FileBeginObservation, error) {
	final, err := destination.ObserveFinalPresence(ctx)
	if err != nil {
		err = joinFailures(ctx, collaboratorError(ctx, err), collaboratorError(ctx, destination.Close()))
		return engine.failedBegin(claim.ID(), operationID, outputsession.MutationNoChange, TraceNoChange, err)
	}
	if !final.valid() {
		err = joinFailures(ctx, bindingError(ErrInvalidObservation), collaboratorError(ctx, destination.Close()))
		return engine.failedBegin(claim.ID(), operationID, outputsession.MutationNoChange, TraceNoChange, err)
	}
	if final.Condition() != FinalAbsent {
		settlement, settlementErr := transfer.NewCollisionFileSettlement(claim.File().Target)
		closeErr := collaboratorError(ctx, destination.Close())
		if settlementErr != nil || closeErr != nil {
			err = joinFailures(ctx, fileContractError(settlementErr), closeErr)
			return engine.failedBegin(claim.ID(), operationID, outputsession.MutationNoChange, TraceNoChange, err)
		}
		engine.emit(engine.traceEvent(
			claim.ID(), operationID, TraceBeginFile, TraceCollision, 0, 0, fault.Fault{},
		))
		return outputsession.FileBeginObservation{
			Cut: outputsession.MutationStable, Settlement: settlement,
		}, nil
	}

	for range MaximumObjectAllocationAttempts {
		object, allocationErr := engine.allocateObjectID()
		if allocationErr != nil {
			err = joinFailures(ctx, allocationErr, collaboratorError(ctx, destination.Close()))
			return engine.failedBegin(claim.ID(), operationID, outputsession.MutationNoChange, TraceNoChange, err)
		}
		file, observation, createErr := engine.platform.CreateOwnedFile(ctx, object, key.exactSize)
		if !observation.validFor(object) {
			closeErr := closeOwnedFile(file)
			err = joinFailures(ctx,
				collaboratorError(ctx, createErr), collaboratorError(ctx, closeErr),
				bindingError(ErrInvalidObservation), collaboratorError(ctx, destination.Close()),
			)
			return engine.failedBegin(claim.ID(), operationID, outputsession.MutationAmbiguous, TraceNeedsAttention, err)
		}
		switch observation.Condition() {
		case OwnedObjectCollision:
			if file != nil {
				err = joinFailures(ctx,
					bindingError(ErrPortContract), collaboratorError(ctx, file.Close()),
					collaboratorError(ctx, destination.Close()),
				)
				return engine.failedBegin(claim.ID(), operationID, outputsession.MutationAmbiguous, TraceNeedsAttention, err)
			}
			continue
		case OwnedAbsent:
			closeErr := collaboratorError(ctx, closeOwnedFile(file))
			if createErr == nil {
				createErr = bindingError(ErrPortContract)
			} else {
				createErr = collaboratorError(ctx, createErr)
			}
			err = joinFailures(ctx, createErr, closeErr, collaboratorError(ctx, destination.Close()))
			return engine.failedBegin(claim.ID(), operationID, outputsession.MutationNoChange, TraceNoChange, err)
		case OwnedReady:
			if file == nil || file.ObjectID() != object {
				err = joinFailures(ctx,
					bindingError(ErrPortContract), collaboratorError(ctx, closeOwnedFile(file)),
					collaboratorError(ctx, destination.Close()),
				)
				return engine.failedBegin(claim.ID(), operationID, outputsession.MutationAmbiguous, TraceNeedsAttention, err)
			}
			engine.emit(engine.traceEvent(
				claim.ID(), operationID, TraceCreateOwnedFile,
				traceOutcomeForReconcile(createErr), 0, checkpointmodel.PhaseActive, fault.Fault{},
			))
			return engine.installInitialCheckpoint(ctx, operationID, claim, key, destination, file, object)
		default:
			return engine.installInitialQuarantine(
				ctx, operationID, claim, key, destination, file, object, createErr,
			)
		}
	}
	err = joinFailures(ctx,
		newOutputFault(fault.ScopeOutputPause, fault.OutputResourceBudget, ErrObjectAllocation),
		collaboratorError(ctx, destination.Close()),
	)
	return engine.failedBegin(claim.ID(), operationID, outputsession.MutationNoChange, TraceNoChange, err)
}

func (engine *Engine) installInitialCheckpoint(
	ctx context.Context,
	operationID uint64,
	claim outputsession.FileClaim,
	key CheckpointKey,
	destination FileDestination,
	file OwnedFile,
	object checkpointmodel.ObjectID,
) (outputsession.FileBeginObservation, error) {
	candidate, err := newInitialRecord(key, object)
	if err != nil {
		return engine.abandonNewObject(ctx, operationID, claim, destination, file, object, fileContractError(err))
	}
	cut, _, err := engine.storeRecord(ctx, nil, candidate)
	if err != nil {
		if cut == outputsession.MutationNoChange {
			return engine.abandonNewObject(ctx, operationID, claim, destination, file, object, err)
		}
		err = joinFailures(ctx, err, collaboratorError(ctx, closeOwnedFile(file)), collaboratorError(ctx, destination.Close()))
		return engine.failedBegin(claim.ID(), operationID, outputsession.MutationAmbiguous, TraceNeedsAttention, err)
	}
	if err := file.Sync(); err != nil {
		err = joinFailures(ctx, collaboratorError(ctx, err), collaboratorError(ctx, file.Close()), collaboratorError(ctx, destination.Close()))
		return engine.failedBegin(claim.ID(), operationID, outputsession.MutationAmbiguous, TraceNeedsAttention, err)
	}
	verified, err := checkpointmodel.PromoteInitialCandidate(candidate)
	if err != nil {
		err = joinFailures(ctx, fileContractError(err), collaboratorError(ctx, file.Close()), collaboratorError(ctx, destination.Close()))
		return engine.failedBegin(claim.ID(), operationID, outputsession.MutationAmbiguous, TraceNeedsAttention, err)
	}
	cut, reconciled, err := engine.storeRecord(ctx, &candidate, verified)
	if err != nil || cut != outputsession.MutationStable {
		err = joinFailures(ctx, err, collaboratorError(ctx, file.Close()), collaboratorError(ctx, destination.Close()))
		return engine.failedBegin(claim.ID(), operationID, outputsession.MutationAmbiguous, TraceNeedsAttention, err)
	}
	engine.emit(engine.traceEvent(
		claim.ID(), operationID, TraceCheckpoint, traceOutcome(reconciled),
		candidate.Phase(), verified.Phase(), fault.Fault{},
	))
	return engine.transactionStart(claim, destination, file, verified)
}

func (engine *Engine) installInitialQuarantine(
	ctx context.Context,
	operationID uint64,
	claim outputsession.FileClaim,
	key CheckpointKey,
	destination FileDestination,
	file OwnedFile,
	object checkpointmodel.ObjectID,
	createErr error,
) (outputsession.FileBeginObservation, error) {
	record, err := newInitialQuarantine(key, object)
	if err != nil {
		err = joinFailures(ctx, fileContractError(err), collaboratorError(ctx, createErr))
		return engine.failedNewMutation(ctx, operationID, claim, destination, file, err)
	}
	cut, reconciled, err := engine.storeRecord(ctx, nil, record)
	if err != nil || cut != outputsession.MutationStable {
		err = joinFailures(ctx, err, collaboratorError(ctx, createErr))
		return engine.failedNewMutation(ctx, operationID, claim, destination, file, err)
	}
	binding, err := outputBinding(claim.File().Target, record)
	if err == nil {
		var settlement transfer.FileSettlement
		settlement, err = quarantinedSettlement(binding, record)
		if err == nil {
			err = joinFailures(ctx, collaboratorError(ctx, closeOwnedFile(file)), collaboratorError(ctx, destination.Close()))
			if err == nil {
				engine.emit(engine.traceEvent(
					claim.ID(), operationID, TraceQuarantine, traceOutcome(reconciled),
					0, checkpointmodel.PhaseQuarantined, fault.Fault{},
				))
				return outputsession.FileBeginObservation{
					Cut: outputsession.MutationStable, Settlement: settlement,
				}, nil
			}
		}
	}
	err = joinFailures(ctx, fileContractError(err), collaboratorError(ctx, createErr))
	return engine.failedBegin(claim.ID(), operationID, outputsession.MutationAmbiguous, TraceNeedsAttention, err)
}

func (engine *Engine) abandonNewObject(
	ctx context.Context,
	operationID uint64,
	claim outputsession.FileClaim,
	destination FileDestination,
	file OwnedFile,
	object checkpointmodel.ObjectID,
	cause error,
) (outputsession.FileBeginObservation, error) {
	closeErr := collaboratorError(ctx, closeOwnedFile(file))
	cleanupErr := engine.cleanupOwned(ctx, object, OwnedReady)
	destinationErr := collaboratorError(ctx, destination.Close())
	err := joinFailures(ctx, cause, closeErr, cleanupErr, destinationErr)
	cut := outputsession.MutationNoChange
	outcome := TraceNoChange
	if closeErr != nil || cleanupErr != nil {
		cut = outputsession.MutationAmbiguous
		outcome = TraceNeedsAttention
	}
	return engine.failedBegin(claim.ID(), operationID, cut, outcome, err)
}

func (engine *Engine) failedNewMutation(
	ctx context.Context,
	operationID uint64,
	claim outputsession.FileClaim,
	destination FileDestination,
	file OwnedFile,
	cause error,
) (outputsession.FileBeginObservation, error) {
	err := joinFailures(ctx, cause, collaboratorError(ctx, closeOwnedFile(file)), collaboratorError(ctx, destination.Close()))
	return engine.failedBegin(claim.ID(), operationID, outputsession.MutationAmbiguous, TraceNeedsAttention, err)
}

func (engine *Engine) failedBegin(
	claimID outputsession.ClaimID,
	operationID uint64,
	cut outputsession.MutationCut,
	outcome TraceOutcome,
	err error,
) (outputsession.FileBeginObservation, error) {
	engine.emit(engine.traceEvent(
		claimID, operationID, TraceBeginFile, outcome, 0, 0, traceFault(err),
	))
	return outputsession.FileBeginObservation{Cut: cut}, err
}

func (engine *Engine) allocateObjectID() (checkpointmodel.ObjectID, error) {
	var raw [transfer.OutputObjectIdentityBytes]byte
	if _, err := io.ReadFull(engine.random, raw[:]); err != nil {
		return checkpointmodel.ObjectID{}, collaboratorError(context.Background(), err)
	}
	object, err := checkpointmodel.ObjectIDFromBytes(raw[:])
	if err != nil {
		return checkpointmodel.ObjectID{}, newOutputFault(
			fault.ScopeOutputPause, fault.OutputResourceBudget, errors.Join(ErrObjectAllocation, err),
		)
	}
	return object, nil
}

func closeDestination(destination FileDestination) error {
	if destination == nil {
		return nil
	}
	return destination.Close()
}

func closeOwnedFile(file OwnedFile) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func traceOutcome(reconciled bool) TraceOutcome {
	if reconciled {
		return TraceReconciled
	}
	return TraceSucceeded
}

func traceOutcomeForReconcile(operationErr error) TraceOutcome {
	return traceOutcome(operationErr != nil)
}
