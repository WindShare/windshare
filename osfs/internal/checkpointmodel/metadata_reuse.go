package checkpointmodel

import (
	"slices"

	"github.com/windshare/windshare/core/content"
)

// AdmitMetadataReuse accepts durable plaintext for a freshly authenticated
// revision. The caller must separately reopen and authorize the same local object.
func AdmitMetadataReuse(previous Record, current content.FileRevisionDescriptor) (Record, error) {
	if !previous.Valid() || previous.FileID() != current.FileID() ||
		previous.FileRevision() == current.FileRevision() ||
		!content.CompareRevisionMetadata(previous.FileRevision(), previous.ExactSize(), previous.SourceMetadata(), current).AllowsReuse() {
		return Record{}, ErrRecordBinding
	}
	next := previous
	next.fileRevision = current.FileRevision()
	next.reusedFrom = previous.FileRevision()
	next.stateGeneration++
	next.checkpointGeneration++
	next.checksum = next.derivedChecksum()
	if err := validateMetadataReuse(previous, next); err != nil {
		return Record{}, err
	}
	return next, nil
}

func validateMetadataReuse(previous, next Record) error {
	// Published deliveries, invalidated files and uncommitted candidates retain
	// their historical meaning. Only unfinished, durable plaintext is reusable.
	if !SameCheckpointLineage(previous, next) || previous.ownedObjectID != next.ownedObjectID ||
		previous.fileRevision == next.fileRevision || next.reusedFrom != previous.fileRevision ||
		previous.sourceMetadata.IsZero() || previous.sourceMetadata != next.sourceMetadata ||
		previous.exactSize != next.exactSize ||
		(previous.phase != PhaseActive && previous.phase != PhasePaused) || previous.phase != next.phase ||
		previous.commitState != CommitVerified || next.commitState != CommitVerified ||
		previous.stateGeneration == ^uint64(0) || previous.checkpointGeneration == ^uint64(0) ||
		next.stateGeneration != previous.stateGeneration+1 || next.checkpointGeneration != previous.checkpointGeneration+1 ||
		!slices.Equal(previous.verifiedRanges, next.verifiedRanges) {
		return ErrRecordBinding
	}
	return nil
}
