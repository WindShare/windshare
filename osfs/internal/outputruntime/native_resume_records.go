package outputruntime

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/osfs/internal/fileexecution"
	"github.com/windshare/windshare/osfs/internal/namestore"
	"github.com/windshare/windshare/osfs/internal/outputnames"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func ordinaryResumeItems(
	ctx context.Context,
	topLevel *destinationauthority.TopLevelReservation,
	store *checkpointstore.FileExecutionStore,
	names *outputnames.Mapper,
) ([]resumeauthority.Item, bool, error) {
	if ctx == nil || topLevel == nil || store == nil || names == nil {
		return nil, false, resumeauthority.ErrInvalidContract
	}
	slots, attention, err := store.Inventory()
	if err != nil {
		return nil, false, err
	}
	items := make([]resumeauthority.Item, 0, len(slots))
	for _, slot := range slots {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		item, err := ordinaryResumeLineageItem(ctx, topLevel, store, names, slot)
		if err != nil {
			return nil, false, err
		}
		items = append(items, item)
	}
	// Reconciliation attention has no authenticated lineage or safe path. Keep
	// it out of item inventory and let the operation lease close ownership.
	return items, checkpointstore.RecoveryBlocked(attention), nil
}

func ordinaryResumeLineageItem(
	ctx context.Context,
	topLevel *destinationauthority.TopLevelReservation,
	store *checkpointstore.FileExecutionStore,
	names *outputnames.Mapper,
	slot checkpointstore.FileExecutionLineageSlot,
) (resumeauthority.Item, error) {
	switch slot.Decision() {
	case checkpointmodel.CheckpointLineageDecisionExact:
		record, exact := slot.Record()
		if !exact {
			return resumeauthority.Item{}, resumeauthority.ErrInvalidContract
		}
		return ordinaryResumeRecordItem(ctx, topLevel, store, names, record)
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
	names *outputnames.Mapper,
	record checkpointmodel.Record,
) (resumeauthority.Item, error) {
	if record.Phase() == checkpointmodel.PhasePublished && record.CommitState() == checkpointmodel.CommitPublished {
		return ordinaryResumeState(resumeauthority.ItemPublished).item(record)
	}
	ownedFile, owned, ownedErr := store.OpenOwnedFile(
		ctx, record.OwnedObjectID(), record.ExactSize(), false,
	)
	ownedErr = errors.Join(ownedErr, closeNativeResumeOwnedFile(ownedFile))
	final, finalErr := observeOrdinaryResumeFinal(ctx, topLevel, store, names, record)
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

func (lease *NativeResumeLease) ensureNamesLocked() error {
	if lease.names != nil {
		return nil
	}
	if err := lease.ensureTopLevelLocked(); err != nil {
		return err
	}
	intent, err := lease.operation.Record().VerifyIntent(transfer.DecodeReceiveIntent)
	if err != nil {
		return err
	}
	mapper, store, err := newOperationNames(intent, lease.topLevel, lease.operation, false, nil)
	if err != nil {
		return err
	}
	lease.names, lease.nameStore = mapper, store
	return nil
}

func (lease *NativeResumeLease) cleanupNamesLocked() error {
	// Empty name state requires no public capability. This preserves cleanup
	// after a result root was removed, and the normal path adds no public I/O.
	intent, err := lease.operation.Record().VerifyIntent(transfer.DecodeReceiveIntent)
	if err != nil {
		return err
	}
	reservation, ok := intent.MaterializationPlan().DestinationReservation()
	if !ok {
		return transfer.ErrInvalidOutputBinding
	}
	store := namestore.New(lease.operation, intent.OperationID().Bytes(), false)
	records, err := store.Load()
	if err != nil {
		return err
	}
	if len(records) == 0 && reservation.LogicalReservedName() == reservation.PhysicalName() {
		return nil
	}
	root := outputnames.Root{RequestedName: reservation.RequestedName(), ReservedName: reservation.LogicalReservedName(), PhysicalName: reservation.PhysicalName(), Kind: reservation.EntryKind()}
	resolver, err := outputnames.NewResolver(intent.OperationID().Bytes(), root, records)
	if err != nil {
		return err
	}
	if err := store.Export(resolver, root, lease.runtime.destination.WithPublicContainer); err != nil {
		return err
	}
	return store.Clear()
}
