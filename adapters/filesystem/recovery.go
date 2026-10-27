package filesystem

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/engine/recovery"
	"github.com/windshare/windshare/osfs"
)

// Recovery binds no handles until an operation is invoked. Native list and
// discard each reacquire the selected destination and release it before return.
func Recovery(rootPath string) *FilesystemAuthority {
	return &FilesystemAuthority{rootPath: rootPath}
}

type FilesystemAuthority struct{ rootPath string }

func (authority *FilesystemAuthority) native() (osfs.ResumeStateAuthority, error) {
	if authority == nil {
		return nil, recovery.ErrContract
	}
	return osfs.NewFilesystemResumeStateAuthority(osfs.FilesystemResumeRoot{RootPath: authority.rootPath})
}

func (authority *FilesystemAuthority) List(ctx context.Context) (recovery.Snapshot, error) {
	native, err := authority.native()
	if err != nil {
		return recovery.Snapshot{}, sealFilesystemOutputFailure(err)
	}
	inventory, err := native.ListResumeState(ctx)
	if err != nil {
		return recovery.Snapshot{}, sealFilesystemOutputFailure(err)
	}
	return projectStateInventory(inventory)
}

func (authority *FilesystemAuthority) Discard(ctx context.Context, id receivecontract.OperationID) (recovery.DiscardReport, error) {
	native, err := authority.native()
	if err != nil {
		return recovery.DiscardReport{}, sealFilesystemOutputFailure(err)
	}
	summary, discardErr := native.Discard(ctx, id)
	if !summary.Valid() {
		return recovery.DiscardReport{}, sealFilesystemOutputFailure(discardErr)
	}
	report, projectionErr := projectDiscardSummary(summary)
	return report, errors.Join(projectionErr, sealFilesystemOutputFailure(discardErr))
}

type nativeSummaryView interface {
	OperationID() receivecontract.OperationID
	ReceiveIntentDigest() transfer.ReceiveIntentDigest
	State() osfs.ResumeOperationState
	StateGeneration() uint64
	NeedsAttentionReason() osfs.FilesystemOutputStateReason
	Items() []osfs.ResumeStateItem
	Busy() bool
	Valid() bool
}

type nativeItemView interface {
	CanonicalPath() string
	State() osfs.ResumeItemState
	BlockReason() osfs.ResumeItemBlockReason
	DiagnosticReference() string
}

func projectStateSummary(summary nativeSummaryView) (recovery.Operation, error) {
	if summary == nil || !summary.Valid() || summary.OperationID().IsZero() ||
		summary.ReceiveIntentDigest().IsZero() || summary.StateGeneration() == 0 {
		return recovery.Operation{}, recovery.ErrContract
	}
	state, err := projectOperationState(summary.State())
	if err != nil {
		return recovery.Operation{}, err
	}
	reason := summary.NeedsAttentionReason()
	attention := recovery.AttentionNone
	if reason != osfs.FilesystemOutputStateReasonNone {
		if !reason.Valid() {
			return recovery.Operation{}, recovery.ErrContract
		}
		attention = recovery.AttentionReason(reason.String())
	}
	operation := recovery.Operation{
		ID:        summary.OperationID(),
		State:     state,
		Attention: attention,
		Running:   summary.Busy(),
	}
	for _, item := range summary.Items() {
		if item.State() != osfs.ResumeItemBlocked {
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

func projectOperationState(state osfs.ResumeOperationState) (recovery.OperationState, error) {
	switch state {
	case osfs.ResumeOperationIncomplete:
		return recovery.OperationIncomplete, nil
	case osfs.ResumeOperationResumable:
		return recovery.OperationResumable, nil
	case osfs.ResumeOperationCleanupPending:
		return recovery.OperationCleanupPending, nil
	case osfs.ResumeOperationRecoveryUnavailable, osfs.ResumeOperationNeedsAttention:
		return recovery.OperationNeedsAttention, nil
	default:
		// Terminal cleanup leaves no resumable operation to list.
		return 0, recovery.ErrContract
	}
}

func projectBlockedItem(item nativeItemView) (recovery.BlockedItem, error) {
	if item == nil || item.State() != osfs.ResumeItemBlocked {
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
	if !projected.PathKnown && item.DiagnosticReference() == "" {
		return recovery.BlockedItem{}, recovery.ErrContract
	}
	if !projected.Valid() {
		return recovery.BlockedItem{}, recovery.ErrContract
	}
	return projected, nil
}

func projectBlockedReason(reason osfs.ResumeItemBlockReason) (recovery.BlockedReason, error) {
	switch reason {
	case osfs.ResumeItemBlockPublicationUnknown:
		return recovery.BlockedPublicationUnknown, nil
	case osfs.ResumeItemBlockCheckpointInvalid:
		return recovery.BlockedCheckpointInvalid, nil
	case osfs.ResumeItemBlockOwnedObjectUnknown:
		return recovery.BlockedOwnedObjectUnknown, nil
	case osfs.ResumeItemBlockRevisionConflict:
		return recovery.BlockedRevisionConflict, nil
	default:
		return 0, recovery.ErrContract
	}
}

func projectDiscardSummary(summary nativeSummaryView) (recovery.DiscardReport, error) {
	if summary == nil || !summary.Valid() || summary.OperationID().IsZero() {
		return recovery.DiscardReport{}, recovery.ErrContract
	}
	reason := summary.NeedsAttentionReason()
	attention := recovery.AttentionNone
	if reason != osfs.FilesystemOutputStateReasonNone {
		if !reason.Valid() {
			return recovery.DiscardReport{}, recovery.ErrContract
		}
		attention = recovery.AttentionReason(reason.String())
	}
	report := recovery.DiscardReport{
		ID:        summary.OperationID(),
		Attention: attention,
	}
	switch summary.State() {
	case osfs.ResumeOperationDiscarded:
		report.Status = recovery.Discarded
	case osfs.ResumeOperationCleanupPending:
		report.Status = recovery.DiscardCleanupPending
	case osfs.ResumeOperationNeedsAttention:
		report.Status = recovery.DiscardNeedsAttention
	default:
		return recovery.DiscardReport{}, recovery.ErrContract
	}
	for _, item := range summary.Items() {
		if item.State() != osfs.ResumeItemBlocked {
			continue
		}
		blocked, err := projectBlockedItem(item)
		if err != nil {
			return recovery.DiscardReport{}, sealFilesystemOutputFailure(err)
		}
		report.BlockedItems = append(report.BlockedItems, blocked)
	}
	if !report.Valid() {
		return recovery.DiscardReport{}, recovery.ErrContract
	}
	return report, nil
}

func projectStateInventory(inventory osfs.ResumeStateInventory) (recovery.Snapshot, error) {
	switch inventory.Status() {
	case osfs.ResumeStateListReady, osfs.ResumeStateListNeedsAttention:
	default:
		return recovery.Snapshot{}, recovery.ErrContract
	}
	summaries := inventory.Summaries()
	operations := make([]recovery.Operation, 0, len(summaries))
	for index := range summaries {
		operation, err := projectStateSummary(summaries[index])
		if err != nil {
			return recovery.Snapshot{}, sealFilesystemOutputFailure(err)
		}
		operations = append(operations, operation)
	}
	snapshot, err := recovery.NewSnapshot(operations, inventory.UnknownEntries())
	if err != nil {
		return recovery.Snapshot{}, sealFilesystemOutputFailure(err)
	}
	needsAttention := snapshot.NeedsAttention()
	if (inventory.Status() == osfs.ResumeStateListReady && needsAttention) ||
		(inventory.Status() == osfs.ResumeStateListNeedsAttention && !needsAttention) {
		return recovery.Snapshot{}, recovery.ErrContract
	}
	return snapshot, nil
}
