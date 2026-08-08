package resumeauthority

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func (repository *resumeLeasedRepository) scanRecords(
	ctx context.Context,
) (map[checkpointmodel.RecordID]*resumeCheckpointPins, []Attention, error) {
	result := make(map[checkpointmodel.RecordID]*resumeCheckpointPins)
	attention := make([]Attention, 0)
	budget := newResumeScanBudget()
	err := scanResumeShards(ctx, repository.records, &budget, func(
		reason AttentionReason,
		scope []byte,
	) {
		attention = append(attention, resumeAdapterAttention(reason, scope))
	}, func(
		shard *resumeShardPins,
		name string,
	) error {
		if checkpointstore.IsTemporaryName(name) {
			attention = append(attention, resumeAdapterAttention(
				AttentionCorruptBinding, []byte(shard.name+"/"+name),
			))
			return nil
		}
		recordID, parseErr := checkpointstore.ParseRecordLocation(shard.name, name)
		if parseErr != nil {
			attention = append(attention, resumeAdapterAttention(
				AttentionUnknownChildren, []byte(shard.name+"/"+name),
			))
			return nil
		}
		entry, pinErr := pinRegularEntry(shard, name)
		if pinErr != nil {
			if reason, attentionState := resumeObservationAttention(pinErr); attentionState {
				attention = append(attention, resumeAdapterAttention(reason, recordID.Bytes()))
				return nil
			}
			return pinErr
		}
		encoded, readErr := readPinnedFile(entry)
		if readErr != nil {
			_ = closeEntryReference(entry.pin)
			if reason, attentionState := resumeObservationAttention(readErr); attentionState {
				attention = append(attention, resumeAdapterAttention(reason, recordID.Bytes()))
				return nil
			}
			return readErr
		}
		record, decodeErr := checkpointmodel.DecodeRecord(encoded)
		if decodeErr != nil || !checkpointmodel.Committed(record) ||
			!repository.binding.Matches(record, recordID) {
			_ = closeEntryReference(entry.pin)
			attention = append(attention, resumeAdapterAttention(
				AttentionCorruptBinding, recordID.Bytes(),
			))
			return nil
		}
		checkpoint := &resumeCheckpointPins{
			record: record, encoded: encoded, entry: entry,
		}
		result[recordID] = checkpoint
		repository.checkpointPins[recordID] = checkpoint
		return nil
	})
	if err != nil {
		if errors.Is(err, errResumeUnknownChildren) {
			attention = append(attention, resumeAdapterAttention(
				AttentionUnknownChildren, []byte(checkpointstore.RecordsDirectory+"-overflow"),
			))
			return result, attention, nil
		}
		return nil, nil, projectResumeError("scan checkpoint records", err)
	}
	return result, attention, nil
}

func (repository *resumeLeasedRepository) scanArtifacts(
	ctx context.Context,
	owner *resumeOwnedDirectory,
	kind checkpointstore.RecoveryArtifactKind,
) (map[checkpointmodel.ObjectID]*resumeArtifactPins, []Attention, error) {
	result := make(map[checkpointmodel.ObjectID]*resumeArtifactPins)
	attention := make([]Attention, 0)
	budget := newResumeScanBudget()
	err := scanResumeShards(ctx, owner, &budget, func(
		reason AttentionReason,
		scope []byte,
	) {
		attention = append(attention, resumeAdapterAttention(reason, scope))
	}, func(
		shard *resumeShardPins,
		name string,
	) error {
		object, parseErr := checkpointstore.ParseRecoveryArtifactLocation(shard.name, name, kind)
		if parseErr != nil {
			attention = append(attention, resumeAdapterAttention(
				AttentionUnknownChildren, []byte(owner.name+"/"+shard.name+"/"+name),
			))
			return nil
		}
		entry, pinErr := pinRegularEntry(shard, name)
		if pinErr != nil {
			if reason, attentionState := resumeObservationAttention(pinErr); attentionState {
				attention = append(attention, resumeAdapterAttention(reason, object.Bytes()))
				return nil
			}
			return pinErr
		}
		artifact := &resumeArtifactPins{object: object, entry: entry}
		result[object] = artifact
		repository.artifactPins = append(repository.artifactPins, artifact)
		return nil
	})
	if err != nil {
		if errors.Is(err, errResumeUnknownChildren) {
			attention = append(attention, resumeAdapterAttention(
				AttentionUnknownChildren, []byte(owner.name+"-overflow"),
			))
			return result, attention, nil
		}
		return nil, nil, projectResumeError("scan owned recovery artifacts", err)
	}
	return result, attention, nil
}

type resumeScanBudget struct {
	canonical int
	auxiliary int
}

func newResumeScanBudget() resumeScanBudget {
	return resumeScanBudget{}
}

func (budget *resumeScanBudget) namesLimit() int {
	return checkpointmodel.MaxCheckpointRecordsPerIntent - budget.canonical +
		checkpointmodel.MaxCheckpointAuxiliaryEntriesPerIntent - budget.auxiliary + 1
}

func (budget *resumeScanBudget) observe(canonical bool) error {
	if canonical {
		if budget.canonical >= checkpointmodel.MaxCheckpointRecordsPerIntent {
			return errResumeUnknownChildren
		}
		budget.canonical++
		return nil
	}
	if budget.auxiliary >= checkpointmodel.MaxCheckpointAuxiliaryEntriesPerIntent {
		return errResumeUnknownChildren
	}
	budget.auxiliary++
	return nil
}

func scanResumeShards(
	ctx context.Context,
	owner *resumeOwnedDirectory,
	budget *resumeScanBudget,
	unknown func(AttentionReason, []byte),
	visit func(*resumeShardPins, string) error,
) error {
	shardNames, err := owner.directory.Names(checkpointstore.ShardLimit)
	if err != nil {
		return err
	}
	if len(shardNames) >= checkpointstore.ShardLimit {
		return errResumeUnknownChildren
	}
	slices.Sort(shardNames)
	for _, shardName := range shardNames {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if err := scanResumeShard(owner, shardName, budget, unknown, visit); err != nil {
			return err
		}
	}
	return nil
}

func scanResumeShard(
	owner *resumeOwnedDirectory,
	shardName string,
	budget *resumeScanBudget,
	unknown func(AttentionReason, []byte),
	visit func(*resumeShardPins, string) error,
) error {
	shard, available, err := pinResumeShard(owner, shardName, budget, unknown)
	if err != nil || !available {
		return err
	}
	return scanResumeShardEntries(shard, budget, visit)
}

func pinResumeShard(
	owner *resumeOwnedDirectory,
	shardName string,
	budget *resumeScanBudget,
	unknown func(AttentionReason, []byte),
) (*resumeShardPins, bool, error) {
	if !checkpointstore.ValidShard(shardName) {
		err := observeUnknownResumeShard(
			owner, shardName, budget, unknown, AttentionUnknownChildren,
		)
		return nil, false, err
	}
	pin, directory, err := pinExistingDirectory(owner.directory, shardName)
	if err == nil {
		shard := &resumeShardPins{owner: owner, name: shardName, pin: pin, directory: directory}
		owner.shards[shardName] = shard
		return shard, true, nil
	}
	reason, attentionState := resumeObservationAttention(err)
	if !attentionState {
		return nil, false, err
	}
	return nil, false, observeUnknownResumeShard(owner, shardName, budget, unknown, reason)
}

func observeUnknownResumeShard(
	owner *resumeOwnedDirectory,
	shardName string,
	budget *resumeScanBudget,
	unknown func(AttentionReason, []byte),
	reason AttentionReason,
) error {
	if err := budget.observe(false); err != nil {
		return err
	}
	unknown(reason, []byte(owner.name+"/"+shardName))
	return nil
}

func scanResumeShardEntries(
	shard *resumeShardPins,
	budget *resumeScanBudget,
	visit func(*resumeShardPins, string) error,
) error {
	limit := budget.namesLimit()
	names, err := shard.directory.Names(limit)
	if err != nil {
		return err
	}
	if len(names) >= limit {
		return errResumeUnknownChildren
	}
	slices.Sort(names)
	for _, name := range names {
		canonical := resumeEntryCanonical(shard.owner.name, shard.name, name)
		if err := budget.observe(canonical); err != nil {
			return err
		}
		if err := visit(shard, name); err != nil {
			return err
		}
	}
	return nil
}

func resumeEntryCanonical(owner, shard, name string) bool {
	switch owner {
	case checkpointstore.RecordsDirectory:
		_, err := checkpointstore.ParseRecordLocation(shard, name)
		return err == nil
	case checkpointstore.StagesDirectory:
		_, err := checkpointstore.ParseRecoveryArtifactLocation(shard, name, checkpointstore.RecoveryStage)
		return err == nil
	case checkpointstore.AnchorsDirectory:
		_, err := checkpointstore.ParseRecoveryArtifactLocation(shard, name, checkpointstore.RecoveryAnchor)
		return err == nil
	default:
		return false
	}
}

func pinRegularEntry(shard *resumeShardPins, name string) (resumeEntryPins, error) {
	kind, exact, err := shard.directory.ClassifyExactEntry(name)
	if err != nil {
		return resumeEntryPins{}, err
	}
	if kind == outputcap.EntryAbsent {
		return resumeEntryPins{}, errResumePinReplaced
	}
	if !exact || kind != outputcap.EntryRegularFile {
		return resumeEntryPins{}, outputcap.ErrUnsafeNamespace
	}
	pin, err := shard.directory.OpenEntry(name)
	if err != nil {
		return resumeEntryPins{}, errors.Join(err, closeEntryReference(pin))
	}
	if pin == nil || pin.Kind() != outputcap.EntryRegularFile {
		return resumeEntryPins{}, errors.Join(outputcap.ErrUnsafeNamespace, closeEntryReference(pin))
	}
	evidence, err := pinnedEntryEvidence(shard.directory, name, pin)
	if err != nil || evidence != EvidenceExact {
		return resumeEntryPins{}, errors.Join(errResumePinReplaced, err, closeEntryReference(pin))
	}
	return resumeEntryPins{shard: shard, name: name, pin: pin}, nil
}

func readPinnedFile(entry resumeEntryPins) ([]byte, error) {
	return readPinnedFileBytes(entry.shard.directory, entry.name, entry.pin)
}

func validateResumeArtifacts(
	checkpoint *resumeCheckpointPins,
) (
	stageEvidence Evidence,
	anchorEvidence Evidence,
	attention []Attention,
	resultErr error,
) {
	stageEvidence = EvidenceAbsent
	anchorEvidence = EvidenceAbsent
	attention = make([]Attention, 0)
	var stageFile, anchorFile outputcap.File
	defer func() {
		resultErr = errors.Join(resultErr, closeFile(stageFile), closeFile(anchorFile))
	}()
	stageFile, stageEvidence, stageAttention, err := validateResumeArtifact(checkpoint, checkpoint.stage)
	if err != nil {
		return 0, 0, nil, err
	}
	anchorFile, anchorEvidence, anchorAttention, err := validateResumeArtifact(checkpoint, checkpoint.anchor)
	if err != nil {
		return 0, 0, nil, err
	}
	attention = slices.Concat(stageAttention, anchorAttention)
	for _, current := range []struct {
		file     outputcap.File
		evidence *Evidence
	}{
		{stageFile, &stageEvidence},
		{anchorFile, &anchorEvidence},
	} {
		if current.file == nil {
			continue
		}
		size, sizeErr := current.file.Size()
		if sizeErr != nil {
			return 0, 0, nil, sizeErr
		}
		if size != checkpoint.record.ExactSize() {
			*current.evidence = EvidenceReplaced
			attention = append(attention, resumeAdapterAttention(
				AttentionReplacement, checkpoint.record.RecordID().Bytes(),
			))
		}
	}
	if stageFile != nil && anchorFile != nil {
		same, sameErr := stageFile.SameFile(anchorFile)
		if sameErr != nil {
			return 0, 0, nil, sameErr
		}
		if !same {
			stageEvidence = EvidenceReplaced
			anchorEvidence = EvidenceReplaced
			attention = append(attention, resumeAdapterAttention(
				AttentionReplacement, checkpoint.record.RecordID().Bytes(),
			))
		}
	}
	return stageEvidence, anchorEvidence, attention, nil
}

func validateResumeArtifact(
	checkpoint *resumeCheckpointPins,
	artifact *resumeArtifactPins,
) (outputcap.File, Evidence, []Attention, error) {
	if artifact == nil {
		return nil, EvidenceAbsent, nil, nil
	}
	file, err := openPinnedArtifact(artifact)
	if err == nil {
		return file, EvidenceExact, nil, nil
	}
	reason, attentionState := resumeObservationAttention(err)
	if !attentionState {
		return nil, 0, nil, err
	}
	attention := resumeAdapterAttention(reason, checkpoint.record.RecordID().Bytes())
	return nil, EvidenceReplaced, []Attention{attention}, nil
}

func openPinnedArtifact(artifact *resumeArtifactPins) (outputcap.File, error) {
	evidence, err := pinnedEntryEvidence(
		artifact.entry.shard.directory, artifact.entry.name, artifact.entry.pin,
	)
	if err != nil || evidence != EvidenceExact {
		return nil, errors.Join(errResumePinReplaced, err)
	}
	file, err := artifact.entry.shard.directory.OpenFile(artifact.entry.name, true, false)
	if err != nil {
		return nil, errors.Join(err, closeFile(file))
	}
	evidence, matchErr := pinnedEntryEvidence(
		artifact.entry.shard.directory, artifact.entry.name, artifact.entry.pin,
	)
	if matchErr != nil || evidence != EvidenceExact {
		return nil, errors.Join(errResumePinReplaced, matchErr, closeFile(file))
	}
	return file, nil
}

func (repository *resumeLeasedRepository) appendExpectedActions(
	checkpoint *resumeCheckpointPins,
	stageEvidence Evidence,
	anchorEvidence Evidence,
) {
	recordID := checkpoint.record.RecordID()
	if stageEvidence == EvidenceExact {
		repository.expected = append(repository.expected,
			resumeExpectedAction{kind: ActionRemoveStage, recordID: recordID})
	}
	repository.expected = append(repository.expected,
		resumeExpectedAction{kind: ActionSyncStages, recordID: recordID})
	if anchorEvidence == EvidenceExact {
		repository.expected = append(repository.expected,
			resumeExpectedAction{kind: ActionRemoveAnchor, recordID: recordID})
	}
	repository.expected = append(repository.expected,
		resumeExpectedAction{kind: ActionSyncAnchors, recordID: recordID},
		resumeExpectedAction{kind: ActionRemoveRecord, recordID: recordID},
		resumeExpectedAction{kind: ActionSyncRecords, recordID: recordID},
	)
}

func compareRecordIDs(left, right checkpointmodel.RecordID) int {
	return bytes.Compare(left.Bytes(), right.Bytes())
}

func resumeObservationAttention(err error) (AttentionReason, bool) {
	switch {
	case errors.Is(err, errResumePinReplaced), errors.Is(err, fs.ErrNotExist):
		return AttentionReplacement, true
	case errors.Is(err, outputcap.ErrUnsafeNamespace),
		errors.Is(err, checkpointmodel.ErrInvalidRecord),
		errors.Is(err, checkpointmodel.ErrRecordBinding),
		errors.Is(err, checkpointmodel.ErrRecordChecksum),
		errors.Is(err, checkpointmodel.ErrRecordNonCanonical):
		return AttentionCorruptBinding, true
	default:
		return "", false
	}
}
