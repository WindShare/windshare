package resumecommand

import (
	"context"
	"encoding/hex"
	"io"

	"github.com/windshare/windshare/core/osfs"
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
	if inventory == nil {
		return nil, errResumeStateContract
	}
	return &filesystemResumeStateInventory{
		authority: authority,
		inventory: inventory,
		summaries: inventory.Summaries(),
	}, nil
}

type filesystemResumeStateInventory struct {
	authority osfs.ResumeStateAuthority
	inventory *osfs.ResumeStateInventory
	summaries []osfs.ResumeStateSummary
}

func (inventory *filesystemResumeStateInventory) Items() ([]resumeStateItem, error) {
	if inventory == nil || inventory.inventory == nil || inventory.authority == nil {
		return nil, errResumeStateContract
	}
	items := make([]resumeStateItem, len(inventory.summaries))
	for index, summary := range inventory.summaries {
		item, err := projectResumeStateSummary(summary)
		if err != nil {
			return nil, err
		}
		items[index] = item
	}
	return items, nil
}

func (inventory *filesystemResumeStateInventory) Discard(
	ctx context.Context,
	index int,
) (resumeDiscardReport, error) {
	if inventory == nil || inventory.inventory == nil || inventory.authority == nil ||
		index < 0 || index >= len(inventory.summaries) {
		return resumeDiscardReport{}, errResumeStateContract
	}
	result, err := inventory.authority.Discard(ctx, inventory.summaries[index].Reference())
	if err != nil {
		return resumeDiscardReport{}, err
	}
	return projectResumeDiscardResult(result)
}

func (inventory *filesystemResumeStateInventory) Close() error {
	if inventory == nil || inventory.inventory == nil {
		return nil
	}
	return inventory.inventory.Close()
}

func projectResumeStateSummary(summary osfs.ResumeStateSummary) (resumeStateItem, error) {
	status := ""
	switch summary.Status() {
	case osfs.ResumeStateAvailable:
		status = resumeListStatusAvailable
	case osfs.ResumeStateNeedsAttention:
		status = resumeListStatusNeedsAttention
	default:
		return resumeStateItem{}, errResumeStateContract
	}
	attention, err := projectResumeAttention(summary.Attention())
	if err != nil {
		return resumeStateItem{}, err
	}
	intentDigest := ""
	if digest := summary.Intent(); !digest.IsZero() {
		intentDigest = hex.EncodeToString(digest.Bytes())
	}
	item := resumeStateItem{
		status:                status,
		intentDigest:          intentDigest,
		backend:               string(summary.Backend()),
		checkpointRecordCount: summary.CheckpointRecordCount(),
		recoveryArtifactBytes: summary.RecoveryArtifactBytes(),
		attention:             attention,
	}
	if !item.valid() {
		return resumeStateItem{}, errResumeStateContract
	}
	return item, nil
}

func projectResumeDiscardResult(result osfs.ResumeStateDiscardResult) (resumeDiscardReport, error) {
	status := ""
	switch result.Status() {
	case osfs.ResumeStateDiscarded:
		status = resumeDiscardStatusDiscarded
	case osfs.ResumeStateAlreadyAbsent:
		status = resumeDiscardStatusAlreadyAbsent
	case osfs.ResumeStateDiscardNeedsAttention:
		status = resumeDiscardStatusNeedsAttention
	default:
		return resumeDiscardReport{}, errResumeStateContract
	}
	attention, err := projectResumeAttention(result.Attention())
	if err != nil {
		return resumeDiscardReport{}, err
	}
	report := resumeDiscardReport{
		status:           status,
		removedArtifacts: result.RemovedArtifacts(),
		attention:        attention,
	}
	if !report.valid() {
		return resumeDiscardReport{}, errResumeStateContract
	}
	return report, nil
}

func projectResumeAttention(values []osfs.ResumeStateAttention) ([]resumeStateAttention, error) {
	projected := make([]resumeStateAttention, len(values))
	for index, value := range values {
		reason := ""
		switch value.Reason() {
		case osfs.ResumeStateAttentionMissingOwnership:
			reason = "missing-ownership"
		case osfs.ResumeStateAttentionReplacement:
			reason = "replacement"
		case osfs.ResumeStateAttentionUnknownChildren:
			reason = "unknown-children"
		case osfs.ResumeStateAttentionCorruptBinding:
			reason = "corrupt-binding"
		case osfs.ResumeStateAttentionAmbiguousPublication:
			reason = "ambiguous-publication"
		default:
			return nil, errResumeStateContract
		}
		projected[index] = resumeStateAttention{
			reason:    reason,
			reference: value.Reference(),
		}
	}
	return projected, nil
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
