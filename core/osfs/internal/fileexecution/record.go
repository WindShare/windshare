package fileexecution

import (
	"bytes"
	"context"
	"errors"
	"math"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

func (engine *Engine) checkpointKey(claim outputsession.FileClaim) (CheckpointKey, error) {
	if engine == nil || claim.ID() == 0 || claim.ParentID() == 0 || claim.LocatorKey() == "" {
		return CheckpointKey{}, ErrInvalidClaim
	}
	file := claim.File()
	descriptor := file.Descriptor
	target := file.Target
	ownership := engine.binding.Ownership()
	canonical, err := engine.canonicalFilePath(file.Path)
	if err != nil || canonical != file.Path || file.Path == "" ||
		descriptor.ShareInstance() != engine.intent.ShareInstance() ||
		descriptor.FileID().IsZero() || descriptor.FileRevision().IsZero() ||
		file.ExpectedSize != descriptor.ExactSize() ||
		target.OutputSessionID() != engine.sessionID || target.BackendID() != ownership.Backend() ||
		target.Descriptor() != descriptor || target.ExactSize() != file.ExpectedSize ||
		target.Locator().Kind() != transfer.OutputPathLocator ||
		target.Locator().CanonicalPath() != file.Path || file.ParentAdmission.IsZero() {
		return CheckpointKey{}, errors.Join(ErrInvalidClaim, err)
	}
	key := CheckpointKey{
		intent: engine.binding.TransferIntentDigest(), fileID: descriptor.FileID(),
		revision: descriptor.FileRevision(), path: file.Path, exactSize: file.ExpectedSize,
		backend: ownership.Backend(), root: ownership.RootIdentity(),
	}
	if !key.valid() {
		return CheckpointKey{}, ErrInvalidClaim
	}
	return key, nil
}

func (engine *Engine) canonicalFilePath(path string) (string, error) {
	return engine.pathCanonicalizer(path)
}

func newInitialRecord(
	key CheckpointKey,
	object checkpointmodel.ObjectID,
) (checkpointmodel.Record, error) {
	if !key.valid() || object.IsZero() {
		return checkpointmodel.Record{}, ErrInvalidClaim
	}
	return checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		TransferIntentDigest: key.intent,
		FileID:               key.fileID,
		FileRevision:         key.revision,
		CanonicalPath:        key.path,
		ExactSize:            key.exactSize,
		BackendID:            string(key.backend),
		RootIdentity:         key.root.Bytes(),
		OwnedOutputObject:    object.Bytes(),
		StateGeneration:      1,
		Phase:                checkpointmodel.PhaseActive,
		CommitState:          checkpointmodel.CommitCandidate,
	})
}

func newInitialQuarantine(
	key CheckpointKey,
	object checkpointmodel.ObjectID,
) (checkpointmodel.Record, error) {
	if !key.valid() || object.IsZero() {
		return checkpointmodel.Record{}, ErrInvalidClaim
	}
	return checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		TransferIntentDigest: key.intent,
		FileID:               key.fileID,
		FileRevision:         key.revision,
		CanonicalPath:        key.path,
		ExactSize:            key.exactSize,
		BackendID:            string(key.backend),
		RootIdentity:         key.root.Bytes(),
		OwnedOutputObject:    object.Bytes(),
		StateGeneration:      1,
		Phase:                checkpointmodel.PhaseQuarantined,
		CommitState:          checkpointmodel.CommitQuarantined,
		QuarantineReason:     checkpointmodel.QuarantinePartialObjectCreation,
		QuarantineOrigin:     checkpointmodel.QuarantineOriginReserved,
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
	return transitionRecord(
		record, checkpointmodel.PhaseActive, checkpointmodel.CommitVerified, 0, 0, 0,
	)
}

func pauseRecord(record checkpointmodel.Record) (checkpointmodel.Record, error) {
	return transitionRecord(
		record, checkpointmodel.PhasePaused, checkpointmodel.CommitVerified, 0, 0, 0,
	)
}

func publishingRecord(record checkpointmodel.Record) (checkpointmodel.Record, error) {
	return transitionRecord(
		record, checkpointmodel.PhasePublishing, checkpointmodel.CommitVerified, 0, 0, 0,
	)
}

func publishedRecord(record checkpointmodel.Record) (checkpointmodel.Record, error) {
	return transitionRecord(
		record, checkpointmodel.PhasePublished, checkpointmodel.CommitPublished, 0, 0, 0,
	)
}

func retiredRecord(
	record checkpointmodel.Record,
	reason checkpointmodel.RetirementReason,
) (checkpointmodel.Record, error) {
	return transitionRecord(
		record, checkpointmodel.PhaseRetired, checkpointmodel.CommitVerified, 0, 0, reason,
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

func quarantineRecord(
	record checkpointmodel.Record,
	reason checkpointmodel.QuarantineReason,
) (checkpointmodel.Record, error) {
	origin := quarantineOrigin(record.Phase())
	if origin == 0 || !reason.Valid() {
		return checkpointmodel.Record{}, checkpointmodel.ErrRecordGeneration
	}
	return transitionRecord(
		record, checkpointmodel.PhaseQuarantined, checkpointmodel.CommitQuarantined,
		reason, origin, record.RetirementReason(),
	)
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

func outputIdentity(object checkpointmodel.ObjectID) (transfer.OutputObjectIdentity, error) {
	return transfer.OutputObjectIdentityFromBytes(object.Bytes())
}

func outputBinding(
	target transfer.OutputFileTarget,
	record checkpointmodel.Record,
) (transfer.OutputFileBinding, error) {
	identity, err := outputIdentity(record.OwnedOutputObject())
	if err != nil {
		return transfer.OutputFileBinding{}, err
	}
	return transfer.BindOutputFileTarget(target, identity)
}

func durableRanges(
	binding transfer.OutputFileBinding,
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

func (engine *Engine) lookupRecord(
	ctx context.Context,
	key CheckpointKey,
) (checkpointmodel.Record, bool, error) {
	record, found, err := engine.checkpoints.Lookup(ctx, key)
	if err != nil {
		return checkpointmodel.Record{}, false, collaboratorError(ctx, err)
	}
	if !found {
		if record.Valid() {
			return checkpointmodel.Record{}, false, checkpointBindingError(ErrPortContract)
		}
		return checkpointmodel.Record{}, false, nil
	}
	if !key.matches(record) || !engine.binding.Matches(record, record.RecordID()) {
		return checkpointmodel.Record{}, false, checkpointBindingError(ErrCheckpointBinding)
	}
	return record, true, nil
}

// storeRecord is the single checkpoint mutation reducer. The repository applies
// one exact decision and reports a fresh target observation; this function alone
// decides whether the cut is installed, unchanged, or ambiguous.
func (engine *Engine) storeRecord(
	ctx context.Context,
	previous *checkpointmodel.Record,
	next checkpointmodel.Record,
) (outputsession.MutationCut, bool, error) {
	if ctx == nil {
		return outputsession.MutationNoChange, false, fileContractError(ErrInvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return outputsession.MutationNoChange, false, err
	}
	if !next.Valid() || !engine.binding.Matches(next, next.RecordID()) ||
		previous != nil && (!previous.Valid() || previous.RecordID() != next.RecordID()) {
		return outputsession.MutationNoChange, false, checkpointBindingError(ErrCheckpointBinding)
	}
	observation, operationErr := engine.checkpoints.Store(ctx, previous, next)
	normalizedErr := collaboratorError(ctx, operationErr)
	if !observation.valid() {
		return outputsession.MutationAmbiguous, false, joinFailures(ctx,
			checkpointInstallError(ErrInvalidObservation), normalizedErr,
		)
	}
	if current, present := observation.Record(); present && recordEqual(current, next) {
		return outputsession.MutationStable, operationErr != nil, nil
	}
	if previous == nil {
		// Exact create cannot have installed next when a fresh observation is
		// either absent or another record. The new private object is therefore
		// still unclaimed and may be retired instead of leaked as ambiguous.
		if operationErr != nil {
			return outputsession.MutationNoChange, false, normalizedErr
		}
		return outputsession.MutationNoChange, false, checkpointInstallError(
			errors.Join(ErrCheckpointNotInstalled, ErrPortContract),
		)
	}
	unchanged := false
	if current, present := observation.Record(); present {
		unchanged = recordEqual(current, *previous)
	}
	if unchanged {
		if operationErr != nil {
			return outputsession.MutationNoChange, false, normalizedErr
		}
		return outputsession.MutationNoChange, false, checkpointInstallError(
			errors.Join(ErrCheckpointNotInstalled, ErrPortContract),
		)
	}
	cause := errors.Join(ErrCheckpointNotInstalled, normalizedErr)
	if operationErr == nil {
		cause = errors.Join(cause, ErrPortContract)
	}
	return outputsession.MutationAmbiguous, false, checkpointInstallError(cause)
}
