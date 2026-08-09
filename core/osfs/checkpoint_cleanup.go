package osfs

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/checkpointcleaner"
)

var (
	ErrCheckpointCleanerBusy      = errors.New("file checkpoint cleaner is already running")
	ErrCheckpointCleanerOwnership = errors.New("file checkpoint cleaner cannot prove namespace ownership")
	ErrCheckpointCleanerState     = errors.New("file checkpoint cleaner state is corrupt")
	ErrCheckpointCleanerLimit     = errors.New("file checkpoint cleaner inspection limit exceeded")
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

func projectCleanupReport(report checkpointcleaner.CheckpointCleanupReport) CheckpointCleanupReport {
	result := CheckpointCleanupReport{Status: CheckpointCleanupStatus(report.Status), Complete: report.Complete, Resumed: report.Resumed, Scanned: report.Scanned, Removed: report.Removed, Quarantined: report.Quarantined, Skipped: report.Skipped, Attention: slices.Clone(report.Attention)}
	result.Entries = make([]CheckpointCleanupEntry, len(report.Entries))
	for index, entry := range report.Entries {
		result.Entries[index] = CheckpointCleanupEntry{RelativePath: entry.RelativePath, Disposition: CheckpointCleanupDisposition(entry.Disposition), Detail: entry.Detail}
	}
	return result
}

// CleanLegacyResumeState is intentionally the sole native cleanup entry point.
// Requiring an explicit legacy operation keeps this maintenance path from being
// mistaken for current checkpoint resume or discard authority.
func CleanLegacyResumeState(
	ctx context.Context,
	root FilesystemResumeRoot,
) (report CheckpointCleanupReport, resultErr error) {
	if root.RootPath == "" || !filepath.IsAbs(root.RootPath) || filepath.Clean(root.RootPath) != root.RootPath {
		return CheckpointCleanupReport{}, ErrCheckpointCleanerOwnership
	}
	platform, err := openNativeOutputPlatform(root.RootPath, false)
	if err != nil {
		return CheckpointCleanupReport{}, err
	}
	if platform == nil {
		return CheckpointCleanupReport{}, ErrCheckpointCleanerOwnership
	}
	defer func() { resultErr = errors.Join(resultErr, platform.Close()) }()
	cleaner, err := checkpointcleaner.NewOneShotCheckpointCleaner(
		checkpointcleaner.OneShotCheckpointCleanerConfig{
			Platform: platform,
		},
	)
	if err != nil {
		return CheckpointCleanupReport{}, wrapCleanerError(err)
	}
	inner, err := cleaner.Run(ctx)
	return projectCleanupReport(inner), wrapCleanerError(err)
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
