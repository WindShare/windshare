package checkpointcleaner

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

const (
	FileCheckpointCleanupState = "cleanup.state"
	FileCheckpointCleanupLock  = "cleanup.lock"

	maxCleanerEntries    = 100_000
	maxCleanerStateBytes = resumestate.MaxSessionHeaderBytes
)

var (
	ErrCheckpointCleanerBusy      = errors.New("file checkpoint cleaner is already running")
	ErrCheckpointCleanerOwnership = errors.New("file checkpoint cleaner cannot prove namespace ownership")
	ErrCheckpointCleanerState     = errors.New("file checkpoint cleaner state is corrupt")
	ErrCheckpointCleanerLimit     = errors.New("file checkpoint cleaner inspection limit exceeded")
)

type CheckpointCleanupDisposition uint8

const (
	CheckpointCleanupSkip CheckpointCleanupDisposition = iota + 1
	CheckpointCleanupRemove
	CheckpointCleanupQuarantine
)

type CheckpointCleanupStep struct {
	Index        uint32
	RelativePath string
	Disposition  CheckpointCleanupDisposition
}

type CheckpointCleanupFault func(CheckpointCleanupStep) error

// OneShotCheckpointCleanerConfig carries a live certified native platform.
// Pathnames and caller-supplied root digests are intentionally excluded because
// neither can authorize destructive cleanup after root replacement.
type OneShotCheckpointCleanerConfig struct {
	Platform  outputcap.Platform
	BackendID transfer.OutputBackendID
	Fault     CheckpointCleanupFault
}

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
	return report.Status == CheckpointCleanupStatusNeedsAttention || report.Quarantined != 0 ||
		len(report.Attention) != 0
}

type OneShotCheckpointCleaner struct {
	config OneShotCheckpointCleanerConfig
}

func NewOneShotCheckpointCleaner(config OneShotCheckpointCleanerConfig) (*OneShotCheckpointCleaner, error) {
	if config.Platform == nil || config.Platform.Root() == nil {
		return nil, fmt.Errorf("%w: certified platform", ErrCheckpointCleanerOwnership)
	}
	if config.BackendID == "" {
		config.BackendID = transfer.NativeFilesystemOutputBackendID
	}
	if _, err := transfer.NewOutputBackendID(string(config.BackendID)); err != nil {
		return nil, fmt.Errorf("%w: backend ID", ErrCheckpointCleanerOwnership)
	}
	return &OneShotCheckpointCleaner{config: config}, nil
}

func NewOwnedNamespaceCleaner(config OneShotCheckpointCleanerConfig) (*OneShotCheckpointCleaner, error) {
	return NewOneShotCheckpointCleaner(config)
}

func (cleaner *OneShotCheckpointCleaner) Run(ctx context.Context) (
	report CheckpointCleanupReport,
	resultErr error,
) {
	if cleaner == nil || cleaner.config.Platform == nil {
		return CheckpointCleanupReport{}, ErrCheckpointCleanerOwnership
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CheckpointCleanupReport{}, err
	}
	run, attention, err := cleaner.prepareRun(ctx)
	if run != nil {
		defer func() { resultErr = errors.Join(resultErr, run.Close()) }()
	}
	if err != nil {
		return CheckpointCleanupReport{}, err
	}
	if len(attention) != 0 {
		return attentionReport(attention), nil
	}
	state, previousEncoded, resumed, err := run.beginState()
	if err != nil {
		return CheckpointCleanupReport{}, err
	}
	report.Resumed = resumed
	if err := run.cleanLegacyNamespace(ctx, &state, &previousEncoded, &report); err != nil {
		return report, err
	}
	state.Complete = true
	if err := run.persistState(&state, &previousEncoded); err != nil {
		return report, err
	}
	report.Status = CheckpointCleanupStatusComplete
	report.Complete = true
	return report, nil
}

func attentionReport(attention []string) CheckpointCleanupReport {
	return CheckpointCleanupReport{
		Status:    CheckpointCleanupStatusNeedsAttention,
		Attention: append([]string(nil), attention...),
	}
}

func RunOneShotCheckpointCleanup(
	ctx context.Context,
	config OneShotCheckpointCleanerConfig,
) (CheckpointCleanupReport, error) {
	cleaner, err := NewOneShotCheckpointCleaner(config)
	if err != nil {
		return CheckpointCleanupReport{}, err
	}
	return cleaner.Run(ctx)
}

func CleanOwnedNamespace(
	ctx context.Context,
	config OneShotCheckpointCleanerConfig,
) (CheckpointCleanupReport, error) {
	return RunOneShotCheckpointCleanup(ctx, config)
}
