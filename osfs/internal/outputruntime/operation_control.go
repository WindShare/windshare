package outputruntime

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

var ErrOutputShareStopped = errors.New("retained output belongs to a stopped share")

func (operation *Operation) Idle() bool {
	if operation == nil || operation.authority == nil {
		return false
	}
	authority := operation.authority
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return operation.idleLocked()
}

func (operation *Operation) idleLocked() bool {
	return !operation.authority.closed && operation.authority.operation == operation && !operation.closed && !operation.opened
}

func (operation *Operation) RecordShareStopped(ctx context.Context) error {
	if operation == nil || operation.authority == nil || ctx == nil {
		return transfer.ErrInvalidOutputBinding
	}
	operation.authority.mu.Lock()
	defer operation.authority.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !operation.idleLocked() || operation.mode != outputcap.ExecutionResumable || operation.lease == nil {
		return transfer.ErrInvalidOutputBinding
	}
	if operation.lease.Record().Lifecycle() == checkpointmodel.OrdinaryOperationShareStopped {
		return nil
	}
	return transitionOutputOperation(operation.lease, checkpointmodel.OrdinaryLifecycleStopShare, checkpointmodel.OrdinaryReasonNone)
}

func (operation *Operation) Discard(ctx context.Context) error {
	if ctx == nil || !operation.Idle() {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	authority := operation.authority
	id, mode := operation.intent.OperationID(), operation.mode
	operation.setNameReport(outputcap.CompatibleNameReport{})
	if mode == outputcap.ExecutionLiveOnly {
		if operation.names != nil && operation.nameStore != nil {
			summary, err := operation.nameStore.Export(operation.names, namesRoot(operation.topLevel), authority.destination.WithPublicContainer)
			if err != nil {
				return err
			}
			operation.setNameReport(summary)
		}
		return authority.Close()
	}
	// End the live lease before the shared recovery workflow acquires the exact
	// operation. It revalidates persisted identity before deleting private state.
	if err := authority.Close(); err != nil {
		return err
	}
	repository, err := NewNativeResumeRepository(authority.rootPath, authority.platformFactory, authority.tracer)
	if err != nil {
		return err
	}
	recovery, err := resumeauthority.New(repository)
	if err != nil {
		return err
	}
	result, err := recovery.Discard(ctx, id)
	if report := result.CompatibleNameReport(); !report.Empty() {
		operation.setNameReport(report)
	}
	if err == nil && result.State() != resumeauthority.OperationDiscarded {
		return errors.New("owned output cleanup remains pending")
	}
	return err
}
