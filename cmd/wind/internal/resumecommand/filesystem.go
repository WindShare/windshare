package resumecommand

import (
	"context"
	"encoding/hex"
	"errors"
	"io"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

// FilesystemConfig keeps raw terminal detection separate from serialized writes;
// the CLI's stderr lock must not hide the underlying terminal file descriptor.
type FilesystemConfig struct {
	Input                    io.Reader
	Output                   io.Writer
	RawTerminalOutput        io.Writer
	SerializedTerminalOutput io.Writer
	Logf                     func(string, ...any)
}

func NewFilesystemRunner(config FilesystemConfig) Runner {
	logger := logFunc(config.Logf)
	return newRunner(resumeDependencies{
		inventories: filesystemResumeStateInventoryOpener{},
		confirmation: newStdioResumeConfirmationTerminal(
			config.Input,
			config.RawTerminalOutput,
			config.SerializedTerminalOutput,
		),
		parser: flagRequestParser{
			logger: logger,
		},
		renderer: textRenderer{},
		output: streamResumeOutput{
			result: config.Output,
			usage:  config.SerializedTerminalOutput,
		},
		logger: logger,
	})
}

type filesystemResumeStateInventoryOpener struct{}

func (filesystemResumeStateInventoryOpener) OpenResumeStateInventory(
	ctx context.Context,
	rootPath string,
) (resumeStateInventory, error) {
	authority, err := osfs.NewFilesystemResumeStateAuthority(
		osfs.FilesystemResumeRoot{RootPath: rootPath},
	)
	if err != nil {
		return nil, err
	}
	inventory, err := authority.ListResumeState(ctx)
	if err != nil {
		return nil, err
	}
	snapshot, err := projectResumeStateInventory(inventory)
	if err != nil {
		return nil, err
	}
	return &filesystemResumeStateInventory{authority: authority, snapshot: snapshot}, nil
}

type filesystemResumeStateInventory struct {
	authority osfs.ResumeStateAuthority
	snapshot  resumeInventorySnapshot
}

func (inventory *filesystemResumeStateInventory) Snapshot() (resumeInventorySnapshot, error) {
	if inventory == nil || inventory.authority == nil || !inventory.snapshot.valid() {
		return resumeInventorySnapshot{}, errResumeStateContract
	}
	return inventory.snapshot.clone(), nil
}

func (inventory *filesystemResumeStateInventory) Discard(
	ctx context.Context,
	index int,
) (resumeDiscardReport, error) {
	if inventory == nil || inventory.authority == nil || !inventory.snapshot.valid() ||
		inventory.snapshot.registryUnknown || index < 0 || index >= len(inventory.snapshot.operations) {
		return resumeDiscardReport{}, errResumeStateContract
	}
	selected := inventory.snapshot.operations[index]
	if !selected.valid() || selected.running {
		return resumeDiscardReport{}, osfs.ErrResumeStateBusy
	}
	operation, err := decodeResumeOperationID(selected.operationID)
	if err != nil {
		return resumeDiscardReport{}, err
	}
	summary, discardErr := inventory.authority.Discard(ctx, operation)
	if !summary.Valid() {
		return resumeDiscardReport{}, errors.Join(errResumeStateContract, discardErr)
	}
	report, projectionErr := projectResumeDiscardSummary(summary)
	if projectionErr != nil || report.operationID != selected.operationID {
		return resumeDiscardReport{}, errResumeStateContract
	}
	return report, discardErr
}

type resumeSummaryView interface {
	OperationID() receivecontract.OperationID
	ReceiveIntentDigest() transfer.ReceiveIntentDigest
	State() osfs.ResumeOperationState
	StateGeneration() uint64
	NeedsAttentionReason() osfs.FilesystemOutputStateReason
	Items() []osfs.ResumeStateItem
	Busy() bool
	Valid() bool
}

type resumeItemView interface {
	CanonicalPath() string
	State() osfs.ResumeItemState
	BlockReason() osfs.ResumeItemBlockReason
	DiagnosticReference() string
}

func projectResumeStateSummary(summary resumeSummaryView) (resumeOperation, error) {
	if summary == nil || !summary.Valid() || summary.OperationID().IsZero() ||
		summary.ReceiveIntentDigest().IsZero() || summary.StateGeneration() == 0 {
		return resumeOperation{}, errResumeStateContract
	}
	state, err := projectResumeOperationState(summary.State())
	if err != nil {
		return resumeOperation{}, err
	}
	reason := summary.NeedsAttentionReason()
	attention := ""
	if reason != osfs.FilesystemOutputStateReasonNone {
		if !reason.Valid() {
			return resumeOperation{}, errResumeStateContract
		}
		attention = reason.String()
	}
	operation := resumeOperation{
		operationID: hex.EncodeToString(summary.OperationID().Bytes()),
		state:       state,
		attention:   attention,
		running:     summary.Busy(),
	}
	for _, item := range summary.Items() {
		if item.State() != osfs.ResumeItemBlocked {
			continue
		}
		blocked, err := projectResumeBlockedItem(item)
		if err != nil {
			return resumeOperation{}, err
		}
		operation.blockedItems = append(operation.blockedItems, blocked)
	}
	if !operation.valid() {
		return resumeOperation{}, errResumeStateContract
	}
	return operation, nil
}

func projectResumeOperationState(state osfs.ResumeOperationState) (resumeOperationState, error) {
	switch state {
	case osfs.ResumeOperationIncomplete:
		return resumeOperationIncomplete, nil
	case osfs.ResumeOperationResumable:
		return resumeOperationResumable, nil
	case osfs.ResumeOperationCleanupPending:
		return resumeOperationCleanupPending, nil
	case osfs.ResumeOperationNeedsAttention:
		return resumeOperationNeedsAttention, nil
	default:
		// Discarded is a command outcome, not inventory history.
		return 0, errResumeStateContract
	}
}

func projectResumeBlockedItem(item resumeItemView) (resumeBlockedItem, error) {
	if item == nil || item.State() != osfs.ResumeItemBlocked {
		return resumeBlockedItem{}, errResumeStateContract
	}
	reason, err := projectResumeBlockedReason(item.BlockReason())
	if err != nil {
		return resumeBlockedItem{}, err
	}
	projected := resumeBlockedItem{
		artifactPath: item.CanonicalPath(),
		pathKnown:    item.CanonicalPath() != "",
		reason:       reason,
	}
	// A diagnostic reference identifies a corrupt control record. It is authority
	// evidence, not a user artifact path, so it is intentionally never projected.
	if !projected.pathKnown && item.DiagnosticReference() == "" {
		return resumeBlockedItem{}, errResumeStateContract
	}
	if !projected.valid() {
		return resumeBlockedItem{}, errResumeStateContract
	}
	return projected, nil
}

func projectResumeBlockedReason(reason osfs.ResumeItemBlockReason) (resumeBlockedReason, error) {
	switch reason {
	case osfs.ResumeItemBlockPublicationUnknown:
		return resumeBlockedPublicationUnknown, nil
	case osfs.ResumeItemBlockCheckpointInvalid:
		return resumeBlockedCheckpointInvalid, nil
	case osfs.ResumeItemBlockOwnedObjectUnknown:
		return resumeBlockedOwnedObjectUnknown, nil
	case osfs.ResumeItemBlockRevisionConflict:
		return resumeBlockedRevisionConflict, nil
	default:
		return 0, errResumeStateContract
	}
}

func projectResumeDiscardSummary(summary resumeSummaryView) (resumeDiscardReport, error) {
	if summary == nil || !summary.Valid() || summary.OperationID().IsZero() {
		return resumeDiscardReport{}, errResumeStateContract
	}
	reason := summary.NeedsAttentionReason()
	attention := ""
	if reason != osfs.FilesystemOutputStateReasonNone {
		if !reason.Valid() {
			return resumeDiscardReport{}, errResumeStateContract
		}
		attention = reason.String()
	}
	report := resumeDiscardReport{
		operationID: hex.EncodeToString(summary.OperationID().Bytes()),
		attention:   attention,
	}
	switch summary.State() {
	case osfs.ResumeOperationDiscarded:
		report.status = resumeDiscardStatusDiscarded
	case osfs.ResumeOperationCleanupPending:
		report.status = resumeDiscardStatusCleanupPending
	case osfs.ResumeOperationNeedsAttention:
		report.status = resumeDiscardStatusNeedsAttention
	default:
		return resumeDiscardReport{}, errResumeStateContract
	}
	for _, item := range summary.Items() {
		if item.State() != osfs.ResumeItemBlocked {
			continue
		}
		blocked, err := projectResumeBlockedItem(item)
		if err != nil {
			return resumeDiscardReport{}, err
		}
		report.blockedItems = append(report.blockedItems, blocked)
	}
	if !report.valid() {
		return resumeDiscardReport{}, errResumeStateContract
	}
	return report, nil
}

func projectResumeStateInventory(inventory osfs.ResumeStateInventory) (resumeInventorySnapshot, error) {
	switch inventory.Status() {
	case osfs.ResumeStateListReady, osfs.ResumeStateListNeedsAttention:
	default:
		return resumeInventorySnapshot{}, errResumeStateContract
	}
	summaries := inventory.Summaries()
	operations := make([]resumeOperation, 0, len(summaries))
	for index := range summaries {
		operation, err := projectResumeStateSummary(summaries[index])
		if err != nil {
			return resumeInventorySnapshot{}, err
		}
		operations = append(operations, operation)
	}
	snapshot, err := newResumeInventorySnapshot(operations, inventory.UnknownEntries())
	if err != nil {
		return resumeInventorySnapshot{}, err
	}
	needsAttention := snapshot.needsAttention()
	if (inventory.Status() == osfs.ResumeStateListReady && needsAttention) ||
		(inventory.Status() == osfs.ResumeStateListNeedsAttention && !needsAttention) {
		return resumeInventorySnapshot{}, errResumeStateContract
	}
	return snapshot, nil
}

func decodeResumeOperationID(encoded string) (receivecontract.OperationID, error) {
	raw, err := hex.DecodeString(encoded)
	if err != nil {
		return receivecontract.OperationID{}, errResumeStateContract
	}
	operation, err := receivecontract.OperationIDFromBytes(raw)
	if err != nil {
		return receivecontract.OperationID{}, errResumeStateContract
	}
	return operation, nil
}
