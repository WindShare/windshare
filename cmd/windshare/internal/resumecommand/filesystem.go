package resumecommand

import (
	"context"
	"encoding/hex"
	"io"
	"sort"

	"github.com/windshare/windshare/core/osfs"
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
		legacy:      filesystemLegacyResumeCleaner{},
		confirmation: newStdioResumeConfirmationTerminal(
			config.Input,
			config.RawTerminalOutput,
			config.SerializedTerminalOutput,
		),
		parser: flagRequestParser{
			output: config.SerializedTerminalOutput,
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
	items, err := projectResumeStateInventory(inventory)
	if err != nil {
		return nil, err
	}
	return &filesystemResumeStateInventory{
		authority: authority, items: items,
	}, nil
}

type filesystemResumeStateInventory struct {
	authority osfs.ResumeStateAuthority
	items     []resumeStateItem
}

func (inventory *filesystemResumeStateInventory) Items() ([]resumeStateItem, error) {
	if inventory == nil || inventory.authority == nil {
		return nil, errResumeStateContract
	}
	items := append([]resumeStateItem(nil), inventory.items...)
	for index := range items {
		items[index].attention = append([]resumeStateAttention(nil), items[index].attention...)
	}
	return items, nil
}

func (inventory *filesystemResumeStateInventory) Discard(
	ctx context.Context,
	index int,
) (resumeDiscardReport, error) {
	if inventory == nil || inventory.authority == nil || index < 0 || index >= len(inventory.items) ||
		!inventory.items[index].valid() || !inventory.items[index].discardable {
		return resumeDiscardReport{}, errResumeStateContract
	}
	operation, err := decodeResumeOperationID(inventory.items[index].operationID)
	if err != nil {
		return resumeDiscardReport{}, err
	}
	result, err := inventory.authority.Discard(ctx, operation)
	if err != nil {
		return resumeDiscardReport{}, err
	}
	report, err := projectResumeDiscardSummary(result)
	if err != nil || report.operationID != inventory.items[index].operationID {
		return resumeDiscardReport{}, errResumeStateContract
	}
	return report, nil
}

func projectResumeStateSummary(summary osfs.ResumeStateSummary) (resumeStateItem, error) {
	operation := summary.OperationID()
	intent := summary.ReceiveIntentDigest()
	if operation.IsZero() || intent.IsZero() || summary.Phase() == 0 || summary.StateGeneration() == 0 {
		return resumeStateItem{}, errResumeStateContract
	}
	item := resumeStateItem{
		status:          resumeItemStatusRecorded,
		operationID:     hex.EncodeToString(operation.Bytes()),
		intentDigest:    hex.EncodeToString(intent.Bytes()),
		phase:           summary.Phase(),
		stateGeneration: summary.StateGeneration(),
		expiresAtMillis: summary.ExpiresAtMillis(),
		successCount:    summary.SuccessCount(),
		failureCount:    summary.FailureCount(),
		resumable:       summary.Resumable(),
		discardable:     true,
	}
	if item.resumable {
		item.status = resumeItemStatusResumable
	}
	if reason := summary.NeedsAttentionReason(); reason != "" {
		attention, err := newResumeStateAttention(operation, reason)
		if err != nil {
			return resumeStateItem{}, err
		}
		item.status = resumeItemStatusNeedsAttention
		item.attention = []resumeStateAttention{attention}
	}
	if !item.valid() {
		return resumeStateItem{}, errResumeStateContract
	}
	return item, nil
}

func projectResumeDiscardSummary(summary osfs.ResumeStateSummary) (resumeDiscardReport, error) {
	item, err := projectResumeStateSummary(summary)
	if err != nil {
		return resumeDiscardReport{}, err
	}
	report := resumeDiscardReport{
		status:          resumeDiscardStatusSettled,
		operationID:     item.operationID,
		phase:           item.phase,
		stateGeneration: item.stateGeneration,
		resumable:       item.resumable,
		attention:       append([]resumeStateAttention(nil), item.attention...),
	}
	if len(report.attention) != 0 {
		report.status = resumeDiscardStatusNeedsAttention
	}
	if !report.valid() {
		return resumeDiscardReport{}, errResumeStateContract
	}
	return report, nil
}

func projectResumeAttention(values []osfs.ResumeStateAttention) ([]resumeStateAttention, error) {
	projected := make([]resumeStateAttention, len(values))
	for index, value := range values {
		attention, err := newResumeStateAttention(value.OperationID(), value.Reason())
		if err != nil {
			return nil, err
		}
		projected[index] = attention
	}
	return projected, nil
}

func projectResumeStateInventory(inventory osfs.ResumeStateInventory) ([]resumeStateItem, error) {
	switch inventory.Status() {
	case osfs.ResumeStateListReady, osfs.ResumeStateListNeedsAttention:
	default:
		return nil, errResumeStateContract
	}
	items := make([]resumeStateItem, 0, len(inventory.Summaries())+len(inventory.Attention()))
	byOperation := make(map[string]int)
	for _, summary := range inventory.Summaries() {
		item, err := projectResumeStateSummary(summary)
		if err != nil {
			return nil, err
		}
		if _, duplicate := byOperation[item.operationID]; duplicate {
			return nil, errResumeStateContract
		}
		byOperation[item.operationID] = len(items)
		items = append(items, item)
	}
	attention, err := projectResumeAttention(inventory.Attention())
	if err != nil {
		return nil, err
	}
	if inventory.Status() == osfs.ResumeStateListReady && len(attention) != 0 {
		return nil, errResumeStateContract
	}
	for _, current := range attention {
		if index, present := byOperation[current.operationID]; present {
			items[index].status = resumeItemStatusNeedsAttention
			items[index].resumable = false
			items[index].attention = append(items[index].attention, current)
			continue
		}
		byOperation[current.operationID] = len(items)
		items = append(items, resumeStateItem{
			status: resumeItemStatusNeedsAttention, operationID: current.operationID,
			attention: []resumeStateAttention{current},
		})
	}
	if inventory.Status() == osfs.ResumeStateListNeedsAttention && len(attention) == 0 {
		return nil, errResumeStateContract
	}
	sort.Slice(items, func(left, right int) bool { return items[left].operationID < items[right].operationID })
	for _, item := range items {
		if !item.valid() {
			return nil, errResumeStateContract
		}
	}
	return items, nil
}

func newResumeStateAttention(
	operation receivecontract.OperationID,
	reason string,
) (resumeStateAttention, error) {
	attention := resumeStateAttention{
		reason: reason, operationID: hex.EncodeToString(operation.Bytes()),
	}
	if !attention.valid() {
		return resumeStateAttention{}, errResumeStateContract
	}
	return attention, nil
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

type legacyCleanupFunc func(
	context.Context,
	osfs.FilesystemResumeRoot,
) (osfs.CheckpointCleanupReport, error)

type filesystemLegacyResumeCleaner struct {
	clean legacyCleanupFunc
}

func (cleaner filesystemLegacyResumeCleaner) CleanLegacy(
	ctx context.Context,
	rootPath string,
) (osfs.CheckpointCleanupReport, error) {
	clean := cleaner.clean
	if clean == nil {
		clean = osfs.CleanLegacyResumeState
	}
	return clean(ctx, osfs.FilesystemResumeRoot{RootPath: rootPath})
}
