package resumeauthority

import (
	"bytes"
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func (repository *resumeLeasedRepository) Apply(
	ctx context.Context,
	action Action,
) (ApplyResult, error) {
	if err := contextErr(ctx); err != nil {
		return ApplyResult{}, err
	}
	if repository == nil {
		return ApplyResult{},
			projectResumeError("apply resume action", transfer.ErrInvalidOutputBinding)
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.closed || !repository.observed || !action.Valid() {
		return ApplyResult{},
			projectResumeError("apply resume action", ErrInvalidContract)
	}
	if len(repository.applyAttention) > 0 {
		return NewApplyResult(
			ApplyNeedsAttention, repository.applyAttention,
		)
	}
	if repository.snapshot.NamespaceEvidence() != EvidenceExact {
		return repository.namespaceNeedsAttention(
			repository.snapshot.NamespaceEvidence(), action.RecordID(),
		)
	}
	if attention := repository.snapshot.Attention(); len(attention) > 0 {
		return NewApplyResult(ApplyNeedsAttention, attention)
	}
	if repository.nextAction >= len(repository.expected) {
		return ApplyResult{},
			projectResumeError("apply exhausted resume plan", ErrInvalidContract)
	}
	expected := repository.expected[repository.nextAction]
	if expected.kind != action.Kind() || expected.recordID != action.RecordID() {
		return ApplyResult{},
			projectResumeError("apply out-of-order resume action", ErrInvalidContract)
	}
	checkpoint := repository.checkpointPins[action.RecordID()]
	if checkpoint == nil {
		return ApplyResult{},
			projectResumeError("bind resume action", checkpointmodel.ErrRecordBinding)
	}
	encoded, err := checkpointmodel.EncodeRecord(action.Record())
	if err != nil || !bytes.Equal(encoded, checkpoint.encoded) {
		return ApplyResult{}, projectResumeError(
			"bind resume action image", errors.Join(checkpointmodel.ErrRecordBinding, err),
		)
	}

	result, err := repository.applyExpected(ctx, checkpoint, action.Kind())
	if err != nil {
		return ApplyResult{}, err
	}
	if result.Status() != ApplyNeedsAttention {
		repository.nextAction++
	} else {
		repository.applyAttention = result.Attention()
	}
	return result, nil
}

func (repository *resumeLeasedRepository) applyExpected(
	ctx context.Context,
	checkpoint *resumeCheckpointPins,
	kind ActionKind,
) (ApplyResult, error) {
	if err := contextErr(ctx); err != nil {
		return ApplyResult{}, err
	}
	evidence, err := repository.revalidateNamespace()
	if err != nil {
		return ApplyResult{}, projectResumeError("revalidate before resume action", err)
	}
	if evidence != EvidenceExact {
		return repository.namespaceNeedsAttention(evidence, checkpoint.record.RecordID())
	}

	switch kind {
	case ActionRemoveStage:
		return repository.removeArtifact(checkpoint, checkpoint.stage)
	case ActionSyncStages:
		return repository.syncOwnedShard(
			checkpoint, repository.stages, checkpointstore.RecoveryStage, checkpoint.stage,
		)
	case ActionRemoveAnchor:
		return repository.removeArtifact(checkpoint, checkpoint.anchor)
	case ActionSyncAnchors:
		return repository.syncOwnedShard(
			checkpoint, repository.anchors, checkpointstore.RecoveryAnchor, checkpoint.anchor,
		)
	case ActionRemoveRecord:
		return repository.removeRecord(checkpoint)
	case ActionSyncRecords:
		return repository.syncEntryShard(checkpoint)
	default:
		return ApplyResult{},
			projectResumeError("apply unknown resume action", ErrInvalidContract)
	}
}

func (repository *resumeLeasedRepository) removeArtifact(
	checkpoint *resumeCheckpointPins,
	artifact *resumeArtifactPins,
) (ApplyResult, error) {
	if artifact == nil {
		return completedApplyResult(ApplyAlreadySatisfied)
	}
	evidence, err := repository.revalidateEntryLineage(artifact.entry)
	if err != nil {
		return ApplyResult{}, projectResumeError("revalidate owned artifact", err)
	}
	if evidence != EvidenceExact {
		return repository.entryEvidenceResult(evidence, checkpoint.record.RecordID())
	}
	file, err := openPinnedArtifact(artifact)
	if err != nil {
		if reason, attentionState := resumeObservationAttention(err); attentionState {
			return needsAttentionApplyResult(reason, checkpoint.record.RecordID().Bytes())
		}
		return ApplyResult{}, projectResumeError("open owned artifact", err)
	}
	size, sizeErr := file.Size()
	closeErr := closeFile(file)
	if sizeErr != nil || closeErr != nil {
		return ApplyResult{},
			projectResumeError("validate owned artifact", errors.Join(sizeErr, closeErr))
	}
	if size != checkpoint.record.ExactSize() {
		return needsAttentionApplyResult(
			AttentionReplacement, checkpoint.record.RecordID().Bytes(),
		)
	}

	removeErr := artifact.entry.shard.directory.RemoveEntry(artifact.entry.name, artifact.entry.pin)
	if removeErr == nil {
		return completedApplyResult(ApplyCompleted)
	}
	evidence, reconcileErr := pinnedEntryEvidence(
		artifact.entry.shard.directory, artifact.entry.name, artifact.entry.pin,
	)
	if reconcileErr != nil {
		return ApplyResult{}, projectResumeError(
			"reconcile owned artifact removal", errors.Join(removeErr, reconcileErr),
		)
	}
	switch evidence {
	case EvidenceAbsent:
		return completedApplyResult(ApplyCompleted)
	case EvidenceExact:
		return ApplyResult{}, projectResumeError("remove owned artifact", removeErr)
	default:
		return repository.entryEvidenceResult(evidence, checkpoint.record.RecordID())
	}
}

func (repository *resumeLeasedRepository) removeRecord(
	checkpoint *resumeCheckpointPins,
) (ApplyResult, error) {
	evidence, err := repository.revalidateEntryLineage(checkpoint.entry)
	if err != nil {
		return ApplyResult{}, projectResumeError("revalidate checkpoint record", err)
	}
	if evidence != EvidenceExact {
		return repository.entryEvidenceResult(evidence, checkpoint.record.RecordID())
	}
	if err := readPinnedExactFile(
		checkpoint.entry.shard.directory,
		checkpoint.entry.name,
		checkpoint.entry.pin,
		checkpoint.encoded,
	); err != nil {
		if reason, attentionState := resumeObservationAttention(err); attentionState {
			return needsAttentionApplyResult(reason, checkpoint.record.RecordID().Bytes())
		}
		return ApplyResult{}, projectResumeError("revalidate checkpoint image", err)
	}
	removeErr := checkpoint.entry.shard.directory.RemoveEntry(
		checkpoint.entry.name, checkpoint.entry.pin,
	)
	if removeErr == nil {
		return completedApplyResult(ApplyCompleted)
	}
	evidence, reconcileErr := pinnedEntryEvidence(
		checkpoint.entry.shard.directory, checkpoint.entry.name, checkpoint.entry.pin,
	)
	if reconcileErr != nil {
		return ApplyResult{}, projectResumeError(
			"reconcile checkpoint removal", errors.Join(removeErr, reconcileErr),
		)
	}
	switch evidence {
	case EvidenceAbsent:
		return completedApplyResult(ApplyCompleted)
	case EvidenceExact:
		return ApplyResult{}, projectResumeError("remove checkpoint record", removeErr)
	default:
		return repository.entryEvidenceResult(evidence, checkpoint.record.RecordID())
	}
}

func (repository *resumeLeasedRepository) syncOwnedShard(
	checkpoint *resumeCheckpointPins,
	owner *resumeOwnedDirectory,
	kind checkpointstore.RecoveryArtifactKind,
	artifact *resumeArtifactPins,
) (ApplyResult, error) {
	if owner == nil {
		return needsAttentionApplyResult(
			AttentionCorruptBinding, checkpoint.record.RecordID().Bytes(),
		)
	}
	shardName, name, err := checkpointstore.RecoveryArtifactLocation(
		checkpoint.record.OwnedOutputObject(), kind,
	)
	if err != nil {
		return ApplyResult{}, projectResumeError("locate owned recovery artifact", err)
	}
	shard := owner.shards[shardName]
	if shard == nil {
		kind, exact, err := owner.directory.ClassifyExactEntry(shardName)
		if err != nil {
			return ApplyResult{}, projectResumeError("classify absent owned shard", err)
		}
		if kind == outputcap.EntryAbsent {
			return completedApplyResult(ApplyAlreadySatisfied)
		}
		if !exact {
			return needsAttentionApplyResult(
				AttentionCorruptBinding, checkpoint.record.RecordID().Bytes(),
			)
		}
		return needsAttentionApplyResult(
			AttentionReplacement, checkpoint.record.RecordID().Bytes(),
		)
	}
	var pin outputcap.CurrentEntryReference
	if artifact != nil {
		pin = artifact.entry.pin
	}
	removed, err := removedEntryStillAbsent(shard.directory, name, pin)
	if err != nil {
		return ApplyResult{}, projectResumeError("revalidate removed artifact", err)
	}
	if !removed {
		return needsAttentionApplyResult(
			AttentionReplacement, checkpoint.record.RecordID().Bytes(),
		)
	}
	return repository.syncShard(checkpoint.record.RecordID(), shard)
}

func (repository *resumeLeasedRepository) syncEntryShard(
	checkpoint *resumeCheckpointPins,
) (ApplyResult, error) {
	recordID := checkpoint.record.RecordID()
	entry := checkpoint.entry
	if entry.shard == nil {
		return needsAttentionApplyResult(AttentionCorruptBinding, recordID.Bytes())
	}
	removed, err := removedEntryStillAbsent(entry.shard.directory, entry.name, entry.pin)
	if err != nil {
		return ApplyResult{}, projectResumeError("revalidate removed checkpoint", err)
	}
	if !removed {
		return needsAttentionApplyResult(AttentionReplacement, recordID.Bytes())
	}
	return repository.syncShard(recordID, entry.shard)
}

func removedEntryStillAbsent(
	parent outputcap.Directory,
	name string,
	pin outputcap.CurrentEntryReference,
) (bool, error) {
	if pin != nil {
		evidence, err := pinnedEntryEvidence(parent, name, pin)
		return evidence == EvidenceAbsent, err
	}
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil {
		return false, err
	}
	return exact && kind == outputcap.EntryAbsent, nil
}

func (repository *resumeLeasedRepository) syncShard(
	recordID checkpointmodel.RecordID,
	shard *resumeShardPins,
) (ApplyResult, error) {
	evidence, err := repository.revalidateShardLineage(shard)
	if err != nil {
		return ApplyResult{}, projectResumeError("revalidate sync shard", err)
	}
	if evidence != EvidenceExact {
		return repository.entryEvidenceResult(evidence, recordID)
	}
	if err := shard.directory.Sync(); err != nil {
		return ApplyResult{}, projectResumeError("sync removed recovery artifact", err)
	}
	return completedApplyResult(ApplyCompleted)
}

func (repository *resumeLeasedRepository) revalidateEntryLineage(
	entry resumeEntryPins,
) (Evidence, error) {
	if entry.shard == nil || entry.pin == nil {
		return EvidenceAmbiguous, transfer.ErrInvalidOutputBinding
	}
	evidence, err := repository.revalidateShardLineage(entry.shard)
	if err != nil || evidence != EvidenceExact {
		return evidence, err
	}
	return pinnedEntryEvidence(entry.shard.directory, entry.name, entry.pin)
}

func (repository *resumeLeasedRepository) revalidateShardLineage(
	shard *resumeShardPins,
) (Evidence, error) {
	if shard == nil || shard.owner == nil || shard.owner.pin == nil || shard.pin == nil {
		return EvidenceAmbiguous, transfer.ErrInvalidOutputBinding
	}
	evidence, err := pinnedEntryEvidence(repository.intent, shard.owner.name, shard.owner.pin)
	if err != nil || evidence != EvidenceExact {
		return evidence, err
	}
	return pinnedEntryEvidence(shard.owner.directory, shard.name, shard.pin)
}

func (repository *resumeLeasedRepository) namespaceNeedsAttention(
	evidence Evidence,
	recordID checkpointmodel.RecordID,
) (ApplyResult, error) {
	reason := AttentionCorruptBinding
	switch evidence {
	case EvidenceAbsent:
		reason = AttentionMissingOwnership
	case EvidenceReplaced:
		reason = AttentionReplacement
	}
	return needsAttentionApplyResult(reason, recordID.Bytes())
}

func (repository *resumeLeasedRepository) entryEvidenceResult(
	evidence Evidence,
	recordID checkpointmodel.RecordID,
) (ApplyResult, error) {
	if evidence == EvidenceAbsent {
		return completedApplyResult(ApplyAlreadySatisfied)
	}
	return repository.namespaceNeedsAttention(evidence, recordID)
}

func completedApplyResult(status ApplyStatus) (ApplyResult, error) {
	return NewApplyResult(status, nil)
}

func needsAttentionApplyResult(
	reason AttentionReason,
	scope []byte,
) (ApplyResult, error) {
	return NewApplyResult(
		ApplyNeedsAttention,
		[]Attention{resumeAdapterAttention(reason, scope)},
	)
}
