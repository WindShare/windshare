package fileexecution

import (
	"bytes"
	"errors"
	"math"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
)

func (engine *Engine) checkpointKey(file transfer.MaterializationFile) (CheckpointKey, error) {
	if engine == nil {
		return CheckpointKey{}, ErrInvalidClaim
	}
	descriptor := file.Descriptor()
	target := file.Target()
	materializationPath := file.MaterializationRelativePath().String()
	canonical, err := catalog.CanonicalPath(materializationPath)
	if err != nil || !transfer.MaterializationFileMatchesIntent(engine.intent, file) ||
		!file.SourcePath().Valid() || !file.ArtifactPath().Valid() ||
		!file.MaterializationRelativePath().Valid() ||
		canonical != materializationPath || materializationPath == "" ||
		descriptor.ShareInstance() != engine.intent.ShareInstance() ||
		descriptor.FileID().IsZero() || descriptor.FileRevision().IsZero() ||
		file.ExpectedSize() != descriptor.ExactSize() || target.OutputSessionID() != engine.sessionID ||
		target.Descriptor() != descriptor || target.ExactSize() != file.ExpectedSize() ||
		target.Locator().Kind() != transfer.MaterializationPathLocator ||
		target.Locator().CanonicalPath() != materializationPath {
		return CheckpointKey{}, errors.Join(ErrInvalidClaim, err)
	}
	ownership := engine.binding.Ownership()
	key := CheckpointKey{
		operation: engine.binding.OperationID(), intent: engine.binding.ReceiveIntentDigest(),
		materialization: engine.binding.MaterializationBindingDigest(),
		fileID:          descriptor.FileID(), revision: descriptor.FileRevision(), path: materializationPath,
		exactSize: file.ExpectedSize(), materializer: ownership.MaterializerKind(),
		authority: ownership.AuthorityRef(),
	}
	if !key.valid() {
		return CheckpointKey{}, ErrInvalidClaim
	}
	return key, nil
}

func newInitialRecord(key CheckpointKey, object checkpointmodel.ObjectID) (checkpointmodel.Record, error) {
	if !key.valid() || object.IsZero() {
		return checkpointmodel.Record{}, ErrInvalidClaim
	}
	return checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		OperationID: key.operation, ReceiveIntentDigest: key.intent,
		MaterializationBindingDigest: key.materialization,
		FileID:                       key.fileID, FileRevision: key.revision, CanonicalPath: key.path,
		ExactSize: key.exactSize, MaterializerKind: key.materializer,
		AuthorityRef: key.authority.Bytes(), OwnedObjectID: object.Bytes(),
		StateGeneration: 1, CheckpointGeneration: 0,
		Phase: checkpointmodel.PhaseActive, CommitState: checkpointmodel.CommitCandidate,
	})
}

func nextStateGeneration(record checkpointmodel.Record) (uint64, error) {
	if !record.Valid() || record.StateGeneration() == math.MaxUint64 {
		return 0, checkpointmodel.ErrRecordGeneration
	}
	return record.StateGeneration() + 1, nil
}

func transitionRecord(
	record checkpointmodel.Record,
	phase checkpointmodel.Phase,
	commit checkpointmodel.CommitState,
	quarantineReason checkpointmodel.QuarantineReason,
	quarantineOrigin checkpointmodel.QuarantineOrigin,
	retirementReason checkpointmodel.RetirementReason,
) (checkpointmodel.Record, error) {
	generation, err := nextStateGeneration(record)
	if err != nil {
		return checkpointmodel.Record{}, err
	}
	return checkpointmodel.AdvanceState(
		record, generation, phase, commit,
		quarantineReason, quarantineOrigin, retirementReason,
	)
}

func activateRecord(record checkpointmodel.Record) (checkpointmodel.Record, error) {
	if record.Phase() == checkpointmodel.PhaseActive && record.CommitState() == checkpointmodel.CommitVerified {
		return record, nil
	}
	return transitionRecord(record, checkpointmodel.PhaseActive, checkpointmodel.CommitVerified, 0, 0, 0)
}

func pauseRecord(record checkpointmodel.Record) (checkpointmodel.Record, error) {
	return transitionRecord(record, checkpointmodel.PhasePaused, checkpointmodel.CommitVerified, 0, 0, 0)
}

func publishingRecord(record checkpointmodel.Record) (checkpointmodel.Record, error) {
	return transitionRecord(record, checkpointmodel.PhasePublishing, checkpointmodel.CommitVerified, 0, 0, 0)
}

func publishedRecord(record checkpointmodel.Record) (checkpointmodel.Record, error) {
	return transitionRecord(record, checkpointmodel.PhasePublished, checkpointmodel.CommitPublished, 0, 0, 0)
}

func retiredRecord(record checkpointmodel.Record, reason checkpointmodel.RetirementReason) (checkpointmodel.Record, error) {
	return transitionRecord(record, checkpointmodel.PhaseRetired, checkpointmodel.CommitVerified, 0, 0, reason)
}

func quarantineRecord(record checkpointmodel.Record, reason checkpointmodel.QuarantineReason) (checkpointmodel.Record, error) {
	origin := quarantineOrigin(record.Phase())
	if origin == 0 || !reason.Valid() {
		return checkpointmodel.Record{}, checkpointmodel.ErrRecordGeneration
	}
	return transitionRecord(
		record, checkpointmodel.PhaseQuarantined, checkpointmodel.CommitQuarantined,
		reason, origin, record.RetirementReason(),
	)
}

func quarantineOrigin(phase checkpointmodel.Phase) checkpointmodel.QuarantineOrigin {
	switch phase {
	case checkpointmodel.PhaseReserved:
		return checkpointmodel.QuarantineOriginReserved
	case checkpointmodel.PhaseActive, checkpointmodel.PhasePaused:
		return checkpointmodel.QuarantineOriginWitnessed
	case checkpointmodel.PhasePublishing:
		return checkpointmodel.QuarantineOriginPublishing
	case checkpointmodel.PhasePublished:
		return checkpointmodel.QuarantineOriginPublished
	case checkpointmodel.PhaseRetired:
		return checkpointmodel.QuarantineOriginRetiring
	default:
		return 0
	}
}

func recordEqual(left, right checkpointmodel.Record) bool {
	return left.Valid() && right.Valid() && bytes.Equal(left.CanonicalBytes(), right.CanonicalBytes())
}

func contentRanges(record checkpointmodel.Record) (content.RangeSet, error) {
	ranges := record.VerifiedRanges()
	converted := make([]content.Range, len(ranges))
	for index, current := range ranges {
		converted[index] = content.Range{Offset: current.Offset, End: current.End}
	}
	return content.NewRangeSet(converted)
}

func checkpointRanges(ranges content.RangeSet) []checkpointmodel.Range {
	items := ranges.Ranges()
	converted := make([]checkpointmodel.Range, len(items))
	for index, current := range items {
		converted[index] = checkpointmodel.Range{Offset: current.Offset, End: current.End}
	}
	return converted
}

func outputIdentity(object checkpointmodel.ObjectID) (transfer.OwnedObjectID, error) {
	return transfer.OwnedObjectIDFromBytes(object.Bytes())
}

func outputBinding(
	target transfer.FileMaterializationTarget,
	record checkpointmodel.Record,
) (transfer.MaterializedFileBinding, error) {
	identity, err := outputIdentity(record.OwnedObjectID())
	if err != nil {
		return transfer.MaterializedFileBinding{}, err
	}
	return transfer.BindFileMaterializationTarget(target, identity)
}

func durableRanges(
	binding transfer.MaterializedFileBinding,
	record checkpointmodel.Record,
) (transfer.VerifiedDurableRanges, error) {
	ranges, err := contentRanges(record)
	if err != nil {
		return transfer.VerifiedDurableRanges{}, err
	}
	return transfer.VerifyDurableRanges(
		binding, transfer.CheckpointGeneration(record.CheckpointGeneration()), ranges,
	)
}

func recordComplete(record checkpointmodel.Record) (bool, error) {
	ranges, err := contentRanges(record)
	if err != nil {
		return false, err
	}
	return transfer.RangesCoverFile(record.ExactSize(), ranges), nil
}
