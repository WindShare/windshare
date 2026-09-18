package recovery

import (
	"context"
	"errors"
	"slices"

	"github.com/windshare/windshare/core/transfer/receivecontract"
)

// Authority reacquires the destination and exact operation ownership for each
// call. A prior list is evidence for user selection, never deletion authority.
// Discard must release its leases before returning, preserving any close error.
type Authority interface {
	List(context.Context) (Snapshot, error)
	Discard(context.Context, receivecontract.OperationID) (DiscardReport, error)
}

type Inventory struct {
	authority Authority
	snapshot  Snapshot
}

func Inspect(ctx context.Context, authority Authority) (*Inventory, error) {
	if ctx == nil || authority == nil {
		return nil, DestinationFailure(ErrContract)
	}
	if err := ctx.Err(); err != nil {
		return nil, DestinationFailure(err)
	}
	snapshot, err := authority.List(ctx)
	if err != nil {
		return nil, DestinationFailure(err)
	}
	canonical, err := NewSnapshot(snapshot.Operations, snapshot.RegistryUnknown)
	if err != nil {
		return nil, DestinationFailure(err)
	}
	return &Inventory{authority: authority, snapshot: canonical}, nil
}

func (inventory *Inventory) Snapshot() (Snapshot, error) {
	if inventory == nil || inventory.authority == nil || !inventory.snapshot.Valid() {
		return Snapshot{}, DestinationFailure(ErrContract)
	}
	return inventory.snapshot.Clone(), nil
}

func (inventory *Inventory) DiscardRestriction() *Failure {
	if inventory == nil || inventory.authority == nil || !inventory.snapshot.Valid() {
		return classifyFailure(ErrContract, true)
	}
	if inventory.snapshot.RegistryUnknown {
		return &Failure{Kind: FailureNeedsAttention, Reason: string(AttentionRegistry), Cause: ErrContract}
	}
	return nil
}

func (inventory *Inventory) CheckDiscard(id receivecontract.OperationID) *Failure {
	if failure := inventory.DiscardRestriction(); failure != nil {
		return failure
	}
	if id.IsZero() {
		return classifyFailure(ErrContract, true)
	}
	for _, operation := range inventory.snapshot.Operations {
		if operation.ID != id {
			continue
		}
		if operation.Running {
			return &Failure{Kind: FailureBusy, Reason: reasonOperationRunning}
		}
		return nil
	}
	return &Failure{Kind: FailureChanged, Reason: reasonOperationChanged, Cause: ErrContract}
}

func (inventory *Inventory) Discard(ctx context.Context, id receivecontract.OperationID) DiscardResult {
	if failure := inventory.CheckDiscard(id); failure != nil {
		return DiscardResult{Failure: failure, Err: failure}
	}
	if ctx == nil {
		failure := classifyFailure(ErrContract, true)
		return DiscardResult{Failure: failure, Err: failure}
	}
	if err := ctx.Err(); err != nil {
		failure := classifyFailure(err, true)
		return DiscardResult{Failure: failure, Err: err}
	}
	report, err := inventory.authority.Discard(ctx, id)
	if !report.Valid() || report.ID != id {
		cause := errors.Join(ErrContract, err)
		return DiscardResult{Failure: classifyFailure(cause, true), Err: cause, CleanupError: AuthorityCloseFailure(err)}
	}
	// Report and error are independent: cleanup may have finished even when the
	// destination lease failed to close. Preserve both facts for final settlement.
	report.BlockedItems = slices.Clone(report.BlockedItems)
	cleanupErr := AuthorityCloseFailure(err)
	if report.Status == Discarded {
		cleanupErr = err
	}
	return DiscardResult{Report: report, Err: err, CleanupError: cleanupErr}
}
