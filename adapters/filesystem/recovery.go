package filesystem

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/recovery"
	"github.com/windshare/windshare/osfs"
)

// Recovery binds no handles until an operation is invoked. Native list and
// discard each reacquire the selected destination and release it before return.
func Recovery(rootPath string) recovery.Authority {
	return &recoveryAuthority{native: osfs.NewRecoveryAuthority(rootPath)}
}

type recoveryAuthority struct {
	native *osfs.RecoveryAuthority
}

var _ recovery.Authority = (*recoveryAuthority)(nil)

func (authority *recoveryAuthority) List(ctx context.Context) (recovery.Snapshot, error) {
	inventory, err := authority.native.List(ctx)
	if err != nil {
		return recovery.Snapshot{}, sealRecoveryError(err)
	}
	return projectRecoveryInventory(inventory)
}

func (authority *recoveryAuthority) Discard(
	ctx context.Context,
	id receivecontract.OperationID,
) (recovery.DiscardReport, error) {
	summary, discardErr := authority.native.Discard(ctx, id)
	discardErr = sealRecoveryError(discardErr)
	if !summary.Valid() {
		return recovery.DiscardReport{}, discardErr
	}
	report, projectionErr := projectDiscardSummary(summary)
	return report, errors.Join(projectionErr, discardErr)
}

func sealRecoveryError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, osfs.ErrRecoveryContract) && !errors.Is(err, recovery.ErrContract) {
		err = errors.Join(recovery.ErrContract, err)
	}
	return sealOutputError(err)
}

func projectRecoveryInventory(inventory osfs.RecoveryInventory) (recovery.Snapshot, error) {
	switch inventory.Status() {
	case osfs.RecoveryListReady, osfs.RecoveryListNeedsAttention:
	default:
		return recovery.Snapshot{}, recovery.ErrContract
	}
	summaries := inventory.Summaries()
	operations := make([]recovery.Operation, 0, len(summaries))
	for _, summary := range summaries {
		operation, err := projectRecoverySummary(summary)
		if err != nil {
			return recovery.Snapshot{}, err
		}
		operations = append(operations, operation)
	}
	snapshot, err := recovery.NewSnapshot(operations, inventory.UnknownEntries())
	if err != nil {
		return recovery.Snapshot{}, err
	}
	needsAttention := snapshot.NeedsAttention()
	if (inventory.Status() == osfs.RecoveryListReady && needsAttention) ||
		(inventory.Status() == osfs.RecoveryListNeedsAttention && !needsAttention) {
		return recovery.Snapshot{}, recovery.ErrContract
	}
	return snapshot, nil
}

func projectRecoverySummary(summary osfs.RecoverySummary) (recovery.Operation, error) {
	if !summary.Valid() || summary.OperationID().IsZero() ||
		summary.ReceiveIntentDigest().IsZero() || summary.StateGeneration() == 0 {
		return recovery.Operation{}, recovery.ErrContract
	}
	state, err := projectOperationState(summary.State())
	if err != nil {
		return recovery.Operation{}, err
	}
	attention, err := projectAttentionReason(summary.NeedsAttentionReason())
	if err != nil {
		return recovery.Operation{}, err
	}
	operation := recovery.Operation{
		ID:        summary.OperationID(),
		State:     state,
		Attention: attention,
		Running:   summary.Busy(),
	}
	for _, item := range summary.Items() {
		if item.State() != osfs.RecoveryItemBlocked {
			continue
		}
		blocked, err := projectBlockedItem(item)
		if err != nil {
			return recovery.Operation{}, err
		}
		operation.BlockedItems = append(operation.BlockedItems, blocked)
	}
	if !operation.Valid() {
		return recovery.Operation{}, recovery.ErrContract
	}
	return operation, nil
}

func projectDiscardSummary(summary osfs.RecoverySummary) (recovery.DiscardReport, error) {
	if !summary.Valid() || summary.OperationID().IsZero() {
		return recovery.DiscardReport{}, recovery.ErrContract
	}
	attention, err := projectAttentionReason(summary.NeedsAttentionReason())
	if err != nil {
		return recovery.DiscardReport{}, err
	}
	report := recovery.DiscardReport{
		ID:        summary.OperationID(),
		Attention: attention,
	}
	switch summary.State() {
	case osfs.RecoveryOperationDiscarded:
		report.Status = recovery.Discarded
	case osfs.RecoveryOperationCleanupPending:
		report.Status = recovery.DiscardCleanupPending
	case osfs.RecoveryOperationNeedsAttention:
		report.Status = recovery.DiscardNeedsAttention
	default:
		return recovery.DiscardReport{}, recovery.ErrContract
	}
	for _, item := range summary.Items() {
		if item.State() != osfs.RecoveryItemBlocked {
			continue
		}
		blocked, err := projectBlockedItem(item)
		if err != nil {
			return recovery.DiscardReport{}, err
		}
		report.BlockedItems = append(report.BlockedItems, blocked)
	}
	if !report.Valid() {
		return recovery.DiscardReport{}, recovery.ErrContract
	}
	return report, nil
}

func projectOperationState(state osfs.RecoveryOperationState) (recovery.OperationState, error) {
	switch state {
	case osfs.RecoveryOperationIncomplete:
		return recovery.OperationIncomplete, nil
	case osfs.RecoveryOperationResumable:
		return recovery.OperationResumable, nil
	case osfs.RecoveryOperationShareStopped:
		return recovery.OperationShareStopped, nil
	case osfs.RecoveryOperationCleanupPending:
		return recovery.OperationCleanupPending, nil
	case osfs.RecoveryOperationNeedsAttention:
		return recovery.OperationNeedsAttention, nil
	default:
		// Terminal cleanup leaves no resumable operation to list.
		return 0, recovery.ErrContract
	}
}

func projectAttentionReason(reason osfs.RecoveryClosedReason) (recovery.AttentionReason, error) {
	switch reason {
	case osfs.RecoveryReasonNone:
		return recovery.AttentionNone, nil
	case osfs.RecoveryReasonDestinationOwnership:
		return recovery.AttentionDestination, nil
	case osfs.RecoveryReasonRegistryOwnership:
		return recovery.AttentionRegistry, nil
	case osfs.RecoveryReasonLeaseOwnership:
		return recovery.AttentionLease, nil
	case osfs.RecoveryReasonOperationOwnership:
		return recovery.AttentionOperation, nil
	case osfs.RecoveryReasonCleanupUncertain:
		return recovery.AttentionCleanup, nil
	default:
		return "", recovery.ErrContract
	}
}

func projectBlockedItem(item osfs.RecoveryItem) (recovery.BlockedItem, error) {
	if item.State() != osfs.RecoveryItemBlocked {
		return recovery.BlockedItem{}, recovery.ErrContract
	}
	reason, err := projectBlockedReason(item.BlockReason())
	if err != nil {
		return recovery.BlockedItem{}, err
	}
	projected := recovery.BlockedItem{
		ArtifactPath: item.CanonicalPath(),
		PathKnown:    item.CanonicalPath() != "",
		Reason:       reason,
	}
	// A diagnostic reference identifies a corrupt control record. It is authority
	// evidence, not a user artifact path, so it is intentionally never projected.
	if !projected.PathKnown && item.Reference() == "" {
		return recovery.BlockedItem{}, recovery.ErrContract
	}
	if !projected.Valid() {
		return recovery.BlockedItem{}, recovery.ErrContract
	}
	return projected, nil
}

func projectBlockedReason(reason osfs.RecoveryItemBlockReason) (recovery.BlockedReason, error) {
	switch reason {
	case osfs.RecoveryItemBlockPublicationUnknown:
		return recovery.BlockedPublicationUnknown, nil
	case osfs.RecoveryItemBlockCheckpointInvalid:
		return recovery.BlockedCheckpointInvalid, nil
	case osfs.RecoveryItemBlockOwnedObjectUnknown:
		return recovery.BlockedOwnedObjectUnknown, nil
	case osfs.RecoveryItemBlockRevisionConflict:
		return recovery.BlockedRevisionConflict, nil
	default:
		return 0, recovery.ErrContract
	}
}
