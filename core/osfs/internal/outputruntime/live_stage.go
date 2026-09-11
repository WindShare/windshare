package outputruntime

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/transfer"
)

const livePartialObjectDomain = "windshare/live-partial-object/v1\x00"

func (executor *liveFileExecutor) createOwnedStage(
	ctx context.Context,
	destination fileexecution.FileDestination,
	exactSize uint64,
) (*fileexecution.LiveOwnedFile, func(*fileexecution.LiveOwnedFile) error, error) {
	parent, ok := destination.(destinationauthority.LiveCleanupStageParent)
	if !ok {
		return nil, nil, transfer.ErrInvalidOutputBinding
	}
	var nonce [checkpointmodel.LiveCleanupNonceBytesV1]byte
	if _, err := io.ReadFull(executor.random, nonce[:]); err != nil {
		return nil, nil, err
	}
	digest := sha256.Sum256(append([]byte(livePartialObjectDomain), nonce[:]...))
	object, err := checkpointmodel.ObjectIDFromBytes(digest[:])
	if err != nil {
		return nil, nil, err
	}
	if !executor.authority.Binding().Capabilities().CrashCleanup().Supported() {
		stage, err := executor.authority.CreateProcessStage(ctx, parent, exactSize, executor.random)
		if err != nil {
			return nil, nil, err
		}
		owned, err := fileexecution.NewProcessOwnedFile(object, stage.File(), stage.Close)
		if err != nil {
			return nil, nil, errors.Join(err, stage.Remove(), stage.Close())
		}
		return owned, func(*fileexecution.LiveOwnedFile) error { return stage.Remove() }, nil
	}
	ticket, err := checkpointmodel.NewLiveCleanupTicket(checkpointmodel.LiveCleanupTicketSpec{
		Nonce: nonce[:], ExactSize: exactSize,
		Profile: executor.authority.LiveCleanupProfile(), Generation: 1,
		State: checkpointmodel.LiveCleanupTicketCommitted,
	})
	if err != nil {
		return nil, nil, err
	}
	stage, created, err := executor.authority.CreateLiveCleanupStage(ctx, parent, ticket)
	if err != nil {
		return nil, nil, err
	}
	owned, err := fileexecution.NewLiveOwnedFile(object, stage, created)
	if err != nil {
		return nil, nil, errors.Join(err, stage.Close())
	}
	return owned, func(current *fileexecution.LiveOwnedFile) error {
		return executor.authority.RemoveLiveCleanupStage(current.CleanupTicket(), current.NativeFile())
	}, nil
}
