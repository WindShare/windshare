package outputruntime

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
)

func ordinaryResumeItems(
	ctx context.Context,
	topLevel *destinationauthority.TopLevelReservation,
	store *checkpointstore.FileExecutionStore,
) ([]resumeauthority.Item, bool, error) {
	if ctx == nil || topLevel == nil || store == nil {
		return nil, false, resumeauthority.ErrInvalidContract
	}
	slots, attention := store.LineageSnapshot()
	items := make([]resumeauthority.Item, 0, len(slots))
	for _, slot := range slots {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		item, err := ordinaryResumeLineageItem(ctx, topLevel, store, slot)
		if err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	// Reconciliation attention has no authenticated lineage or safe path. Keep
	// it out of item inventory and let the operation lease close ownership.
	return items, len(attention) != 0, nil
}

func ordinaryResumeLineageItem(
	ctx context.Context,
	topLevel *destinationauthority.TopLevelReservation,
	store *checkpointstore.FileExecutionStore,
	slot checkpointstore.FileExecutionLineageSlot,
) (resumeauthority.Item, error) {
	switch slot.Decision() {
	case checkpointmodel.CheckpointLineageDecisionExact:
		record, exact := slot.Record()
		if !exact {
			return resumeauthority.Item{}, resumeauthority.ErrInvalidContract
		}
		return ordinaryResumeRecordItem(ctx, topLevel, store, record)
	case checkpointmodel.CheckpointLineageDecisionRevisionConflict:
		return resumeauthority.NewItem(
			slot.CanonicalPath(), resumeauthority.ItemBlocked,
			resumeauthority.ItemBlockRevisionConflict,
		)
	case checkpointmodel.CheckpointLineageDecisionOwnershipConflict:
		return resumeauthority.NewItem(
			slot.CanonicalPath(), resumeauthority.ItemBlocked,
			resumeauthority.ItemBlockOwnedObjectUnknown,
		)
	case checkpointmodel.CheckpointLineageDecisionInvalid:
		return resumeauthority.NewItem(
			slot.CanonicalPath(), resumeauthority.ItemBlocked,
			resumeauthority.ItemBlockCheckpointInvalid,
		)
	default:
		// Startup slots always contain authenticated physical evidence. Absent is
		// meaningful for a live claim, but cannot grant an inventory item.
		return resumeauthority.Item{}, resumeauthority.ErrInvalidContract
	}
}

func ordinaryResumeRecordItem(
	ctx context.Context,
	topLevel *destinationauthority.TopLevelReservation,
	store *checkpointstore.FileExecutionStore,
	record checkpointmodel.Record,
) (resumeauthority.Item, error) {
	ownedFile, owned, ownedErr := store.OpenOwnedFile(
		ctx, record.OwnedObjectID(), record.ExactSize(), false,
	)
	ownedErr = errors.Join(ownedErr, closeNativeResumeOwnedFile(ownedFile))
	final, finalErr := observeOrdinaryResumeFinal(ctx, topLevel, store, record)
	if err := ctx.Err(); err != nil {
		return resumeauthority.Item{}, err
	}
	if ownedErr != nil {
		return resumeauthority.NewItem(
			record.CanonicalPath(), resumeauthority.ItemBlocked,
			resumeauthority.ItemBlockOwnedObjectUnknown,
		)
	}
	if finalErr != nil {
		return resumeauthority.NewItem(
			record.CanonicalPath(), resumeauthority.ItemBlocked,
			resumeauthority.ItemBlockPublicationUnknown,
		)
	}
	return reduceOrdinaryResumeRecord(record, owned, final).item(record)
}

func blockedPublicationItem(record checkpointmodel.Record) (resumeauthority.Item, error) {
	return ordinaryResumeBlock(resumeauthority.ItemBlockPublicationUnknown).item(record)
}

func blockedOwnedItem(
	record checkpointmodel.Record,
	_ fileexecution.OwnedObservation,
) (resumeauthority.Item, error) {
	return ordinaryResumeBlock(resumeauthority.ItemBlockOwnedObjectUnknown).item(record)
}

func closeNativeResumeOwnedFile(file fileexecution.OwnedFile) error {
	if file == nil {
		return nil
	}
	return file.Close()
}
