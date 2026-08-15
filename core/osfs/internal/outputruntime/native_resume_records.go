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
) ([]resumeauthority.Item, error) {
	if ctx == nil || topLevel == nil || store == nil {
		return nil, resumeauthority.ErrInvalidContract
	}
	records, attention := store.Snapshot()
	items := make([]resumeauthority.Item, 0, len(records)+len(attention))
	for _, current := range attention {
		item, err := resumeauthority.NewBlockedReference(current.Reference())
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item, err := ordinaryResumeRecordItem(ctx, topLevel, store, record)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
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
	if record.Phase() == checkpointmodel.PhaseQuarantined ||
		record.CommitState() == checkpointmodel.CommitQuarantined {
		return resumeauthority.NewItem(
			record.CanonicalPath(), resumeauthority.ItemBlocked,
			resumeauthority.ItemBlockCheckpointInvalid,
		)
	}

	switch record.Phase() {
	case checkpointmodel.PhasePublished:
		if final.Condition() == fileexecution.FinalOwnedExact {
			return ordinaryResumeItem(record, resumeauthority.ItemPublished)
		}
		return blockedPublicationItem(record)
	case checkpointmodel.PhasePublishing:
		switch final.Condition() {
		case fileexecution.FinalOwnedExact:
			return ordinaryResumeItem(record, resumeauthority.ItemPublished)
		case fileexecution.FinalAbsent, fileexecution.FinalCollision:
			// A definite collision changes only when publication may proceed. It
			// does not make an intact owned object ambiguous or permanently block
			// the operation after the foreign final is removed.
			if owned.Condition() == fileexecution.OwnedReady {
				return ordinaryResumeItem(record, resumeauthority.ItemResumable)
			}
			return blockedOwnedItem(record, owned)
		default:
			return blockedPublicationItem(record)
		}
	case checkpointmodel.PhaseReserved, checkpointmodel.PhaseActive, checkpointmodel.PhasePaused:
		switch final.Condition() {
		case fileexecution.FinalAbsent, fileexecution.FinalCollision:
			// Inventory reports durable restart capability, not the current
			// destination occupancy. A known foreign final is surfaced as a typed
			// collision by file execution and remains safe to retry in-place.
			switch owned.Condition() {
			case fileexecution.OwnedReady:
				if len(record.VerifiedRanges()) != 0 ||
					record.Phase() == checkpointmodel.PhasePaused {
					return ordinaryResumeItem(record, resumeauthority.ItemResumable)
				}
				return ordinaryResumeItem(record, resumeauthority.ItemIncomplete)
			case fileexecution.OwnedAbsent, fileexecution.OwnedAnchorMissing,
				fileexecution.OwnedStageMissing:
				// A normal matched receive starts a fresh object while preserving
				// this lost checkpoint evidence.
				return ordinaryResumeItem(record, resumeauthority.ItemIncomplete)
			default:
				return blockedOwnedItem(record, owned)
			}
		default:
			return blockedPublicationItem(record)
		}
	case checkpointmodel.PhaseRetired:
		if record.RetirementReason() == checkpointmodel.RetirementPublished &&
			final.Condition() == fileexecution.FinalOwnedExact {
			return ordinaryResumeItem(record, resumeauthority.ItemPublished)
		}
		return ordinaryResumeItem(record, resumeauthority.ItemFailed)
	default:
		return resumeauthority.NewItem(
			record.CanonicalPath(), resumeauthority.ItemBlocked,
			resumeauthority.ItemBlockCheckpointInvalid,
		)
	}
}

func ordinaryResumeItem(
	record checkpointmodel.Record,
	state resumeauthority.ItemState,
) (resumeauthority.Item, error) {
	return resumeauthority.NewItem(
		record.CanonicalPath(), state, resumeauthority.ItemBlockNone,
	)
}

func blockedPublicationItem(record checkpointmodel.Record) (resumeauthority.Item, error) {
	return resumeauthority.NewItem(
		record.CanonicalPath(), resumeauthority.ItemBlocked,
		resumeauthority.ItemBlockPublicationUnknown,
	)
}

func blockedOwnedItem(
	record checkpointmodel.Record,
	_ fileexecution.OwnedObservation,
) (resumeauthority.Item, error) {
	return resumeauthority.NewItem(
		record.CanonicalPath(), resumeauthority.ItemBlocked,
		resumeauthority.ItemBlockOwnedObjectUnknown,
	)
}

func closeNativeResumeOwnedFile(file fileexecution.OwnedFile) error {
	if file == nil {
		return nil
	}
	return file.Close()
}
