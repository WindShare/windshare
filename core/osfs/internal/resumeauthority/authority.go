package resumeauthority

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type Store interface {
	List(context.Context) ([]Snapshot, error)
	Acquire(context.Context, receivecontract.OperationID) (OperationLease, error)
}

type OperationLease interface {
	Snapshot(context.Context) (Snapshot, error)
	ObserveRecovery(context.Context) (RecoveryEvidence, error)
	CleanupOwned(context.Context) (DiscardEvidence, error)
	InstallReceipt(context.Context, checkpointmodel.DirectTreeReceipt) error
	ReplaceLifecycle(
		context.Context,
		checkpointmodel.ReceiveLifecycleState,
		checkpointmodel.ReceiveLifecycleState,
	) error
	Close() error
}

type Authority struct{ store Store }

func New(store Store) (*Authority, error) {
	if store == nil {
		return nil, ErrInvalidContract
	}
	return &Authority{store: store}, nil
}

func (authority *Authority) List(ctx context.Context) (Inventory, error) {
	if authority == nil || authority.store == nil || ctx == nil {
		return Inventory{}, ErrInvalidContract
	}
	if err := ctx.Err(); err != nil {
		return Inventory{}, err
	}
	snapshots, err := authority.store.List(ctx)
	if err != nil {
		return Inventory{}, err
	}
	seen := make(map[receivecontract.OperationID]int)
	summaries := make([]Summary, 0, len(snapshots))
	attention := make([]Attention, 0)
	for _, snapshot := range snapshots {
		operation := snapshot.operation.OperationID()
		if !snapshot.Valid() {
			if !operation.IsZero() {
				current, _ := NewAttention(operation, checkpointmodel.AttentionTargetOwnershipUnknown)
				attention = append(attention, current)
			}
			continue
		}
		if prior, duplicate := seen[operation]; duplicate {
			summaries[prior] = Summary{}
			current, _ := NewAttention(operation, checkpointmodel.AttentionTargetOwnershipUnknown)
			attention = append(attention, current)
			continue
		}
		seen[operation] = len(summaries)
		summaries = append(summaries, summaryFromSnapshot(snapshot))
	}
	compacted := summaries[:0]
	for _, summary := range summaries {
		if !summary.operationID.IsZero() {
			compacted = append(compacted, summary)
		}
	}
	return newInventory(compacted, attention), nil
}

func (authority *Authority) Recover(
	ctx context.Context,
	operation receivecontract.OperationID,
	nowMillis uint64,
) (Summary, error) {
	return authority.mutate(ctx, operation, func(
		ctx context.Context,
		lease OperationLease,
		snapshot Snapshot,
	) (Decision, error) {
		evidence, err := lease.ObserveRecovery(ctx)
		if err != nil {
			return Decision{}, err
		}
		if err := validateEvidenceReceipts(snapshot, evidence); err != nil {
			return Decision{}, err
		}
		decision, err := ReduceRecovery(snapshot.lifecycle, evidence, nowMillis)
		if err != nil {
			return Decision{}, err
		}
		next, replace := decision.Next()
		if !replace || next.ReceiptDigest().IsZero() ||
			next.ReceiptDigest() == snapshot.lifecycle.ReceiptDigest() {
			return decision, nil
		}
		// Only the receipt selected by the reducer is installed. Unrelated valid
		// observations are not durable authority for this lifecycle transition.
		for _, receipt := range []checkpointmodel.DirectTreeReceipt{
			evidence.TerminalReceipt, evidence.ExpiryReceipt,
		} {
			if receipt.Valid() && receipt.Digest() == next.ReceiptDigest() {
				return decision, lease.InstallReceipt(ctx, receipt)
			}
		}
		return Decision{}, ErrInvalidContract
	})
}

func (authority *Authority) Discard(
	ctx context.Context,
	operation receivecontract.OperationID,
) (Summary, error) {
	return authority.mutate(ctx, operation, func(
		ctx context.Context,
		lease OperationLease,
		snapshot Snapshot,
	) (Decision, error) {
		evidence, err := lease.ObserveRecovery(ctx)
		if err != nil {
			return Decision{}, err
		}
		initial := DiscardEvidence{State: CleanupPending}
		decision, err := ReduceDiscard(snapshot.lifecycle, evidence.TargetOwnership, initial)
		if err != nil || decision.action != DecisionCleanupRequired {
			return decision, err
		}
		cleanup, err := lease.CleanupOwned(ctx)
		if err != nil {
			return Decision{}, err
		}
		if !cleanup.valid() {
			return Decision{}, ErrInvalidContract
		}
		if cleanup.Receipt.Valid() {
			if err := validateReceipt(snapshot, cleanup.Receipt); err != nil {
				return Decision{}, err
			}
			if err := lease.InstallReceipt(ctx, cleanup.Receipt); err != nil {
				return Decision{}, err
			}
		}
		return ReduceDiscard(snapshot.lifecycle, evidence.TargetOwnership, cleanup)
	})
}

func (authority *Authority) mutate(
	ctx context.Context,
	operation receivecontract.OperationID,
	reduce func(context.Context, OperationLease, Snapshot) (Decision, error),
) (result Summary, resultErr error) {
	if authority == nil || authority.store == nil || ctx == nil || operation.IsZero() || reduce == nil {
		return Summary{}, ErrInvalidContract
	}
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	lease, err := authority.store.Acquire(ctx, operation)
	if err != nil {
		return Summary{}, err
	}
	if lease == nil {
		return Summary{}, ErrInvalidContract
	}
	defer func() { resultErr = errors.Join(resultErr, lease.Close()) }()
	snapshot, err := lease.Snapshot(ctx)
	if err != nil || !snapshot.Valid() || snapshot.operation.OperationID() != operation {
		return Summary{}, errors.Join(ErrInvalidContract, err)
	}
	decision, err := reduce(ctx, lease, snapshot)
	if err != nil {
		return Summary{}, err
	}
	if next, replace := decision.Next(); replace {
		if err := lease.ReplaceLifecycle(ctx, snapshot.lifecycle, next); err != nil {
			return Summary{}, err
		}
		snapshot.lifecycle = next
	}
	return summaryFromSnapshot(snapshot), nil
}

func validateEvidenceReceipts(snapshot Snapshot, evidence RecoveryEvidence) error {
	if !evidence.valid() {
		return ErrInvalidContract
	}
	for _, receipt := range []checkpointmodel.DirectTreeReceipt{
		evidence.TerminalReceipt, evidence.ExpiryReceipt,
	} {
		if receipt.Valid() {
			if err := validateReceipt(snapshot, receipt); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateReceipt(snapshot Snapshot, receipt checkpointmodel.DirectTreeReceipt) error {
	if !receipt.Valid() || receipt.OperationID() != snapshot.operation.OperationID() ||
		receipt.ReceiveIntentDigest() != snapshot.operation.ReceiveIntentDigest() ||
		receipt.ReservationDigest() != snapshot.operation.BindingDigest() {
		return ErrInvalidContract
	}
	return nil
}
