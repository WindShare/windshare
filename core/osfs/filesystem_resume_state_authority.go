package osfs

import (
	"context"
	"path/filepath"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/outputruntime"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

// ErrResumeStateBusy is stable across native providers so callers never need
// to inspect lock error strings or internal checkpoint fault wrappers.
var ErrResumeStateBusy = outputruntime.ErrNativeResumeBusy

// NewFilesystemResumeStateAuthority composes native certification and operation
// leases behind the repository-backed reducer. The root path selects what to
// certify; every list or mutation reacquires live filesystem authority.
func NewFilesystemResumeStateAuthority(
	root FilesystemResumeRoot,
) (ResumeStateAuthority, error) {
	if root.RootPath == "" || !filepath.IsAbs(root.RootPath) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	repository, err := outputruntime.NewNativeResumeRepository(
		filepath.Clean(root.RootPath),
		openNativeOutputPlatform,
	)
	if err != nil {
		return nil, err
	}
	return NewResumeStateAuthority(filesystemResumeRepository{inner: repository})
}

type filesystemResumeRepository struct {
	inner *outputruntime.NativeResumeRepository
}

func (repository filesystemResumeRepository) List(
	ctx context.Context,
) ([]ResumeStateRepositorySnapshot, error) {
	if repository.inner == nil {
		return nil, ErrResumeStateContract
	}
	records, err := repository.inner.List(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]ResumeStateRepositorySnapshot, len(records))
	for index, record := range records {
		result[index] = ResumeStateRepositorySnapshot{
			OperationRecord: slices.Clone(record.OperationRecord),
			LifecycleRecord: slices.Clone(record.LifecycleRecord),
		}
	}
	return result, nil
}

func (repository filesystemResumeRepository) Acquire(
	ctx context.Context,
	operation receivecontract.OperationID,
) (ResumeStateRepositoryLease, error) {
	if repository.inner == nil {
		return nil, ErrResumeStateContract
	}
	lease, err := repository.inner.Acquire(ctx, operation)
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return nil, ErrResumeStateContract
	}
	return &filesystemResumeRepositoryLease{inner: lease}, nil
}

type filesystemResumeRepositoryLease struct {
	inner *outputruntime.NativeResumeLease
}

func (lease *filesystemResumeRepositoryLease) Snapshot(
	ctx context.Context,
) (ResumeStateRepositorySnapshot, error) {
	if lease == nil || lease.inner == nil {
		return ResumeStateRepositorySnapshot{}, ErrResumeStateContract
	}
	snapshot, err := lease.inner.Snapshot(ctx)
	return ResumeStateRepositorySnapshot{
		OperationRecord: slices.Clone(snapshot.OperationRecord),
		LifecycleRecord: slices.Clone(snapshot.LifecycleRecord),
	}, err
}

func (lease *filesystemResumeRepositoryLease) ObserveRecovery(
	ctx context.Context,
) (ResumeStateRecoveryEvidence, error) {
	if lease == nil || lease.inner == nil {
		return ResumeStateRecoveryEvidence{}, ErrResumeStateContract
	}
	evidence, err := lease.inner.ObserveRecovery(ctx)
	if err != nil {
		return ResumeStateRecoveryEvidence{}, err
	}
	return ResumeStateRecoveryEvidence{
		TargetOwnership: projectNativeResumeEvidenceState(evidence.TargetOwnership),
		Checkpoints:     projectNativeResumeEvidenceState(evidence.Checkpoints),
		Cleanup:         projectNativeResumeCleanupState(evidence.Cleanup),
		TerminalReceipt: slices.Clone(evidence.TerminalReceipt),
		ExpiryReceipt:   slices.Clone(evidence.ExpiryReceipt),
	}, nil
}

func (lease *filesystemResumeRepositoryLease) CleanupOwned(
	ctx context.Context,
) (ResumeStateDiscardEvidence, error) {
	if lease == nil || lease.inner == nil {
		return ResumeStateDiscardEvidence{}, ErrResumeStateContract
	}
	evidence, err := lease.inner.CleanupOwned(ctx)
	if err != nil {
		return ResumeStateDiscardEvidence{}, err
	}
	return ResumeStateDiscardEvidence{
		State:   projectNativeResumeCleanupState(evidence.State),
		Receipt: slices.Clone(evidence.Receipt),
	}, nil
}

func (lease *filesystemResumeRepositoryLease) InstallReceipt(
	ctx context.Context,
	receipt []byte,
) error {
	if lease == nil || lease.inner == nil {
		return ErrResumeStateContract
	}
	return lease.inner.InstallReceipt(ctx, receipt)
}

func (lease *filesystemResumeRepositoryLease) ReplaceLifecycle(
	ctx context.Context,
	previous []byte,
	next []byte,
) error {
	if lease == nil || lease.inner == nil {
		return ErrResumeStateContract
	}
	return lease.inner.ReplaceLifecycle(ctx, previous, next)
}

func (lease *filesystemResumeRepositoryLease) Close() error {
	if lease == nil || lease.inner == nil {
		return nil
	}
	return lease.inner.Close()
}

func projectNativeResumeEvidenceState(
	state outputruntime.NativeResumeEvidenceState,
) ResumeEvidenceState {
	switch state {
	case outputruntime.NativeResumeEvidenceAbsent:
		return ResumeEvidenceAbsent
	case outputruntime.NativeResumeEvidenceProven:
		return ResumeEvidenceProven
	case outputruntime.NativeResumeEvidenceUnknown:
		return ResumeEvidenceUnknown
	default:
		return 0
	}
}

func projectNativeResumeCleanupState(
	state outputruntime.NativeResumeCleanupState,
) ResumeCleanupEvidenceState {
	switch state {
	case outputruntime.NativeResumeCleanupPending:
		return ResumeCleanupPending
	case outputruntime.NativeResumeCleanupComplete:
		return ResumeCleanupComplete
	case outputruntime.NativeResumeCleanupUnknown:
		return ResumeCleanupUnknown
	default:
		return 0
	}
}

var _ ResumeStateRepository = filesystemResumeRepository{}
var _ ResumeStateRepositoryLease = (*filesystemResumeRepositoryLease)(nil)
