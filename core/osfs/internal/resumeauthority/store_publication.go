package resumeauthority

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

type resumePinnedCheckpoint struct {
	repository *resumeLeasedRepository
	recordID   checkpointmodel.RecordID
	record     checkpointmodel.Record
}

func (repository *resumeLeasedRepository) PinnedCheckpoint(
	recordID checkpointmodel.RecordID,
) (PinnedCheckpoint, bool) {
	if repository == nil || recordID.IsZero() {
		return nil, false
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	checkpoint := repository.checkpointPins[recordID]
	if repository.closed || !repository.observed || checkpoint == nil ||
		checkpoint.record.RecordID() != recordID {
		return nil, false
	}
	return &resumePinnedCheckpoint{
		repository: repository,
		recordID:   recordID,
		record:     checkpoint.record,
	}, true
}

func (checkpoint *resumePinnedCheckpoint) Record() checkpointmodel.Record {
	if checkpoint == nil {
		return checkpointmodel.Record{}
	}
	return checkpoint.record
}

func (checkpoint *resumePinnedCheckpoint) SameOwnedFile(
	ctx context.Context,
	public outputcap.File,
) (Evidence, error) {
	if err := contextErr(ctx); err != nil {
		return EvidenceAmbiguous, err
	}
	if checkpoint == nil || checkpoint.repository == nil || checkpoint.recordID.IsZero() || public == nil {
		return EvidenceAmbiguous,
			projectResumeError("compare published final", transfer.ErrInvalidOutputBinding)
	}
	repository := checkpoint.repository
	repository.mu.Lock()
	defer repository.mu.Unlock()
	if err := contextErr(ctx); err != nil {
		return EvidenceAmbiguous, err
	}
	if repository.closed || !repository.observed {
		return EvidenceAmbiguous,
			projectResumeError("compare closed published final", transfer.ErrInvalidOutputBinding)
	}
	retained := repository.checkpointPins[checkpoint.recordID]
	if retained == nil || retained.record.RecordID() != checkpoint.recordID ||
		retained.record.Checksum() != checkpoint.record.Checksum() {
		return EvidenceAmbiguous, nil
	}
	evidence, err := repository.revalidateNamespace()
	if err != nil {
		return EvidenceAmbiguous,
			projectResumeError("revalidate before published comparison", err)
	}
	if evidence != EvidenceExact {
		return EvidenceAmbiguous, nil
	}
	evidence, err = repository.revalidateEntryLineage(retained.entry)
	if err != nil {
		return EvidenceAmbiguous,
			projectResumeError("revalidate published checkpoint", err)
	}
	if evidence != EvidenceExact {
		return EvidenceAmbiguous, nil
	}
	if err := readPinnedExactFile(
		retained.entry.shard.directory,
		retained.entry.name,
		retained.entry.pin,
		retained.encoded,
	); err != nil {
		if _, attentionState := resumeObservationAttention(err); attentionState {
			return EvidenceAmbiguous, nil
		}
		return EvidenceAmbiguous,
			projectResumeError("revalidate published checkpoint image", err)
	}
	if retained.anchor == nil {
		return EvidenceAmbiguous, nil
	}
	evidence, err = repository.revalidateEntryLineage(retained.anchor.entry)
	if err != nil {
		return EvidenceAmbiguous,
			projectResumeError("revalidate published anchor", err)
	}
	if evidence != EvidenceExact {
		return EvidenceAmbiguous, nil
	}
	anchor, err := openPinnedArtifact(retained.anchor)
	if err != nil {
		if _, attentionState := resumeObservationAttention(err); attentionState {
			return EvidenceAmbiguous, nil
		}
		return EvidenceAmbiguous,
			projectResumeError("open published anchor", err)
	}
	anchorSize, sizeErr := anchor.Size()
	same, sameErr := public.SameFile(anchor)
	closeErr := closeFile(anchor)
	if sizeErr != nil || sameErr != nil || closeErr != nil {
		return EvidenceAmbiguous, projectResumeError(
			"compare published final with anchor",
			errors.Join(sizeErr, sameErr, closeErr),
		)
	}
	if !same {
		return EvidenceReplaced, nil
	}
	if anchorSize != retained.record.ExactSize() {
		return EvidenceAmbiguous, nil
	}
	return EvidenceExact, nil
}

var _ PinnedCheckpointProvider = (*resumeLeasedRepository)(nil)
var _ PinnedCheckpoint = (*resumePinnedCheckpoint)(nil)
