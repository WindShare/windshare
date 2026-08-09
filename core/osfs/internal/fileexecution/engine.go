package fileexecution

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"sync/atomic"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type Engine struct {
	intent      transfer.ReceiveIntent
	binding     checkpointmodel.Binding
	sessionID   transfer.OutputSessionID
	directories DirectoryAuthority
	platform    Platform
	checkpoints CheckpointRepository
	random      io.Reader
	trace       TraceSink
	sequence    atomic.Uint64
}

func New(config Config) (*Engine, error) {
	reservation, hasReservation := config.Intent.MaterializationPlan().DestinationReservation()
	if config.Intent.IsZero() || config.Intent.MaterializationPlan().Kind() != receivecontract.PlanDirectTree ||
		!hasReservation || !config.Ownership.Valid() ||
		config.Ownership.MaterializerKind() != checkpointmodel.MaterializerNativeTree ||
		config.Ownership.AuthorityRef() != reservation.AuthorityRef() ||
		config.SessionID.IsZero() || config.Directories == nil || config.Platform == nil ||
		config.Checkpoints == nil {
		return nil, ErrInvalidConfiguration
	}
	binding, err := checkpointmodel.NewBinding(
		config.Ownership, config.Intent.OperationID(), config.Intent.Digest(), config.Intent.BindingDigest(),
	)
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
	}, nil
}

func (engine *Engine) BeginFile(
	ctx context.Context,
	file transfer.MaterializationFile,
) (transfer.FileStart, error) {
	sequence := engine.nextSequence()
	if engine == nil || ctx == nil {
		return transfer.FileStart{}, fileContractError(ErrInvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return transfer.FileStart{}, err
	}
	key, err := engine.checkpointKey(file)
	if err != nil {
		engine.emit(engine.traceEvent(sequence, TraceBeginFile, TraceNoChange, 0, 0, traceFault(err)))
		return transfer.FileStart{}, bindingError(err)
	}
	destination, err := engine.directories.BindFile(ctx, file)
	if err != nil {
		return transfer.FileStart{}, collaboratorError(ctx, err)
	}
	if destination == nil || destination.Target() != file.Target {
		return transfer.FileStart{}, joinFailures(ctx, bindingError(ErrPortContract), closeDestination(destination))
	}
	record, found, err := engine.checkpoints.Lookup(ctx, key)
	if err != nil {
		return transfer.FileStart{}, joinFailures(ctx, collaboratorError(ctx, err), closeDestination(destination))
	}
	if found {
		if !key.matches(record) || !engine.binding.Matches(record, record.RecordID()) {
			return transfer.FileStart{}, joinFailures(ctx, checkpointBindingError(ErrCheckpointBinding), closeDestination(destination))
		}
		return engine.beginExisting(ctx, sequence, file, destination, record)
	}
	if record.Valid() {
		return transfer.FileStart{}, joinFailures(ctx, checkpointBindingError(ErrPortContract), closeDestination(destination))
	}
	return engine.beginNew(ctx, sequence, file, key, destination)
}

func (engine *Engine) beginNew(
	ctx context.Context,
	sequence uint64,
	file transfer.MaterializationFile,
	key CheckpointKey,
	destination FileDestination,
) (transfer.FileStart, error) {
	final, err := destination.ObserveFinalPresence(ctx)
	if err != nil || !final.valid() {
		return transfer.FileStart{}, joinFailures(ctx, collaboratorError(ctx, err), closeDestination(destination))
	}
	if final.Condition() != FinalAbsent {
		if final.Condition() != FinalCollision && final.Condition() != FinalOwnedExact &&
			final.Condition() != FinalOwnedMetadataMismatch {
			return transfer.FileStart{}, joinFailures(ctx, bindingError(ErrTargetOwnershipUnknown), closeDestination(destination))
		}
		settlement, settlementErr := transfer.NewCollisionFileSettlement(file.Target)
		return settlementStart(settlement, joinFailures(ctx, settlementErr, closeDestination(destination)))
	}
	for range MaximumObjectAllocationAttempts {
		object, allocationErr := engine.allocateObjectID()
		if allocationErr != nil {
			return transfer.FileStart{}, joinFailures(ctx, allocationErr, closeDestination(destination))
		}
		ownedFile, observation, createErr := engine.platform.CreateOwnedFile(ctx, object, key.exactSize)
		if !observation.validFor(object) {
			return transfer.FileStart{}, joinFailures(
				ctx, bindingError(ErrInvalidObservation), collaboratorError(ctx, createErr),
				closeOwnedFile(ownedFile), closeDestination(destination),
			)
		}
		if observation.Condition() == OwnedObjectCollision {
			if ownedFile != nil {
				return transfer.FileStart{}, joinFailures(ctx, bindingError(ErrPortContract), closeOwnedFile(ownedFile), closeDestination(destination))
			}
			continue
		}
		if observation.Condition() != OwnedReady || ownedFile == nil || ownedFile.ObjectID() != object {
			return transfer.FileStart{}, joinFailures(ctx, collaboratorError(ctx, createErr), bindingError(ErrTargetOwnershipUnknown), closeOwnedFile(ownedFile), closeDestination(destination))
		}
		candidate, err := newInitialRecord(key, object)
		if err != nil {
			return transfer.FileStart{}, joinFailures(ctx, err, closeOwnedFile(ownedFile), closeDestination(destination))
		}
		if _, err := engine.storeRecord(ctx, nil, candidate); err != nil {
			return transfer.FileStart{}, joinFailures(ctx, err, closeOwnedFile(ownedFile), closeDestination(destination))
		}
		if err := ownedFile.Sync(); err != nil {
			return transfer.FileStart{}, joinFailures(ctx, collaboratorError(ctx, err), closeOwnedFile(ownedFile), closeDestination(destination))
		}
		verified, err := checkpointmodel.PromoteInitialCandidate(candidate)
		if err != nil {
			return transfer.FileStart{}, joinFailures(ctx, err, closeOwnedFile(ownedFile), closeDestination(destination))
		}
		if _, err := engine.storeRecord(ctx, &candidate, verified); err != nil {
			return transfer.FileStart{}, joinFailures(ctx, err, closeOwnedFile(ownedFile), closeDestination(destination))
		}
		engine.emit(engine.traceEvent(sequence, TraceCreateOwnedFile, TraceSucceeded, 0, verified.Phase(), fault.Fault{}))
		return engine.transactionStart(file, destination, ownedFile, verified)
	}
	return transfer.FileStart{}, joinFailures(
		ctx, newOutputFault(fault.ScopeOutputPause, fault.OutputResourceBudget, ErrObjectAllocation),
		closeDestination(destination),
	)
}

func (engine *Engine) beginExisting(
	ctx context.Context,
	sequence uint64,
	file transfer.MaterializationFile,
	destination FileDestination,
	record checkpointmodel.Record,
) (transfer.FileStart, error) {
	binding, err := outputBinding(file.Target, record)
	if err != nil {
		return transfer.FileStart{}, joinFailures(ctx, err, closeDestination(destination))
	}
	writable := record.Phase() == checkpointmodel.PhaseActive || record.Phase() == checkpointmodel.PhasePaused ||
		record.Phase() == checkpointmodel.PhasePublishing
	ownedFile, owned, openErr := engine.platform.OpenOwnedFile(ctx, record.OwnedObjectID(), record.ExactSize(), writable)
	if !owned.validFor(record.OwnedObjectID()) ||
		owned.Condition() == OwnedReady && (ownedFile == nil || ownedFile.ObjectID() != record.OwnedObjectID()) ||
		owned.Condition() != OwnedReady && ownedFile != nil {
		return transfer.FileStart{}, joinFailures(ctx, bindingError(ErrInvalidObservation), collaboratorError(ctx, openErr), closeOwnedFile(ownedFile), closeDestination(destination))
	}
	expectation, err := expectationFor(file, record)
	if err != nil {
		return transfer.FileStart{}, joinFailures(ctx, err, closeOwnedFile(ownedFile), closeDestination(destination))
	}
	final, finalErr := destination.ObserveFinal(ctx, expectation)
	if finalErr != nil || !final.valid() {
		return transfer.FileStart{}, joinFailures(ctx, collaboratorError(ctx, finalErr), closeOwnedFile(ownedFile), closeDestination(destination))
	}
	if record.CommitState() == checkpointmodel.CommitCandidate {
		if owned.Condition() != OwnedReady || final.Condition() != FinalAbsent || ownedFile == nil {
			return transfer.FileStart{}, joinFailures(ctx, bindingError(ErrTargetOwnershipUnknown), closeOwnedFile(ownedFile), closeDestination(destination))
		}
		if err := ownedFile.Sync(); err != nil {
			return transfer.FileStart{}, joinFailures(ctx, collaboratorError(ctx, err), closeOwnedFile(ownedFile), closeDestination(destination))
		}
		verified, promoteErr := checkpointmodel.Promote(
			record, record.Phase(), checkpointmodel.CommitVerified,
		)
		if promoteErr != nil {
			return transfer.FileStart{}, joinFailures(ctx, promoteErr, closeOwnedFile(ownedFile), closeDestination(destination))
		}
		if _, err := engine.storeRecord(ctx, &record, verified); err != nil {
			return transfer.FileStart{}, joinFailures(ctx, err, closeOwnedFile(ownedFile), closeDestination(destination))
		}
		record = verified
	}
	decision, err := ReduceRecovery(record, owned, final)
	if err != nil {
		return transfer.FileStart{}, joinFailures(ctx, err, closeOwnedFile(ownedFile), closeDestination(destination))
	}
	engine.emit(engine.traceEvent(sequence, TraceRecoverFile, traceRecoveryOutcome(decision), record.Phase(), record.Phase(), fault.Fault{}))
	switch decision.Action() {
	case RecoveryOpenActive:
		return engine.transactionStart(file, destination, ownedFile, record)
	case RecoveryActivate:
		next, err := activateRecord(record)
		if err == nil {
			_, err = engine.storeRecord(ctx, &record, next)
		}
		if err != nil {
			return transfer.FileStart{}, joinFailures(ctx, err, closeOwnedFile(ownedFile), closeDestination(destination))
		}
		return engine.transactionStart(file, destination, ownedFile, next)
	case RecoveryRetryPublication, RecoveryCompletePublication:
		transaction := engine.newTransaction(file, destination, ownedFile, binding, record)
		return transaction.recoverPublication(ctx, decision.Action() == RecoveryRetryPublication)
	case RecoveryPublishBlocked:
		settlement, settlementErr := verifiedSettlement(transfer.FilePublishBlocked, binding, record)
		return engine.terminalStart(ctx, destination, ownedFile, settlement, settlementErr)
	case RecoveryReturnPublished:
		settlement, settlementErr := verifiedSettlement(transfer.FilePublished, binding, record)
		return engine.terminalStart(ctx, destination, ownedFile, settlement, settlementErr)
	case RecoveryReturnRetired:
		settlement, settlementErr := transfer.NewRetiredFileSettlement(binding)
		return engine.terminalStart(ctx, destination, ownedFile, settlement, settlementErr)
	case RecoveryReturnQuarantined:
		settlement, settlementErr := quarantinedSettlement(binding, record)
		return engine.terminalStart(ctx, destination, ownedFile, settlement, settlementErr)
	case RecoveryInstallQuarantine:
		next, transitionErr := quarantineRecord(record, decision.QuarantineReason())
		if transitionErr == nil {
			_, transitionErr = engine.storeRecord(ctx, &record, next)
		}
		if transitionErr != nil {
			return transfer.FileStart{}, joinFailures(ctx, transitionErr, closeOwnedFile(ownedFile), closeDestination(destination))
		}
		settlement, settlementErr := quarantinedSettlement(binding, next)
		return engine.terminalStart(ctx, destination, ownedFile, settlement, settlementErr)
	case RecoveryNeedsAttention:
		return transfer.FileStart{}, joinFailures(ctx, bindingError(ErrTargetOwnershipUnknown), closeOwnedFile(ownedFile), closeDestination(destination))
	default:
		return transfer.FileStart{}, joinFailures(ctx, bindingError(ErrPortContract), closeOwnedFile(ownedFile), closeDestination(destination))
	}
}

func (engine *Engine) transactionStart(
	file transfer.MaterializationFile,
	destination FileDestination,
	ownedFile OwnedFile,
	record checkpointmodel.Record,
) (transfer.FileStart, error) {
	binding, err := outputBinding(file.Target, record)
	if err != nil {
		return transfer.FileStart{}, err
	}
	transaction := engine.newTransaction(file, destination, ownedFile, binding, record)
	durable, err := durableRanges(binding, record)
	if err != nil {
		return transfer.FileStart{}, err
	}
	return transfer.NewFileTransactionStart(transaction, durable)
}

func (engine *Engine) newTransaction(
	file transfer.MaterializationFile,
	destination FileDestination,
	ownedFile OwnedFile,
	binding transfer.MaterializedFileBinding,
	record checkpointmodel.Record,
) *Transaction {
	pending, _ := content.NewRangeSet(nil)
	return &Transaction{
		engine: engine, materialization: file, destination: destination, file: ownedFile,
		binding: binding, record: record, pending: pending, state: transactionOpen,
	}
}

func (engine *Engine) terminalStart(
	ctx context.Context,
	destination FileDestination,
	ownedFile OwnedFile,
	settlement transfer.FileSettlement,
	err error,
) (transfer.FileStart, error) {
	err = joinFailures(ctx, err, closeOwnedFile(ownedFile), closeDestination(destination))
	return settlementStart(settlement, err)
}

func settlementStart(settlement transfer.FileSettlement, err error) (transfer.FileStart, error) {
	if err != nil {
		return transfer.FileStart{}, err
	}
	return transfer.NewFileSettlementStart(settlement)
}

func (engine *Engine) storeRecord(
	ctx context.Context,
	previous *checkpointmodel.Record,
	next checkpointmodel.Record,
) (bool, error) {
	if ctx == nil || !next.Valid() || !engine.binding.Matches(next, next.RecordID()) ||
		previous != nil && (!previous.Valid() || previous.RecordID() != next.RecordID()) {
		return false, checkpointBindingError(ErrCheckpointBinding)
	}
	observation, operationErr := engine.checkpoints.Store(ctx, previous, next)
	if !observation.valid() {
		return false, checkpointInstallError(errors.Join(ErrInvalidObservation, operationErr))
	}
	if current, present := observation.Record(); present && recordEqual(current, next) {
		return operationErr != nil, nil
	}
	if previous != nil {
		if current, present := observation.Record(); present && recordEqual(current, *previous) {
			return false, checkpointInstallError(errors.Join(ErrCheckpointNotInstalled, operationErr))
		}
	} else if _, present := observation.Record(); !present {
		return false, checkpointInstallError(errors.Join(ErrCheckpointNotInstalled, operationErr))
	}
	return false, checkpointInstallError(errors.Join(ErrTargetOwnershipUnknown, operationErr))
}

func (engine *Engine) allocateObjectID() (checkpointmodel.ObjectID, error) {
	var raw [transfer.OwnedObjectIdentityBytes]byte
	if _, err := io.ReadFull(engine.random, raw[:]); err != nil {
		return checkpointmodel.ObjectID{}, collaboratorError(context.Background(), err)
	}
	return checkpointmodel.ObjectIDFromBytes(raw[:])
}

func expectationFor(file transfer.MaterializationFile, record checkpointmodel.Record) (FinalExpectation, error) {
	identity, err := outputIdentity(record.OwnedObjectID())
	if err != nil {
		return FinalExpectation{}, err
	}
	return NewFinalExpectation(identity, record.ExactSize(), file.Descriptor.ModifiedTime())
}

func closeOwnedFile(file OwnedFile) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func closeDestination(destination FileDestination) error {
	if destination == nil {
		return nil
	}
	return destination.Close()
}

func traceRecoveryOutcome(decision RecoveryDecision) TraceOutcome {
	if decision.Action() == RecoveryNeedsAttention {
		return TraceNeedsAttention
	}
	return TraceReconciled
}
