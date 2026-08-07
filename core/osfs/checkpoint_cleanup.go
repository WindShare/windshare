package osfs

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/checkpointcleaner"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

// Public cleaner projection keeps internal cleanup authority out of the facade.
type CheckpointCleanupDisposition uint8

const (
	CheckpointCleanupSkip CheckpointCleanupDisposition = iota + 1
	CheckpointCleanupRemove
	CheckpointCleanupQuarantine
)

type CheckpointCleanupStatus uint8

const (
	CheckpointCleanupStatusComplete CheckpointCleanupStatus = iota + 1
	CheckpointCleanupStatusNeedsAttention
	CheckpointCleanupStatusInProgress
)

type CheckpointCleanupStep struct {
	Index        uint32
	RelativePath string
	Disposition  CheckpointCleanupDisposition
}
type CheckpointCleanupFault func(CheckpointCleanupStep) error
type OneShotCheckpointCleanerConfig struct {
	RootPath string
	Fault    CheckpointCleanupFault
}
type CheckpointCleanupEntry struct {
	RelativePath string
	Disposition  CheckpointCleanupDisposition
	Detail       string
}
type CheckpointCleanupReport struct {
	Status      CheckpointCleanupStatus
	Complete    bool
	Resumed     bool
	Scanned     uint64
	Removed     uint64
	Quarantined uint64
	Skipped     uint64
	Entries     []CheckpointCleanupEntry
	Attention   []string
}

func (report CheckpointCleanupReport) NeedsAttention() bool {
	return report.Status == CheckpointCleanupStatusNeedsAttention || len(report.Attention) != 0
}

type OneShotCheckpointCleaner struct {
	config OneShotCheckpointCleanerConfig
}

func internalCleanerConfig(
	platform outputcap.Platform,
	config OneShotCheckpointCleanerConfig,
) checkpointcleaner.OneShotCheckpointCleanerConfig {
	result := checkpointcleaner.OneShotCheckpointCleanerConfig{
		Platform: platform, BackendID: transfer.NativeFilesystemOutputBackendID,
	}
	if config.Fault != nil {
		result.Fault = func(step checkpointcleaner.CheckpointCleanupStep) error {
			return config.Fault(CheckpointCleanupStep{Index: step.Index, RelativePath: step.RelativePath, Disposition: CheckpointCleanupDisposition(step.Disposition)})
		}
	}
	return result
}
func NewOneShotCheckpointCleaner(config OneShotCheckpointCleanerConfig) (*OneShotCheckpointCleaner, error) {
	if config.RootPath == "" || !filepath.IsAbs(config.RootPath) || filepath.Clean(config.RootPath) != config.RootPath {
		return nil, ErrCheckpointCleanerOwnership
	}
	return &OneShotCheckpointCleaner{config: config}, nil
}

func NewOwnedNamespaceCleaner(config OneShotCheckpointCleanerConfig) (*OneShotCheckpointCleaner, error) {
	return NewOneShotCheckpointCleaner(config)
}
func (cleaner *OneShotCheckpointCleaner) Run(ctx context.Context) (CheckpointCleanupReport, error) {
	if cleaner == nil {
		return CheckpointCleanupReport{}, ErrCheckpointCleanerOwnership
	}
	platform, err := openNativeOutputPlatform(cleaner.config.RootPath, false)
	if err != nil {
		return CheckpointCleanupReport{}, err
	}
	defer platform.Close()
	inner, err := checkpointcleaner.NewOneShotCheckpointCleaner(internalCleanerConfig(platform, cleaner.config))
	if err != nil {
		return CheckpointCleanupReport{}, wrapCleanerError(err)
	}
	report, err := inner.Run(ctx)
	return projectCleanupReport(report), wrapCleanerError(err)
}
func projectCleanupReport(report checkpointcleaner.CheckpointCleanupReport) CheckpointCleanupReport {
	result := CheckpointCleanupReport{Status: CheckpointCleanupStatus(report.Status), Complete: report.Complete, Resumed: report.Resumed, Scanned: report.Scanned, Removed: report.Removed, Quarantined: report.Quarantined, Skipped: report.Skipped, Attention: slices.Clone(report.Attention)}
	result.Entries = make([]CheckpointCleanupEntry, len(report.Entries))
	for index, entry := range report.Entries {
		result.Entries[index] = CheckpointCleanupEntry{RelativePath: entry.RelativePath, Disposition: CheckpointCleanupDisposition(entry.Disposition), Detail: entry.Detail}
	}
	return result
}
func CleanLegacyResumeState(ctx context.Context, root FilesystemResumeRoot) (CheckpointCleanupReport, error) {
	return RunOneShotCheckpointCleanup(ctx, OneShotCheckpointCleanerConfig{RootPath: root.RootPath})
}
func RunOneShotCheckpointCleanup(ctx context.Context, config OneShotCheckpointCleanerConfig) (CheckpointCleanupReport, error) {
	cleaner, err := NewOneShotCheckpointCleaner(config)
	if err != nil {
		return CheckpointCleanupReport{}, err
	}
	return cleaner.Run(ctx)
}
func CleanOwnedNamespace(ctx context.Context, config OneShotCheckpointCleanerConfig) (CheckpointCleanupReport, error) {
	return RunOneShotCheckpointCleanup(ctx, config)
}
func wrapCleanerError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, checkpointcleaner.ErrCheckpointCleanerBusy):
		return fmt.Errorf("%w: %w", ErrCheckpointCleanerBusy, err)
	case errors.Is(err, checkpointcleaner.ErrCheckpointCleanerOwnership):
		return fmt.Errorf("%w: %w", ErrCheckpointCleanerOwnership, err)
	case errors.Is(err, checkpointcleaner.ErrCheckpointCleanerState):
		return fmt.Errorf("%w: %w", ErrCheckpointCleanerState, err)
	case errors.Is(err, checkpointcleaner.ErrCheckpointCleanerLimit):
		return fmt.Errorf("%w: %w", ErrCheckpointCleanerLimit, err)
	default:
		return err
	}
}
