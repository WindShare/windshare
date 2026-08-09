package osfs

import (
	"context"
	"errors"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var ErrResumeStateContract = resumeauthority.ErrInvalidContract

type ResumeEvidenceState uint8

const (
	ResumeEvidenceAbsent ResumeEvidenceState = iota + 1
	ResumeEvidenceProven
	ResumeEvidenceUnknown
)

type ResumeCleanupEvidenceState uint8

const (
	ResumeCleanupPending ResumeCleanupEvidenceState = iota + 1
	ResumeCleanupComplete
	ResumeCleanupUnknown
)

type ResumeStateRepositorySnapshot struct {
	OperationRecord []byte
	LifecycleRecord []byte
}

type ResumeStateRecoveryEvidence struct {
	TargetOwnership ResumeEvidenceState
	Checkpoints     ResumeEvidenceState
	Cleanup         ResumeCleanupEvidenceState
	TerminalReceipt []byte
	ExpiryReceipt   []byte
}

type ResumeStateDiscardEvidence struct {
	State   ResumeCleanupEvidenceState
	Receipt []byte
}

// ResumeStateRepository is an authority-bearing port. Canonical records cross
// it as bytes so adapters cannot manufacture internal model values; the core
// decoder revalidates them after every fresh-process lease acquisition.
type ResumeStateRepository interface {
	List(context.Context) ([]ResumeStateRepositorySnapshot, error)
	Acquire(context.Context, receivecontract.OperationID) (ResumeStateRepositoryLease, error)
}

type ResumeStateRepositoryLease interface {
	Snapshot(context.Context) (ResumeStateRepositorySnapshot, error)
	ObserveRecovery(context.Context) (ResumeStateRecoveryEvidence, error)
	CleanupOwned(context.Context) (ResumeStateDiscardEvidence, error)
	InstallReceipt(context.Context, []byte) error
	ReplaceLifecycle(context.Context, []byte, []byte) error
	Close() error
}

type ResumeStateSummary struct{ inner resumeauthority.Summary }

func (summary ResumeStateSummary) OperationID() receivecontract.OperationID {
	return summary.inner.OperationID()
}
func (summary ResumeStateSummary) ReceiveIntentDigest() transfer.ReceiveIntentDigest {
	return summary.inner.ReceiveIntentDigest()
}
func (summary ResumeStateSummary) Phase() uint8            { return uint8(summary.inner.Phase()) }
func (summary ResumeStateSummary) StateGeneration() uint64 { return summary.inner.StateGeneration() }
func (summary ResumeStateSummary) ExpiresAtMillis() uint64 { return summary.inner.ExpiresAtMillis() }
func (summary ResumeStateSummary) SuccessCount() uint64    { return summary.inner.SuccessCount() }
func (summary ResumeStateSummary) FailureCount() uint64    { return summary.inner.FailureCount() }
func (summary ResumeStateSummary) Resumable() bool         { return summary.inner.Resumable() }
func (summary ResumeStateSummary) NeedsAttentionReason() string {
	return summary.inner.NeedsAttentionReason().String()
}

type ResumeStateAttention struct{ inner resumeauthority.Attention }

func (attention ResumeStateAttention) OperationID() receivecontract.OperationID {
	return attention.inner.OperationID()
}
func (attention ResumeStateAttention) Reason() string { return attention.inner.Reason().String() }

type ResumeStateListStatus uint8

const (
	ResumeStateListReady ResumeStateListStatus = iota + 1
	ResumeStateListNeedsAttention
)

type ResumeStateInventory struct {
	status    ResumeStateListStatus
	summaries []ResumeStateSummary
	attention []ResumeStateAttention
}

func (inventory ResumeStateInventory) Status() ResumeStateListStatus { return inventory.status }
func (inventory ResumeStateInventory) Summaries() []ResumeStateSummary {
	return slices.Clone(inventory.summaries)
}
func (inventory ResumeStateInventory) Attention() []ResumeStateAttention {
	return slices.Clone(inventory.attention)
}

type ResumeStateAuthority interface {
	ListResumeState(context.Context) (ResumeStateInventory, error)
	Recover(context.Context, receivecontract.OperationID, uint64) (ResumeStateSummary, error)
	Discard(context.Context, receivecontract.OperationID) (ResumeStateSummary, error)
}

type RepositoryResumeStateAuthority struct{ inner *resumeauthority.Authority }

func NewResumeStateAuthority(repository ResumeStateRepository) (*RepositoryResumeStateAuthority, error) {
	if repository == nil {
		return nil, ErrResumeStateContract
	}
	inner, err := resumeauthority.New(repositoryBridge{repository: repository})
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
	result := ResumeStateInventory{status: ResumeStateListStatus(inventory.Status())}
	for _, summary := range inventory.Summaries() {
		result.summaries = append(result.summaries, ResumeStateSummary{inner: summary})
	}
	for _, attention := range inventory.Attention() {
		result.attention = append(result.attention, ResumeStateAttention{inner: attention})
	}
	return result, nil
}

func (authority *RepositoryResumeStateAuthority) Recover(
	ctx context.Context,
	operation receivecontract.OperationID,
	nowMillis uint64,
) (ResumeStateSummary, error) {
	if authority == nil || authority.inner == nil {
		return ResumeStateSummary{}, ErrResumeStateContract
	}
	summary, err := authority.inner.Recover(ctx, operation, nowMillis)
	return ResumeStateSummary{inner: summary}, err
}

func (authority *RepositoryResumeStateAuthority) Discard(
	ctx context.Context,
	operation receivecontract.OperationID,
) (ResumeStateSummary, error) {
	if authority == nil || authority.inner == nil {
		return ResumeStateSummary{}, ErrResumeStateContract
	}
	summary, err := authority.inner.Discard(ctx, operation)
	return ResumeStateSummary{inner: summary}, err
}

var _ ResumeStateAuthority = (*RepositoryResumeStateAuthority)(nil)

type repositoryBridge struct{ repository ResumeStateRepository }

func (bridge repositoryBridge) List(ctx context.Context) ([]resumeauthority.Snapshot, error) {
	records, err := bridge.repository.List(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]resumeauthority.Snapshot, len(records))
	for index, record := range records {
		result[index], err = decodeResumeSnapshot(record)
		if err != nil {
			// List retains structurally identifiable corrupt entries as invalid
			// snapshots so the authority can project NeedsAttention, never resume.
			operation, operationErr := checkpointmodel.DecodeReceiveOperation(record.OperationRecord)
			if operationErr != nil {
				return nil, errors.Join(ErrResumeStateContract, err)
			}
			result[index] = resumeauthority.CorruptSnapshot(operation)
		}
	}
	return result, nil
}

func (bridge repositoryBridge) Acquire(
	ctx context.Context,
	operation receivecontract.OperationID,
) (resumeauthority.OperationLease, error) {
	lease, err := bridge.repository.Acquire(ctx, operation)
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return nil, ErrResumeStateContract
	}
	return &leaseBridge{lease: lease}, nil
}

type leaseBridge struct{ lease ResumeStateRepositoryLease }

func (bridge *leaseBridge) Snapshot(ctx context.Context) (resumeauthority.Snapshot, error) {
	record, err := bridge.lease.Snapshot(ctx)
	if err != nil {
		return resumeauthority.Snapshot{}, err
	}
	return decodeResumeSnapshot(record)
}

func (bridge *leaseBridge) ObserveRecovery(
	ctx context.Context,
) (resumeauthority.RecoveryEvidence, error) {
	evidence, err := bridge.lease.ObserveRecovery(ctx)
	if err != nil {
		return resumeauthority.RecoveryEvidence{}, err
	}
	terminal, err := decodeOptionalReceipt(evidence.TerminalReceipt)
	if err != nil {
		return resumeauthority.RecoveryEvidence{}, err
	}
	expiry, err := decodeOptionalReceipt(evidence.ExpiryReceipt)
	if err != nil {
		return resumeauthority.RecoveryEvidence{}, err
	}
	return resumeauthority.RecoveryEvidence{
		TargetOwnership: resumeauthority.EvidenceState(evidence.TargetOwnership),
		Checkpoints:     resumeauthority.EvidenceState(evidence.Checkpoints),
		Cleanup:         resumeauthority.CleanupEvidenceState(evidence.Cleanup),
		TerminalReceipt: terminal, ExpiryReceipt: expiry,
	}, nil
}

func (bridge *leaseBridge) CleanupOwned(
	ctx context.Context,
) (resumeauthority.DiscardEvidence, error) {
	evidence, err := bridge.lease.CleanupOwned(ctx)
	if err != nil {
		return resumeauthority.DiscardEvidence{}, err
	}
	receipt, err := decodeOptionalReceipt(evidence.Receipt)
	if err != nil {
		return resumeauthority.DiscardEvidence{}, err
	}
	return resumeauthority.DiscardEvidence{
		State: resumeauthority.CleanupEvidenceState(evidence.State), Receipt: receipt,
	}, nil
}

func (bridge *leaseBridge) InstallReceipt(
	ctx context.Context,
	receipt checkpointmodel.DirectTreeReceipt,
) error {
	return bridge.lease.InstallReceipt(ctx, receipt.CanonicalBytes())
}

func (bridge *leaseBridge) ReplaceLifecycle(
	ctx context.Context,
	previous checkpointmodel.ReceiveLifecycleState,
	next checkpointmodel.ReceiveLifecycleState,
) error {
	previousBytes, previousErr := checkpointmodel.EncodeReceiveLifecycleState(previous)
	nextBytes, nextErr := checkpointmodel.EncodeReceiveLifecycleState(next)
	if previousErr != nil || nextErr != nil {
		return errors.Join(ErrResumeStateContract, previousErr, nextErr)
	}
	return bridge.lease.ReplaceLifecycle(ctx, previousBytes, nextBytes)
}

func (bridge *leaseBridge) Close() error { return bridge.lease.Close() }

func decodeResumeSnapshot(record ResumeStateRepositorySnapshot) (resumeauthority.Snapshot, error) {
	operation, operationErr := checkpointmodel.DecodeReceiveOperation(record.OperationRecord)
	lifecycle, lifecycleErr := checkpointmodel.DecodeReceiveLifecycleState(record.LifecycleRecord)
	if operationErr != nil || lifecycleErr != nil {
		return resumeauthority.Snapshot{}, errors.Join(ErrResumeStateContract, operationErr, lifecycleErr)
	}
	return resumeauthority.NewSnapshot(operation, lifecycle)
}

func decodeOptionalReceipt(encoded []byte) (checkpointmodel.DirectTreeReceipt, error) {
	if len(encoded) == 0 {
		return checkpointmodel.DirectTreeReceipt{}, nil
	}
	receipt, err := checkpointmodel.DecodeDirectTreeReceipt(encoded)
	if err != nil {
		return checkpointmodel.DirectTreeReceipt{}, errors.Join(ErrResumeStateContract, err)
	}
	return receipt, nil
}
