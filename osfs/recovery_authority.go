package osfs

import (
	"context"

	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/output"
)

type (
	RecoveryInventory       = output.RecoveryInventory
	RecoverySummary         = output.RecoverySummary
	RecoveryItem            = output.RecoveryItem
	RecoveryListStatus      = output.RecoveryListStatus
	RecoveryOperationState  = output.RecoveryOperationState
	RecoveryItemState       = output.RecoveryItemState
	RecoveryItemBlockReason = output.RecoveryItemBlockReason
	RecoveryClosedReason    = output.AttentionReason
)

const (
	RecoveryListReady          = output.RecoveryListReady
	RecoveryListNeedsAttention = output.RecoveryListNeedsAttention

	RecoveryOperationIncomplete     = output.RecoveryOperationIncomplete
	RecoveryOperationResumable      = output.RecoveryOperationResumable
	RecoveryOperationShareStopped   = output.RecoveryOperationShareStopped
	RecoveryOperationCleanupPending = output.RecoveryOperationCleanupPending
	RecoveryOperationNeedsAttention = output.RecoveryOperationNeedsAttention
	RecoveryOperationDiscarded      = output.RecoveryOperationDiscarded

	RecoveryItemIncomplete = output.RecoveryItemIncomplete
	RecoveryItemResumable  = output.RecoveryItemResumable
	RecoveryItemPublished  = output.RecoveryItemPublished
	RecoveryItemFailed     = output.RecoveryItemFailed
	RecoveryItemBlocked    = output.RecoveryItemBlocked

	RecoveryItemBlockNone               = output.RecoveryItemBlockNone
	RecoveryItemBlockPublicationUnknown = output.RecoveryItemBlockPublicationUnknown
	RecoveryItemBlockCheckpointInvalid  = output.RecoveryItemBlockCheckpointInvalid
	RecoveryItemBlockOwnedObjectUnknown = output.RecoveryItemBlockOwnedObjectUnknown
	RecoveryItemBlockRevisionConflict   = output.RecoveryItemBlockRevisionConflict

	RecoveryReasonNone                 = output.AttentionNone
	RecoveryReasonDestinationOwnership = output.AttentionDestination
	RecoveryReasonRegistryOwnership    = output.AttentionRegistry
	RecoveryReasonLeaseOwnership       = output.AttentionLease
	RecoveryReasonOperationOwnership   = output.AttentionOperation
	RecoveryReasonCleanupUncertain     = output.AttentionCleanup
)

// RecoveryAuthority inspects and discards retained native receive state.
// It binds no handles until List or Discard is called, and releases them on return.
type RecoveryAuthority struct {
	rootPath string
}

func NewRecoveryAuthority(rootPath string) *RecoveryAuthority {
	return &RecoveryAuthority{rootPath: rootPath}
}

func (authority *RecoveryAuthority) List(ctx context.Context) (RecoveryInventory, error) {
	if authority == nil {
		return RecoveryInventory{}, ErrRecoveryContract
	}
	return output.ListOperations(ctx, authority.rootPath)
}

func (authority *RecoveryAuthority) Discard(
	ctx context.Context,
	id receivecontract.OperationID,
) (RecoverySummary, error) {
	if authority == nil {
		return RecoverySummary{}, ErrRecoveryContract
	}
	return output.DiscardOperation(ctx, authority.rootPath, id)
}
