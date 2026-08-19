package outputruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"sync"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

// operationOutputResources retains the staged operation until the session has
// drained. This preserves destination/name/lease ownership across every output
// mutation instead of reopening any display path.
type operationOutputResources struct {
	once        sync.Once
	err         error
	authority   *Authority
	operation   *Operation
	directories *directoryauthority.Authority
	repository  *checkpointstore.Repository
	store       *checkpointstore.FileExecutionStore
	lifecycle   *ordinaryLifecycleRecorder
}

func (resources *operationOutputResources) ReleaseOutputSession(context.Context) error {
	if resources == nil {
		return nil
	}
	resources.once.Do(func() {
		if resources.repository != nil {
			resources.err = errors.Join(resources.err, resources.repository.Close())
			resources.repository = nil
		}
		if resources.directories != nil {
			resources.err = errors.Join(resources.err, resources.directories.Close())
			resources.directories = nil
		}
		if resources.authority != nil && resources.operation != nil {
			resources.authority.mu.Lock()
			if resources.authority.operation == resources.operation {
				resources.authority.operation = nil
				resources.authority.stage = authorityStageLookupStopped
			}
			resources.authority.mu.Unlock()
			resources.err = errors.Join(resources.err, resources.operation.close())
		}
		resources.operation = nil
		resources.authority = nil
	})
	return resources.err
}

func (authority *Authority) openNamedOperation(
	ctx context.Context,
	operation *Operation,
) (transfer.DirectTreeSession, error) {
	intent := operation.intent
	entry := operation.topLevel.ReservedEntry()
	binder, err := newOperationDestinationBinder(entry)
	if err != nil {
		return nil, runtimeDependencyError("bind operation artifact coordinates", err)
	}
	directories, err := directoryauthority.New(operation.topLevel, directoryauthority.Config{
		Trace: authority.directoryRuntimeTrace(intent.Digest(), transfer.OutputSessionID{}),
	})
	if err != nil {
		return nil, runtimeDependencyError("construct named directory authority", err)
	}
	resources := &operationOutputResources{
		authority: authority, operation: operation, directories: directories,
	}
	secret, err := newDirectoryAdmissionSecret(authority.random)
	if err != nil {
		_ = resources.ReleaseOutputSession(context.Background())
		return nil, runtimeOutputError(
			ctx, transferfault.OutputStateIO, "generate named directory admission secret", err,
		)
	}
	sessionID, err := newOutputSessionID(authority.random)
	if err != nil {
		_ = resources.ReleaseOutputSession(context.Background())
		return nil, runtimeOutputError(ctx, transferfault.OutputStateIO, "generate output session identity", err)
	}
	capabilities, err := operationSessionCapabilities(operation)
	if err != nil {
		_ = resources.ReleaseOutputSession(context.Background())
		return nil, runtimeDependencyError("construct named DirectTree capabilities", err)
	}
	files, repository, store, err := authority.namedFileExecutor(operation, directories, sessionID)
	if err != nil {
		_ = resources.ReleaseOutputSession(context.Background())
		return nil, err
	}
	resources.repository = repository
	resources.store = store
	if operation.mode == outputcap.ExecutionResumable {
		resources.lifecycle, err = newOrdinaryLifecycleRecorder(operation, store, repository, sessionID)
		if err != nil {
			_ = resources.ReleaseOutputSession(context.Background())
			return nil, checkpointRuntimeError(ctx, "compose ordinary lifecycle reducer", err)
		}
	}
	session, err := outputsession.New(outputsession.Config{
		Intent: intent, SessionID: sessionID, Capabilities: capabilities, ReceiptSecret: secret[:],
		Locator: directories, Destinations: binder, Directories: directories,
		Files: files, Resources: resources,
		Lifecycle: ordinaryLifecycleInterface(resources.lifecycle),
		Trace:     authority.outputSessionRuntimeTrace(),
	})
	if err != nil {
		_ = resources.ReleaseOutputSession(context.Background())
		return nil, runtimeDependencyError("construct named DirectTree session", err)
	}
	return session, nil
}

func (authority *Authority) namedFileExecutor(
	operation *Operation,
	directories *directoryauthority.Authority,
	sessionID transfer.OutputSessionID,
) (outputsession.FileExecutor, *checkpointstore.Repository, *checkpointstore.FileExecutionStore, error) {
	if authority == nil || authority.destination == nil || operation == nil || directories == nil ||
		sessionID.IsZero() {
		return nil, nil, nil, runtimeDependencyError(
			"compose named file transaction", transfer.ErrInvalidOutputBinding,
		)
	}
	if operation.mode == outputcap.ExecutionLiveOnly {
		fileAuthority, err := directoryauthority.NewLiveFileAuthority(directories, sessionID)
		if err != nil {
			return nil, nil, nil, runtimeDependencyError("bind live-only file destinations", err)
		}
		return &liveFileExecutor{
			runtime: authority, authority: authority.destination,
			intent: operation.intent, sessionID: sessionID,
			directories: fileAuthority, random: authority.random,
		}, nil, nil, nil
	}
	if operation.mode != outputcap.ExecutionResumable || operation.lease == nil {
		return nil, nil, nil, runtimeDependencyError("compose named file transaction", transfer.ErrInvalidOutputBinding)
	}
	ownership, err := authority.destination.FileCheckpointOwnership(operation.topLevel.RootOpenDisposition())
	if err != nil {
		return nil, nil, nil, checkpointRuntimeError(context.Background(), "bind ordinary file ownership", err)
	}
	binding, err := checkpointmodel.NewBinding(
		ownership, operation.intent.OperationID(), operation.intent.Digest(), operation.intent.BindingDigest(),
	)
	if err != nil {
		return nil, nil, nil, runtimeDependencyError("bind ordinary file checkpoints", err)
	}
	repository, err := checkpointstore.OpenOrdinaryFileRepository(operation.lease, binding, true)
	if err != nil {
		return nil, nil, nil, checkpointRuntimeError(context.Background(), "open ordinary file repository", err)
	}
	storeFactory := checkpointstore.NewFreshFileExecutionStoreWithProfile
	if operation.reopened {
		storeFactory = checkpointstore.NewFileExecutionStoreWithProfile
	}
	store, err := storeFactory(&repository, authority.destination.LiveCleanupProfile())
	var repositoryAttention bool
	if operation.reopened {
		var recordCount uint64
		if store != nil {
			recordCount = store.RecordCount()
			_, attention := store.LineageSnapshot()
			repositoryAttention = len(attention) != 0
		}
		authority.traceCheckpointReconciled(
			operation.intent, sessionID, recordCount, repositoryAttention, err,
		)
	}
	if err != nil {
		primary := diagnoseFilesystemOutputFailure(
			FilesystemOutputFailureCheckpointReconciliation,
			checkpointRuntimeError(context.Background(), "reconcile ordinary file repository", err),
		)
		return nil, nil, nil, freezeFilesystemOutputFailure(primary, repository.Close())
	}
	if repositoryAttention {
		// Opaque repository evidence cannot grant mutation authority even when
		// authenticated siblings reconcile cleanly.
		attentionErr := requireOperationAttention(
			operation.lease, checkpointmodel.OrdinaryReasonOperationOwnershipUnknown,
		)
		primary := runtimeOutputError(
			context.Background(), transferfault.OutputOwnership,
			"reject ordinary file repository attention", ErrNativeResumeOwnershipUnknown,
		)
		return nil, nil, nil, errors.Join(primary, attentionErr, repository.Close())
	}
	fileAuthority, err := directoryauthority.NewFileAuthority(directories, store, sessionID)
	if err != nil {
		return nil, nil, nil, errors.Join(runtimeDependencyError("bind ordinary file destinations", err), repository.Close())
	}
	engine, err := fileexecution.New(fileexecution.Config{
		Intent: operation.intent, Ownership: ownership, SessionID: sessionID,
		Directories: fileAuthority, Platform: store, Checkpoints: store,
		Trace: authority.fileRuntimeTrace(),
	})
	if err != nil {
		return nil, nil, nil, errors.Join(runtimeDependencyError("construct ordinary file transaction", err), repository.Close())
	}
	return newFileExecutionAdapter(engine), &repository, store, nil
}

func ordinaryLifecycleInterface(
	recorder *ordinaryLifecycleRecorder,
) outputsession.TreeLifecycleRecorder {
	if recorder == nil {
		return nil
	}
	return recorder
}

type ordinaryLifecycleRecorder struct {
	mu         sync.Mutex
	operation  *Operation
	store      *checkpointstore.FileExecutionStore
	repository *checkpointstore.Repository
	sessionID  transfer.OutputSessionID
}

func newOrdinaryLifecycleRecorder(
	operation *Operation,
	store *checkpointstore.FileExecutionStore,
	repository *checkpointstore.Repository,
	sessionID transfer.OutputSessionID,
) (*ordinaryLifecycleRecorder, error) {
	if operation == nil || operation.lease == nil || operation.mode != outputcap.ExecutionResumable ||
		operation.lease.Record().Lifecycle() != checkpointmodel.OrdinaryOperationActive ||
		store == nil || repository == nil || sessionID.IsZero() {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return &ordinaryLifecycleRecorder{
		operation: operation, store: store, repository: repository, sessionID: sessionID,
	}, nil
}

func (recorder *ordinaryLifecycleRecorder) traceCleanup(
	decision FilesystemOutputRuntimeDecision,
	failed bool,
) {
	if recorder == nil || recorder.operation == nil || recorder.operation.authority == nil {
		return
	}
	intent := recorder.operation.intent
	recorder.operation.authority.trace(FilesystemOutputTrace{
		Operation:           TraceRuntimeDecision,
		ReceiveIntentDigest: intent.Digest(),
		ReceiveOperationID:  intent.OperationID(),
		SessionID:           recorder.sessionID,
		RuntimeComponent:    FilesystemOutputRuntimeCheckpoint,
		RuntimeOperation:    FilesystemOutputRuntimeCleanup,
		RuntimeDecision:     decision,
		Failed:              failed,
	})
}

func (recorder *ordinaryLifecycleRecorder) RecordTreeSettlement(
	ctx context.Context,
	kind transfer.DirectTreeSettlementKind,
	_ transfer.DirectTreeOutcome,
	_ outputsession.TreeSettlementSnapshot,
) error {
	if recorder == nil || ctx == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.operation == nil || recorder.operation.lease == nil {
		return transfer.ErrInvalidOutputBinding
	}
	event := checkpointmodel.OrdinaryLifecycleContinue
	reason := checkpointmodel.OrdinaryReasonNone
	switch kind {
	case transfer.DirectTreeSettlementSuccess:
		event = checkpointmodel.OrdinaryLifecycleComplete
	case transfer.DirectTreeSettlementPartial, transfer.DirectTreeSettlementPaused:
		// Product results never imply lost operation authority. They retain the
		// active row so a later invocation reuses the frozen destination name.
	case transfer.DirectTreeSettlementFailed:
		event = checkpointmodel.OrdinaryLifecycleRequireAttention
		reason = checkpointmodel.OrdinaryReasonOperationOwnershipUnknown
	default:
		return transfer.ErrInvalidOutputSettlement
	}
	previous := recorder.operation.lease.Record()
	nextLifecycle, nextReason, err := checkpointmodel.ReduceOrdinaryOperationLifecycle(
		previous.Lifecycle(), event, reason,
	)
	if err != nil {
		return err
	}
	if nextLifecycle == previous.Lifecycle() && nextReason == previous.ClosedReason() {
		return nil
	}
	next, err := checkpointmodel.NextOrdinaryOperationRecord(
		previous,
		checkpointmodel.NextOrdinaryOperationRecordSpec{
			Lifecycle: nextLifecycle, Lease: checkpointmodel.OrdinaryLeaseHeld, ClosedReason: nextReason,
		},
	)
	if err != nil {
		return err
	}
	if err := recorder.operation.lease.Replace(previous, next); err != nil {
		return err
	}
	if next.Lifecycle() != checkpointmodel.OrdinaryOperationCompleted {
		return nil
	}
	return recorder.cleanupTerminal(ctx)
}

func (recorder *ordinaryLifecycleRecorder) cleanupTerminal(ctx context.Context) error {
	var cleanupErr error
	if recorder.store == nil || recorder.repository == nil {
		cleanupErr = transfer.ErrInvalidOutputBinding
	} else if cleanupErr = recorder.store.CleanupOwned(ctx); cleanupErr == nil {
		// Close every file-state capability before the registry removes its empty
		// hierarchy; this also makes a crash at any removal cut restartable.
		cleanupErr = recorder.repository.Close()
	}
	if cleanupErr == nil {
		cleanupErr = recorder.operation.lease.DeleteTerminal()
	}
	if cleanupErr == nil || recorder.operation.lease.Deleted() {
		// An unlink that reached the parent but failed its sync is durability
		// uncertain, not a license to recreate the deleted row. After a crash the
		// filesystem exposes either the completed row for retry or no private state.
		recorder.store = nil
		recorder.repository = nil
		recorder.traceCleanup(FilesystemOutputRuntimeSucceeded, false)
		return nil
	}
	previous := recorder.operation.lease.Record()
	nextLifecycle, nextReason, reduceErr := checkpointmodel.ReduceOrdinaryOperationLifecycle(
		previous.Lifecycle(), checkpointmodel.OrdinaryLifecycleCleanupFailed,
		checkpointmodel.OrdinaryReasonCleanupUncertain,
	)
	if reduceErr != nil {
		return errors.Join(cleanupErr, reduceErr)
	}
	next, nextErr := checkpointmodel.NextOrdinaryOperationRecord(
		previous,
		checkpointmodel.NextOrdinaryOperationRecordSpec{
			Lifecycle: nextLifecycle, Lease: checkpointmodel.OrdinaryLeaseHeld, ClosedReason: nextReason,
		},
	)
	if nextErr != nil {
		return errors.Join(cleanupErr, nextErr)
	}
	if replaceErr := recorder.operation.lease.Replace(previous, next); replaceErr != nil {
		return errors.Join(cleanupErr, replaceErr)
	}
	// The published command result remains truthful. The durable row carries the
	// cleanup failure for explicit list/discard repair without retracting finals.
	recorder.traceCleanup(FilesystemOutputRuntimeCleanupPending, true)
	return nil
}

type liveFileExecutor struct {
	runtime     *Authority
	authority   *destinationauthority.BoundDestination
	intent      transfer.ReceiveIntent
	sessionID   transfer.OutputSessionID
	directories *directoryauthority.FileAuthority
	random      io.Reader
}

func (executor *liveFileExecutor) BeginFile(
	ctx context.Context,
	claim outputsession.FileClaim,
) (outputsession.FileBeginObservation, error) {
	if executor == nil || executor.runtime == nil || executor.authority == nil ||
		executor.directories == nil || ctx == nil ||
		executor.intent.IsZero() || executor.sessionID.IsZero() || executor.random == nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange}, transfer.ErrInvalidOutputBinding
	}
	file := claim.File()
	destination, err := executor.directories.BindFile(ctx, file, claim.DestinationPath())
	if err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange}, err
	}
	final, err := destination.ObserveFinalPresence(ctx)
	if err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange}, errors.Join(err, destination.Close())
	}
	if final.Condition() != fileexecution.FinalAbsent {
		settlement, settlementErr := transfer.NewCollisionFileSettlement(file.Target())
		return outputsession.FileBeginObservation{
			Cut: outputsession.MutationStable, Settlement: settlement,
		}, errors.Join(settlementErr, destination.Close())
	}
	var nonce [checkpointmodel.LiveCleanupNonceBytesV1]byte
	if _, err := io.ReadFull(executor.random, nonce[:]); err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange}, errors.Join(err, destination.Close())
	}
	ticket, err := checkpointmodel.NewLiveCleanupTicket(checkpointmodel.LiveCleanupTicketSpec{
		Nonce: nonce[:], ExactSize: file.ExpectedSize(),
		Profile: executor.authority.LiveCleanupProfile(), Generation: 1,
		State: checkpointmodel.LiveCleanupTicketCommitted,
	})
	if err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange}, errors.Join(err, destination.Close())
	}
	parent, ok := destination.(destinationauthority.LiveCleanupStageParent)
	if !ok {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange}, errors.Join(
			transfer.ErrInvalidOutputBinding, destination.Close(),
		)
	}
	stage, created, err := executor.authority.CreateLiveCleanupStage(ctx, parent, ticket)
	if err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationAmbiguous}, errors.Join(err, destination.Close())
	}
	objectDigest := sha256.Sum256(append([]byte("windshare/live-partial-object/v1\x00"), nonce[:]...))
	object, err := checkpointmodel.ObjectIDFromBytes(objectDigest[:])
	if err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationAmbiguous}, errors.Join(err, stage.Close(), destination.Close())
	}
	owned, err := fileexecution.NewLiveOwnedFile(object, stage, created)
	if err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationAmbiguous}, errors.Join(err, stage.Close(), destination.Close())
	}
	transaction, err := fileexecution.NewLivePartialFileTransaction(
		file, destination, owned,
		func(current *fileexecution.LiveOwnedFile) error {
			err := executor.authority.RemoveLiveCleanupStage(
				current.CleanupTicket(), current.NativeFile(),
			)
			decision := FilesystemOutputRuntimeSucceeded
			if err != nil {
				decision = FilesystemOutputRuntimeCleanupPending
			}
			executor.runtime.trace(FilesystemOutputTrace{
				Operation:           TraceRuntimeDecision,
				ReceiveIntentDigest: executor.intent.Digest(),
				ReceiveOperationID:  executor.intent.OperationID(),
				SessionID:           executor.sessionID,
				RuntimeComponent:    FilesystemOutputRuntimeCheckpoint,
				RuntimeOperation:    FilesystemOutputRuntimeCleanup,
				RuntimeDecision:     decision,
				Failed:              err != nil,
			})
			return err
		},
	)
	if err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationAmbiguous}, errors.Join(err, owned.Close(), destination.Close())
	}
	empty, _ := content.NewRangeSet(nil)
	durable, err := transfer.VerifyDurableRanges(transaction.Binding(), 0, empty)
	if err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationAmbiguous}, err
	}
	return outputsession.FileBeginObservation{
		Cut: outputsession.MutationStable, Transaction: fileTransactionAdapter{transaction: transaction}, Durable: durable,
	}, nil
}

func newOutputSessionID(random io.Reader) (transfer.OutputSessionID, error) {
	if random == nil {
		return transfer.OutputSessionID{}, transfer.ErrInvalidOutputBinding
	}
	raw := make([]byte, transfer.OutputSessionIdentityBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return transfer.OutputSessionID{}, err
	}
	return transfer.OutputSessionIDFromBytes(raw)
}

func operationSessionCapabilities(
	operation *Operation,
) (transfer.DirectTreeCapabilities, error) {
	if operation == nil || operation.topLevel == nil {
		return transfer.DirectTreeCapabilities{}, transfer.ErrInvalidOutputBinding
	}
	binding := operation.authority.binding
	if !binding.Valid() {
		return transfer.DirectTreeCapabilities{}, transfer.ErrInvalidOutputBinding
	}
	durability := transfer.DurabilityNone
	if operation.mode == outputcap.ExecutionResumable {
		durability = transfer.DurabilityProcessRestart
	}
	return transfer.NewDirectTreeCapabilities(transfer.DirectTreeCapabilities{
		Durability: durability, RandomWrite: true,
		FileFailureIsolation: true, ModifiedTime: true,
	})
}

type operationDestinationBinder struct {
	entry destinationauthority.ReservedEntry
}

func newOperationDestinationBinder(
	entry destinationauthority.ReservedEntry,
) (operationDestinationBinder, error) {
	if !entry.Valid() {
		return operationDestinationBinder{}, transfer.ErrInvalidOutputBinding
	}
	return operationDestinationBinder{entry: entry}, nil
}

func (binder operationDestinationBinder) BindArtifactPath(
	artifact ordinaryoutput.ArtifactPath,
) (outputsession.DestinationPath, error) {
	if !artifact.Valid() || !binder.entry.Valid() {
		return outputsession.DestinationPath{}, transfer.ErrInvalidOutputBinding
	}
	physical, err := destinationauthority.PhysicalArtifactPath(artifact.String(), binder.entry)
	if err != nil {
		return outputsession.DestinationPath{}, err
	}
	if physical == "" {
		if binder.entry.EntryKind() != receivecontract.ContainerEntryResultRoot {
			return outputsession.DestinationPath{}, transfer.ErrInvalidOutputBinding
		}
		return outputsession.NewDestinationSessionRoot(), nil
	}
	return outputsession.NewDestinationPath(physical)
}

var _ directoryauthority.Platform = (*destinationauthority.TopLevelReservation)(nil)
var _ outputsession.ArtifactDestinationBinder = operationDestinationBinder{}
var _ transfer.DirectTreeSession = (*outputsession.Session)(nil)

func validateNamedOperationIntent(
	intent transfer.ReceiveIntent,
	entry destinationauthority.ReservedEntry,
) error {
	if intent.IsZero() || !entry.Valid() {
		return transfer.ErrInvalidOutputBinding
	}
	reservation, ok := intent.MaterializationPlan().DestinationReservation()
	if !ok || reservation.Kind() != receivecontract.ReservationNamedContainerEntry ||
		reservation.RequestedName() != entry.PreferredName() ||
		reservation.ReservedName() != entry.ReservedName() ||
		reservation.CollisionIndex() != entry.CollisionIndex() ||
		reservation.EntryKind() != entry.EntryKind() {
		return transfer.ErrInvalidOutputBinding
	}
	projector, err := transfer.OrdinaryOutputArtifactPathProjector(intent)
	if err != nil {
		return err
	}
	_ = projector
	return nil
}
