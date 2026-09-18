package recovery

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

// Filesystem binds no handles until an operation is invoked. Native list and
// discard each reacquire the selected destination and release it before return.
func Filesystem(rootPath string) *FilesystemAuthority {
	return &FilesystemAuthority{rootPath: rootPath}
}

type FilesystemAuthority struct{ rootPath string }

func (authority *FilesystemAuthority) native() (osfs.ResumeStateAuthority, error) {
	if authority == nil {
		return nil, ErrContract
	}
	return osfs.NewFilesystemResumeStateAuthority(osfs.FilesystemResumeRoot{RootPath: authority.rootPath})
}

func (authority *FilesystemAuthority) List(ctx context.Context) (Snapshot, error) {
	native, err := authority.native()
	if err != nil {
		return Snapshot{}, err
	}
	inventory, err := native.ListResumeState(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return projectStateInventory(inventory)
}

func (authority *FilesystemAuthority) Discard(ctx context.Context, id receivecontract.OperationID) (DiscardReport, error) {
	native, err := authority.native()
	if err != nil {
		return DiscardReport{}, err
	}
	summary, discardErr := native.Discard(ctx, id)
	if !summary.Valid() {
		return DiscardReport{}, discardErr
	}
	report, projectionErr := projectDiscardSummary(summary)
	return report, errors.Join(projectionErr, discardErr)
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

func projectStateSummary(summary nativeSummaryView) (Operation, error) {
	if summary == nil || !summary.Valid() || summary.OperationID().IsZero() ||
		summary.ReceiveIntentDigest().IsZero() || summary.StateGeneration() == 0 {
		return Operation{}, ErrContract
	}
	state, err := projectOperationState(summary.State())
	if err != nil {
		return Operation{}, err
	}
	reason := summary.NeedsAttentionReason()
	attention := AttentionNone
	if reason != osfs.FilesystemOutputStateReasonNone {
		if !reason.Valid() {
			return Operation{}, ErrContract
		}
		attention = AttentionReason(reason.String())
	}
	operation := Operation{
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
			return Operation{}, err
		}
		operation.BlockedItems = append(operation.BlockedItems, blocked)
	}
	if !operation.Valid() {
		return Operation{}, ErrContract
	}
	return operation, nil
}

func projectOperationState(state osfs.ResumeOperationState) (OperationState, error) {
	switch state {
	case osfs.ResumeOperationIncomplete:
		return OperationIncomplete, nil
	case osfs.ResumeOperationResumable:
		return OperationResumable, nil
	case osfs.ResumeOperationCleanupPending:
		return OperationCleanupPending, nil
	case osfs.ResumeOperationNeedsAttention:
		return OperationNeedsAttention, nil
	default:
		// Terminal cleanup leaves no resumable operation to list.
		return 0, ErrContract
	}
}

func projectBlockedItem(item nativeItemView) (BlockedItem, error) {
	if item == nil || item.State() != osfs.ResumeItemBlocked {
		return BlockedItem{}, ErrContract
	}
	reason, err := projectBlockedReason(item.BlockReason())
	if err != nil {
		return BlockedItem{}, err
	}
	projected := BlockedItem{
		ArtifactPath: item.CanonicalPath(),
		PathKnown:    item.CanonicalPath() != "",
		Reason:       reason,
	}
	// A diagnostic reference identifies a corrupt control record. It is authority
	// evidence, not a user artifact path, so it is intentionally never projected.
	if !projected.PathKnown && item.DiagnosticReference() == "" {
		return BlockedItem{}, ErrContract
	}
	if !projected.Valid() {
		return BlockedItem{}, ErrContract
	}
	return projected, nil
}

func projectBlockedReason(reason osfs.ResumeItemBlockReason) (BlockedReason, error) {
	switch reason {
	case osfs.ResumeItemBlockPublicationUnknown:
		return BlockedPublicationUnknown, nil
	case osfs.ResumeItemBlockCheckpointInvalid:
		return BlockedCheckpointInvalid, nil
	case osfs.ResumeItemBlockOwnedObjectUnknown:
		return BlockedOwnedObjectUnknown, nil
	case osfs.ResumeItemBlockRevisionConflict:
		return BlockedRevisionConflict, nil
	default:
		return 0, ErrContract
	}
}

func projectDiscardSummary(summary nativeSummaryView) (DiscardReport, error) {
	if summary == nil || !summary.Valid() || summary.OperationID().IsZero() {
		return DiscardReport{}, ErrContract
	}
	reason := summary.NeedsAttentionReason()
	attention := AttentionNone
	if reason != osfs.FilesystemOutputStateReasonNone {
		if !reason.Valid() {
			return DiscardReport{}, ErrContract
		}
		attention = AttentionReason(reason.String())
	}
	report := DiscardReport{
		ID:        summary.OperationID(),
		Attention: attention,
	}
	switch summary.State() {
	case osfs.ResumeOperationDiscarded:
		report.Status = Discarded
	case osfs.ResumeOperationCleanupPending:
		report.Status = DiscardCleanupPending
	case osfs.ResumeOperationNeedsAttention:
		report.Status = DiscardNeedsAttention
	default:
		return DiscardReport{}, ErrContract
	}
	for _, item := range summary.Items() {
		if item.State() != osfs.ResumeItemBlocked {
			continue
		}
		blocked, err := projectBlockedItem(item)
		if err != nil {
			return DiscardReport{}, err
		}
		report.BlockedItems = append(report.BlockedItems, blocked)
	}
	if !report.Valid() {
		return DiscardReport{}, ErrContract
	}
	return report, nil
}

func projectStateInventory(inventory osfs.ResumeStateInventory) (Snapshot, error) {
	switch inventory.Status() {
	case osfs.ResumeStateListReady, osfs.ResumeStateListNeedsAttention:
	default:
		return Snapshot{}, ErrContract
	}
	summaries := inventory.Summaries()
	operations := make([]Operation, 0, len(summaries))
	for index := range summaries {
		operation, err := projectStateSummary(summaries[index])
		if err != nil {
			return Snapshot{}, err
		}
		operations = append(operations, operation)
	}
	snapshot, err := NewSnapshot(operations, inventory.UnknownEntries())
	if err != nil {
		return Snapshot{}, err
	}
	needsAttention := snapshot.NeedsAttention()
	if (inventory.Status() == osfs.ResumeStateListReady && needsAttention) ||
		(inventory.Status() == osfs.ResumeStateListNeedsAttention && !needsAttention) {
		return Snapshot{}, ErrContract
	}
	return snapshot, nil
}
