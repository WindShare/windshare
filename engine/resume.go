package engine

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/internal/recovery"
	"github.com/windshare/windshare/engine/internal/task"
)

type RecoveryObservation = recovery.Observation
type RecoveryPhase = recovery.Phase
type RecoveryAuthority = recovery.Authority
type FilesystemRecoveryAuthority = recovery.FilesystemAuthority
type RecoverySnapshot = recovery.Snapshot
type RecoveryOperation = recovery.Operation
type RecoveryOperationState = recovery.OperationState
type RecoveryBlockedItem = recovery.BlockedItem
type RecoveryBlockedReason = recovery.BlockedReason
type RecoveryAttentionReason = recovery.AttentionReason
type RecoveryDiscardStatus = recovery.DiscardStatus
type RecoveryDiscardReport = recovery.DiscardReport
type RecoveryDiscardResult = recovery.DiscardResult
type RecoveryFailure = recovery.Failure
type RecoveryFailureKind = recovery.FailureKind
type RecoveryFailureDetail = recovery.FailureDetail

const (
	RecoveryInspectStarted   = recovery.InspectStarted
	RecoveryInspectCompleted = recovery.InspectCompleted
	RecoveryDiscardStarted   = recovery.DiscardStarted
	RecoveryDiscardCompleted = recovery.DiscardCompleted

	RecoveryIncomplete                = recovery.OperationIncomplete
	RecoveryResumable                 = recovery.OperationResumable
	RecoveryCleanupPending            = recovery.OperationCleanupPending
	RecoveryNeedsAttention            = recovery.OperationNeedsAttention
	RecoveryBlockedPublicationUnknown = recovery.BlockedPublicationUnknown
	RecoveryBlockedCheckpointInvalid  = recovery.BlockedCheckpointInvalid
	RecoveryBlockedOwnedObjectUnknown = recovery.BlockedOwnedObjectUnknown
	RecoveryBlockedRevisionConflict   = recovery.BlockedRevisionConflict
	RecoveryAttentionNone             = recovery.AttentionNone
	RecoveryAttentionDestination      = recovery.AttentionDestination
	RecoveryAttentionRegistry         = recovery.AttentionRegistry
	RecoveryAttentionLease            = recovery.AttentionLease
	RecoveryAttentionOperation        = recovery.AttentionOperation
	RecoveryAttentionCleanup          = recovery.AttentionCleanup
	RecoveryDiscarded                 = recovery.Discarded
	RecoveryDiscardCleanupPending     = recovery.DiscardCleanupPending
	RecoveryDiscardNeedsAttention     = recovery.DiscardNeedsAttention
	RecoveryFailureNeedsAttention     = recovery.FailureNeedsAttention
	RecoveryFailureBusy               = recovery.FailureBusy
	RecoveryFailureChanged            = recovery.FailureChanged
	RecoveryFailureCancelled          = recovery.FailureCancelled
)

var ErrRecoveryContract = recovery.ErrContract
var ErrRecoveryBusy = osfs.ErrResumeStateBusy

func FilesystemRecovery(rootPath string) *FilesystemRecoveryAuthority {
	return recovery.Filesystem(rootPath)
}

func NewRecoverySnapshot(operations []RecoveryOperation, registryUnknown bool) (RecoverySnapshot, error) {
	return recovery.NewSnapshot(operations, registryUnknown)
}

func RecoveryDestinationFailure(err error) *RecoveryFailure {
	return recovery.DestinationFailure(err)
}

// RecoveryInventory retains only listed identity evidence. The destination is
// reacquired for each discard, so a prompt never holds a filesystem lease.
type RecoveryInventory struct {
	application  *Engine
	inventory    *recovery.Inventory
	observations <-chan Observation
}

func (application *Engine) InspectRecovery(ctx context.Context, authority RecoveryAuthority) (*RecoveryInventory, error) {
	if application == nil {
		return nil, recovery.DestinationFailure(recovery.ErrContract)
	}
	current, err := start(application, ctx, func(ctx context.Context, control task.Control) task.Completion[*recovery.Inventory] {
		control.Emit(recovery.Observation{Phase: recovery.InspectStarted})
		inventory, err := recovery.Inspect(ctx, authority)
		event := recovery.Observation{Phase: recovery.InspectCompleted, Failed: err != nil}
		if err == nil {
			snapshot, _ := inventory.Snapshot()
			event.OperationCount = len(snapshot.Operations)
			event.NeedsAttention = snapshot.NeedsAttention()
		} else {
			if failure, ok := errors.AsType[*recovery.Failure](err); ok {
				event.FailureKind = failure.Kind
			}
		}
		control.Emit(event)
		return task.Completion[*recovery.Inventory]{Value: inventory, Settlement: settleRecovery(err, recovery.AuthorityCloseFailure(err))}
	})
	if err != nil {
		return nil, recovery.DestinationFailure(err)
	}
	// Caller cancellation stops the registered work, but the synchronous boundary
	// still joins its native leases before returning authority to the application.
	result, err := current.Wait(context.Background())
	if err != nil {
		return nil, recovery.DestinationFailure(err)
	}
	if result.Err != nil {
		if failure, ok := errors.AsType[*recovery.Failure](result.Err); ok {
			failure.Observations = current.Observations()
		}
		return nil, result.Err
	}
	return &RecoveryInventory{application: application, inventory: result.Value, observations: current.Observations()}, nil
}

// Observations is complete when InspectRecovery returns. Draining or ignoring
// this bounded history has no effect on subsequent identity-bound discard.
func (inventory *RecoveryInventory) Observations() <-chan Observation {
	if inventory == nil {
		return nil
	}
	return inventory.observations
}

func (inventory *RecoveryInventory) Snapshot() (RecoverySnapshot, error) {
	if inventory == nil {
		return RecoverySnapshot{}, recovery.DestinationFailure(recovery.ErrContract)
	}
	return inventory.inventory.Snapshot()
}

func (inventory *RecoveryInventory) DiscardRestriction() *RecoveryFailure {
	if inventory == nil {
		return (*recovery.Inventory)(nil).DiscardRestriction()
	}
	return inventory.inventory.DiscardRestriction()
}

func (inventory *RecoveryInventory) CheckDiscard(id receivecontract.OperationID) *RecoveryFailure {
	if inventory == nil {
		return (*recovery.Inventory)(nil).CheckDiscard(id)
	}
	return inventory.inventory.CheckDiscard(id)
}

func (inventory *RecoveryInventory) Discard(ctx context.Context, id receivecontract.OperationID) RecoveryDiscardResult {
	if inventory == nil || inventory.application == nil {
		failure := (*recovery.Inventory)(nil).CheckDiscard(id)
		return RecoveryDiscardResult{Failure: failure, Err: failure}
	}
	current, err := start(inventory.application, ctx, func(ctx context.Context, control task.Control) task.Completion[RecoveryDiscardResult] {
		control.Emit(recovery.Observation{Phase: recovery.DiscardStarted, OperationID: id})
		result := inventory.inventory.Discard(ctx, id)
		event := recovery.Observation{
			Phase: recovery.DiscardCompleted, OperationID: id,
			DiscardStatus: result.Report.Status, Failed: !result.Successful(),
		}
		if result.Failure != nil {
			event.FailureKind = result.Failure.Kind
		}
		control.Emit(event)
		failure := result.Err
		if result.Failure != nil {
			failure = result.Failure
		} else if !result.Successful() && failure == nil {
			failure = &recovery.Failure{Kind: recovery.FailureNeedsAttention, Reason: string(result.Report.Status)}
		}
		return task.Completion[RecoveryDiscardResult]{Value: result, Settlement: settleRecovery(failure, result.CleanupError)}
	})
	if err != nil {
		failure := recovery.DestinationFailure(err)
		return RecoveryDiscardResult{Failure: failure, Err: err}
	}
	result, err := current.Wait(context.Background())
	if err != nil {
		failure := recovery.DestinationFailure(err)
		return RecoveryDiscardResult{Failure: failure, Err: err}
	}
	result.Value.Observations = current.Observations()
	return result.Value
}

func settleRecovery(err, cleanupErr error) task.Settlement {
	settlement := task.Settlement{Outcome: task.OutcomeSuccess}
	if err == nil && cleanupErr == nil {
		return settlement
	}
	settlement = task.Settlement{Outcome: task.OutcomeFailed, FailureClass: task.FailureLocal,
		Err: errors.Join(err, cleanupErr), CleanupError: cleanupErr}
	failure, classified := errors.AsType[*recovery.Failure](err)
	// Joined cancellation may also contain a real failure. Only a bare cause or
	// the recovery workflow's explicit classification can establish cancellation.
	//nolint:errorlint
	if cleanupErr == nil && (err == context.Canceled || err == context.DeadlineExceeded || classified && failure.Kind == recovery.FailureCancelled) {
		settlement.Outcome, settlement.FailureClass = task.OutcomeCancelled, task.FailureNone
	}
	return settlement
}
