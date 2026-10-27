package outputruntime

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func (operation *Operation) RetryAvailable() bool {
	if operation == nil || operation.authority == nil {
		return false
	}
	authority := operation.authority
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return !authority.closed && authority.operation == operation && !operation.closed && !operation.opened
}

func (operation *Operation) Discard(ctx context.Context) error {
	if ctx == nil || !operation.RetryAvailable() {
		return transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	authority := operation.authority
	id, mode := operation.intent.OperationID(), operation.mode
	// End the live lease before the shared recovery workflow acquires the exact
	// operation. It revalidates persisted identity before deleting private state.
	if err := authority.Close(); err != nil {
		return err
	}
	if mode == outputcap.ExecutionLiveOnly {
		return nil
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
	if err == nil && result.State() != resumeauthority.OperationDiscarded {
		return errors.New("owned output cleanup remains pending")
	}
	return err
}
