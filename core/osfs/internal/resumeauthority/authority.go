package resumeauthority

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const operationInventoryPageSize = 128

type Store interface {
	Page(context.Context, PageCursor, int) (Page, error)
	Acquire(context.Context, receivecontract.OperationID) (OperationLease, error)
}

type OperationLease interface {
	Snapshot(context.Context) (Snapshot, error)
	Transition(
		context.Context,
		checkpointmodel.OrdinaryLifecycleEvent,
		checkpointmodel.OrdinaryClosedReason,
	) (Header, error)
	Cleanup(context.Context) (CleanupState, error)
	Close() error
}

type Authority struct {
	store Store
}

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
	var (
		cursor    PageCursor
		summaries []Summary
		unknown   bool
	)
	for {
		page, err := authority.store.Page(ctx, cursor, operationInventoryPageSize)
		if err != nil {
			return Inventory{}, err
		}
		unknown = unknown || page.Unknown()
		for _, header := range page.Headers() {
			summary, currentUnknown, err := authority.listOperation(ctx, header)
			if err != nil {
				return Inventory{}, err
			}
			unknown = unknown || currentUnknown
			if summary.Valid() {
				summaries = append(summaries, summary)
			}
		}
		next := page.Next()
		if next.IsZero() {
			break
		}
		if next.After() == cursor.After() {
			return Inventory{}, ErrInvalidContract
		}
		cursor = next
	}
	return newInventory(summaries, unknown), nil
}

func (authority *Authority) listOperation(
	ctx context.Context,
	header Header,
) (summary Summary, unknown bool, resultErr error) {
	lease, err := authority.store.Acquire(ctx, header.Record().OperationID())
	if errors.Is(err, ErrBusy) {
		summary, reduceErr := ReduceHeader(header, true)
		return summary, false, reduceErr
	}
	if err != nil {
		return Summary{}, false, err
	}
	if lease == nil {
		return Summary{}, false, ErrInvalidContract
	}
	defer func() { resultErr = errors.Join(resultErr, lease.Close()) }()
	snapshot, err := lease.Snapshot(ctx)
	if err != nil {
		return Summary{}, false, err
	}
	if !snapshot.Valid() ||
		snapshot.Header().Record().OperationID() != header.Record().OperationID() {
		return Summary{}, false, ErrInvalidContract
	}
	summary, err = ReduceRecovery(snapshot)
	return summary, false, err
}

func (authority *Authority) Discard(
	ctx context.Context,
	operation receivecontract.OperationID,
) (result Summary, resultErr error) {
	if authority == nil || authority.store == nil || ctx == nil || operation.IsZero() {
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
	if err != nil || !snapshot.Valid() ||
		snapshot.Header().Record().OperationID() != operation {
		return Summary{}, errors.Join(ErrInvalidContract, err)
	}
	header := snapshot.Header()
	action, err := ReduceDiscard(header.Record())
	if err != nil {
		return Summary{}, err
	}
	if action == DiscardTransitionAndCleanup {
		header, err = lease.Transition(
			ctx,
			checkpointmodel.OrdinaryLifecycleDiscard,
			checkpointmodel.OrdinaryReasonNone,
		)
		if err != nil {
			return Summary{}, err
		}
	}
	cleanup, cleanupErr := lease.Cleanup(ctx)
	if !cleanup.Valid() {
		return Summary{}, errors.Join(ErrInvalidContract, cleanupErr)
	}
	if cleanup == CleanupComplete {
		return newSummary(header, OperationDiscarded, snapshot.Items(), false)
	}

	record := header.Record()
	if record.Lifecycle() != checkpointmodel.OrdinaryOperationCleanupPending {
		header, err = lease.Transition(
			ctx,
			checkpointmodel.OrdinaryLifecycleCleanupFailed,
			checkpointmodel.OrdinaryReasonCleanupUncertain,
		)
		if err != nil {
			return Summary{}, errors.Join(cleanupErr, err)
		}
	}
	pending, err := NewSnapshot(header, snapshot.Items())
	if err != nil {
		return Summary{}, errors.Join(cleanupErr, err)
	}
	result, err = ReduceRecovery(pending)
	return result, errors.Join(cleanupErr, err)
}
