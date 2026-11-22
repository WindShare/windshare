package osfs

import (
	"context"
	"path/filepath"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputruntime"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

type (
	RecoveryInventory       = resumeauthority.Inventory
	RecoverySummary         = resumeauthority.Summary
	RecoveryItem            = resumeauthority.Item
	RecoveryListStatus      = resumeauthority.ListStatus
	RecoveryOperationState  = resumeauthority.OperationState
	RecoveryItemState       = resumeauthority.ItemState
	RecoveryItemBlockReason = resumeauthority.ItemBlockReason
	RecoveryClosedReason    = checkpointmodel.OrdinaryClosedReason
)

const (
	RecoveryListReady          = resumeauthority.ListReady
	RecoveryListNeedsAttention = resumeauthority.ListNeedsAttention

	RecoveryOperationIncomplete     = resumeauthority.OperationIncomplete
	RecoveryOperationResumable      = resumeauthority.OperationResumable
	RecoveryOperationShareStopped   = resumeauthority.OperationShareStopped
	RecoveryOperationCleanupPending = resumeauthority.OperationCleanupPending
	RecoveryOperationNeedsAttention = resumeauthority.OperationNeedsAttention
	RecoveryOperationDiscarded      = resumeauthority.OperationDiscarded

	RecoveryItemIncomplete = resumeauthority.ItemIncomplete
	RecoveryItemResumable  = resumeauthority.ItemResumable
	RecoveryItemPublished  = resumeauthority.ItemPublished
	RecoveryItemFailed     = resumeauthority.ItemFailed
	RecoveryItemBlocked    = resumeauthority.ItemBlocked

	RecoveryItemBlockNone               = resumeauthority.ItemBlockNone
	RecoveryItemBlockPublicationUnknown = resumeauthority.ItemBlockPublicationUnknown
	RecoveryItemBlockCheckpointInvalid  = resumeauthority.ItemBlockCheckpointInvalid
	RecoveryItemBlockOwnedObjectUnknown = resumeauthority.ItemBlockOwnedObjectUnknown
	RecoveryItemBlockRevisionConflict   = resumeauthority.ItemBlockRevisionConflict

	RecoveryReasonNone                 = checkpointmodel.OrdinaryReasonNone
	RecoveryReasonDestinationOwnership = checkpointmodel.OrdinaryReasonDestinationOwnershipUnknown
	RecoveryReasonRegistryOwnership    = checkpointmodel.OrdinaryReasonRegistryOwnershipUnknown
	RecoveryReasonLeaseOwnership       = checkpointmodel.OrdinaryReasonLeaseOwnershipUnknown
	RecoveryReasonOperationOwnership   = checkpointmodel.OrdinaryReasonOperationOwnershipUnknown
	RecoveryReasonCleanupUncertain     = checkpointmodel.OrdinaryReasonCleanupUncertain
)

// RecoveryAuthority inspects and discards retained native ordinary-v1 state.
// It binds no handles until List or Discard is called, and each call reacquires
// destination ownership afresh and releases it on return.
type RecoveryAuthority struct {
	rootPath string
	store    resumeauthority.Store
}

func NewRecoveryAuthority(rootPath string) *RecoveryAuthority {
	return &RecoveryAuthority{rootPath: rootPath}
}

func newRecoveryAuthorityWithStore(store resumeauthority.Store) *RecoveryAuthority {
	return &RecoveryAuthority{store: store}
}

func (authority *RecoveryAuthority) open() (*resumeauthority.Authority, error) {
	if authority == nil {
		return nil, ErrRecoveryContract
	}
	store := authority.store
	if store == nil {
		if authority.rootPath == "" || !filepath.IsAbs(authority.rootPath) {
			return nil, transfer.ErrInvalidOutputBinding
		}
		var err error
		store, err = outputruntime.NewNativeResumeRepository(
			filepath.Clean(authority.rootPath),
			openNativeOutputPlatform,
			nil,
		)
		if err != nil {
			return nil, err
		}
	}
	return resumeauthority.New(store)
}

func (authority *RecoveryAuthority) List(ctx context.Context) (RecoveryInventory, error) {
	native, err := authority.open()
	if err != nil {
		return RecoveryInventory{}, err
	}
	return native.List(ctx)
}

func (authority *RecoveryAuthority) Discard(
	ctx context.Context,
	id receivecontract.OperationID,
) (RecoverySummary, error) {
	native, err := authority.open()
	if err != nil {
		return RecoverySummary{}, err
	}
	return native.Discard(ctx, id)
}
