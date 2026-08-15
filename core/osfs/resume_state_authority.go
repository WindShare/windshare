package osfs

import (
	"context"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var ErrResumeStateContract = resumeauthority.ErrInvalidContract

type ResumeOperationState uint8

const (
	ResumeOperationIncomplete ResumeOperationState = iota + 1
	ResumeOperationResumable
	ResumeOperationCleanupPending
	ResumeOperationNeedsAttention
	ResumeOperationDiscarded
)

func (state ResumeOperationState) String() string {
	return resumeauthority.OperationState(state).String()
}

type ResumeItemState uint8

const (
	ResumeItemIncomplete ResumeItemState = iota + 1
	ResumeItemResumable
	ResumeItemPublished
	ResumeItemFailed
	ResumeItemBlocked
)

func (state ResumeItemState) String() string {
	return resumeauthority.ItemState(state).String()
}

type ResumeItemBlockReason uint8

const (
	ResumeItemBlockNone ResumeItemBlockReason = iota + 1
	ResumeItemBlockPublicationUnknown
	ResumeItemBlockCheckpointInvalid
	ResumeItemBlockOwnedObjectUnknown
)

func (reason ResumeItemBlockReason) String() string {
	return resumeauthority.ItemBlockReason(reason).String()
}

type ResumeStateItem struct {
	path      string
	state     ResumeItemState
	reason    ResumeItemBlockReason
	reference string
}

func projectResumeStateItem(item resumeauthority.Item) ResumeStateItem {
	return ResumeStateItem{
		path: item.CanonicalPath(), state: ResumeItemState(item.State()),
		reason: ResumeItemBlockReason(item.BlockReason()), reference: item.Reference(),
	}
}

func (item ResumeStateItem) CanonicalPath() string              { return item.path }
func (item ResumeStateItem) State() ResumeItemState             { return item.state }
func (item ResumeStateItem) BlockReason() ResumeItemBlockReason { return item.reason }
func (item ResumeStateItem) DiagnosticReference() string        { return item.reference }

type ResumeStateSummary struct {
	inner resumeauthority.Summary
	items []ResumeStateItem
}

func projectResumeStateSummary(summary resumeauthority.Summary) ResumeStateSummary {
	items := summary.Items()
	projected := make([]ResumeStateItem, len(items))
	for index, item := range items {
		projected[index] = projectResumeStateItem(item)
	}
	return ResumeStateSummary{inner: summary, items: projected}
}

func (summary ResumeStateSummary) OperationID() receivecontract.OperationID {
	return summary.inner.OperationID()
}
func (summary ResumeStateSummary) ReceiveIntentDigest() transfer.ReceiveIntentDigest {
	return summary.inner.ReceiveIntentDigest()
}
func (summary ResumeStateSummary) State() ResumeOperationState {
	return ResumeOperationState(summary.inner.State())
}
func (summary ResumeStateSummary) StateGeneration() uint64 {
	return summary.inner.StateGeneration()
}
func (summary ResumeStateSummary) NeedsAttentionReason() string {
	switch summary.State() {
	case ResumeOperationNeedsAttention, ResumeOperationCleanupPending:
		return summary.inner.NeedsAttentionReason().String()
	default:
		return ""
	}
}
func (summary ResumeStateSummary) Items() []ResumeStateItem {
	return slices.Clone(summary.items)
}
func (summary ResumeStateSummary) Busy() bool {
	return summary.inner.Busy()
}
func (summary ResumeStateSummary) Resumable() bool {
	return summary.State() == ResumeOperationResumable
}
func (summary ResumeStateSummary) Valid() bool {
	return summary.inner.Valid()
}

type ResumeStateListStatus uint8

const (
	ResumeStateListReady ResumeStateListStatus = iota + 1
	ResumeStateListNeedsAttention
)

type ResumeStateInventory struct {
	status    ResumeStateListStatus
	summaries []ResumeStateSummary
	unknown   bool
}

func (inventory ResumeStateInventory) Status() ResumeStateListStatus { return inventory.status }
func (inventory ResumeStateInventory) Summaries() []ResumeStateSummary {
	return slices.Clone(inventory.summaries)
}
func (inventory ResumeStateInventory) UnknownEntries() bool { return inventory.unknown }

type ResumeStateAuthority interface {
	ListResumeState(context.Context) (ResumeStateInventory, error)
	Discard(context.Context, receivecontract.OperationID) (ResumeStateSummary, error)
}

type RepositoryResumeStateAuthority struct {
	inner *resumeauthority.Authority
}

func newResumeStateAuthority(
	repository resumeauthority.Store,
) (*RepositoryResumeStateAuthority, error) {
	inner, err := resumeauthority.New(repository)
	if err != nil {
		return nil, err
	}
	return &RepositoryResumeStateAuthority{inner: inner}, nil
}

func (authority *RepositoryResumeStateAuthority) ListResumeState(
	ctx context.Context,
) (ResumeStateInventory, error) {
	if authority == nil || authority.inner == nil {
		return ResumeStateInventory{}, ErrResumeStateContract
	}
	inventory, err := authority.inner.List(ctx)
	if err != nil {
		return ResumeStateInventory{}, err
	}
	summaries := inventory.Summaries()
	projected := make([]ResumeStateSummary, len(summaries))
	for index, summary := range summaries {
		projected[index] = projectResumeStateSummary(summary)
	}
	return ResumeStateInventory{
		status: ResumeStateListStatus(inventory.Status()), summaries: projected,
		unknown: inventory.UnknownEntries(),
	}, nil
}

func (authority *RepositoryResumeStateAuthority) Discard(
	ctx context.Context,
	operation receivecontract.OperationID,
) (ResumeStateSummary, error) {
	if authority == nil || authority.inner == nil {
		return ResumeStateSummary{}, ErrResumeStateContract
	}
	summary, err := authority.inner.Discard(ctx, operation)
	return projectResumeStateSummary(summary), err
}

var _ ResumeStateAuthority = (*RepositoryResumeStateAuthority)(nil)
