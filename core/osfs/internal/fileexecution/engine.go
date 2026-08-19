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
	destinationPath transfer.OutputDestinationPath,
) (transfer.FileStart, error) {
	sequence := engine.nextSequence()
	if engine == nil || ctx == nil || !destinationPath.Valid() || destinationPath.IsSessionRoot() {
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
	destination, err := engine.directories.BindFile(ctx, file, destinationPath)
	if err != nil {
		return transfer.FileStart{}, collaboratorError(ctx, err)
	}
	if destination == nil || destination.Target() != file.Target() {
		return transfer.FileStart{}, joinFailures(ctx, bindingError(ErrPortContract), closeDestination(destination))
	}
	resolution, err := engine.checkpoints.Lookup(ctx, key)
	if err != nil {
		return transfer.FileStart{}, joinFailures(ctx, collaboratorError(ctx, err), closeDestination(destination))
	}
	if !resolution.valid() {
		return transfer.FileStart{}, joinFailures(ctx, checkpointBindingError(ErrPortContract), closeDestination(destination))
	}
	engine.emit(engine.checkpointDecisionTrace(sequence, resolution.Decision()))
	sequence = engine.nextSequence()
	return engine.beginResolved(ctx, sequence, file, key, destination, resolution)
}

func (engine *Engine) beginResolved(
	ctx context.Context,
	sequence uint64,
	file transfer.MaterializationFile,
	key CheckpointKey,
	destination FileDestination,
	resolution CheckpointResolution,
) (transfer.FileStart, error) {
	switch resolution.Decision() {
	case checkpointmodel.CheckpointLineageDecisionExact:
		record, exact := resolution.Record()
		if !exact || !key.matches(record) || !engine.binding.Matches(record, record.RecordID()) {
			return transfer.FileStart{}, joinFailures(ctx, checkpointBindingError(ErrCheckpointBinding), closeDestination(destination))
		}
		return engine.beginExisting(ctx, sequence, file, destination, record)
	case checkpointmodel.CheckpointLineageDecisionAbsent:
		return engine.beginNew(ctx, sequence, file, key, destination)
	case checkpointmodel.CheckpointLineageDecisionRevisionConflict,
		checkpointmodel.CheckpointLineageDecisionOwnershipConflict,
		checkpointmodel.CheckpointLineageDecisionInvalid:
		return engine.beginCheckpointBlocked(ctx, file, destination, resolution.Decision())
	default:
		return transfer.FileStart{}, joinFailures(ctx, checkpointBindingError(ErrPortContract), closeDestination(destination))
	}
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
	switch final.Condition() {
	case FinalAbsent:
		return engine.beginInitialCheckpoint(ctx, sequence, file, key, destination)
	case FinalCollision, FinalOwnedExact:
		settlement, settlementErr := transfer.NewCollisionFileSettlement(file.Target())
		return settlementStart(settlement, joinFailures(ctx, settlementErr, closeDestination(destination)))
	default:
		return transfer.FileStart{}, joinFailures(
			ctx, bindingError(ErrTargetOwnershipUnknown), closeDestination(destination),
		)
	}
}

func (engine *Engine) beginCheckpointBlocked(
	ctx context.Context,
	file transfer.MaterializationFile,
	destination FileDestination,
	decision checkpointmodel.CheckpointLineageDecision,
) (transfer.FileStart, error) {
	var reason transfer.ItemBlockReason
	switch decision {
	case checkpointmodel.CheckpointLineageDecisionRevisionConflict:
		reason = transfer.ItemBlockRevisionConflict
	case checkpointmodel.CheckpointLineageDecisionOwnershipConflict:
		reason = transfer.ItemBlockOwnedObjectUnknown
	case checkpointmodel.CheckpointLineageDecisionInvalid:
		reason = transfer.ItemBlockCheckpointInvalid
	default:
		return transfer.FileStart{}, joinFailures(ctx, checkpointBindingError(ErrPortContract), closeDestination(destination))
	}
	return engine.beginItemBlocked(ctx, file, destination, reason)
}

func (engine *Engine) beginItemBlocked(
	ctx context.Context,
	file transfer.MaterializationFile,
	destination FileDestination,
	reason transfer.ItemBlockReason,
) (transfer.FileStart, error) {
	reference, err := transfer.NewMaterializationStateRef(
		file.Target().OutputSessionID(), file.Target().Locator().Digest(),
	)
	if err != nil {
		return transfer.FileStart{}, joinFailures(ctx, err, closeDestination(destination))
	}
	settlement, err := transfer.NewImmediateItemBlockedFileSettlement(file.Target(), reference, reason)
	return engine.terminalStart(ctx, destination, nil, settlement, err)
}

func (engine *Engine) transactionStart(
	file transfer.MaterializationFile,
	destination FileDestination,
	ownedFile OwnedFile,
	record checkpointmodel.Record,
	resumed ...bool,
) (transfer.FileStart, error) {
	binding, err := outputBinding(file.Target(), record)
	if err != nil {
		return transfer.FileStart{}, err
	}
	wasResumed := len(resumed) == 1 && resumed[0]
	transaction, err := WrapPartialFileTransaction(
		engine.newResumablePartialFileTransaction(file, destination, ownedFile, binding, record, wasResumed),
	)
	if err != nil {
		return transfer.FileStart{}, err
	}
	durable, err := durableRanges(binding, record)
	if err != nil {
		return transfer.FileStart{}, err
	}
	return transfer.NewFileTransactionStart(transaction, durable)
}

func (engine *Engine) newResumablePartialFileTransaction(
	file transfer.MaterializationFile,
	destination FileDestination,
	ownedFile OwnedFile,
	binding transfer.MaterializedFileBinding,
	record checkpointmodel.Record,
	resumed ...bool,
) *resumablePartialFileTransaction {
	pending, _ := content.NewRangeSet(nil)
	return &resumablePartialFileTransaction{
		engine: engine, materialization: file, destination: destination, file: ownedFile,
		binding: binding, record: record, pending: pending, state: transactionOpen,
		resumed: len(resumed) == 1 && resumed[0],
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
		previous == nil || !previous.Valid() || previous.RecordID() != next.RecordID() {
		return false, checkpointBindingError(ErrCheckpointBinding)
	}
	observation, operationErr := engine.checkpoints.Replace(ctx, *previous, next)
	if operationErr != nil {
		return false, checkpointInstallError(operationErr)
	}
	if !observation.valid() {
		return false, checkpointInstallError(ErrInvalidObservation)
	}
	if current, present := observation.Record(); present && recordEqual(current, next) {
		return false, nil
	}
	if current, present := observation.Record(); present && recordEqual(current, *previous) {
		return false, checkpointInstallError(ErrCheckpointNotInstalled)
	}
	if _, present := observation.Record(); !present {
		return false, checkpointInstallError(ErrCheckpointNotInstalled)
	}
	return false, checkpointInstallError(ErrTargetOwnershipUnknown)
}

func (engine *Engine) allocateObjectID() (checkpointmodel.ObjectID, error) {
	var raw [transfer.OwnedObjectIdentityBytes]byte
	if _, err := io.ReadFull(engine.random, raw[:]); err != nil {
		return checkpointmodel.ObjectID{}, collaboratorError(context.Background(), err)
	}
	return checkpointmodel.ObjectIDFromBytes(raw[:])
}

func expectationFor(record checkpointmodel.Record) (FinalExpectation, error) {
	identity, err := outputIdentity(record.OwnedObjectID())
	if err != nil {
		return FinalExpectation{}, err
	}
	return NewFinalExpectation(identity, record.ExactSize())
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

func traceOperationForRecovery(decision RecoveryDecision) TraceOperation {
	switch decision.Action() {
	case RecoveryReturnQuarantined, RecoveryInstallQuarantine:
		return TraceItemBlocked
	default:
		return TraceRecoverFile
	}
}

func traceOutcomeForRecovery(decision RecoveryDecision) TraceOutcome {
	if decision.Action() == RecoveryReturnCollision {
		return TraceCollision
	}
	return TraceReconciled
}
