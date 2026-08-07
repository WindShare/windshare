package resumestate

import (
	"bytes"
	"fmt"
	"slices"
)

func CheckpointIdentityEqual(left, right FileCheckpointV1) bool {
	return left.recordID == right.recordID && left.intentDigest == right.intentDigest &&
		left.fileID == right.fileID && left.fileRevision == right.fileRevision &&
		left.canonicalPath == right.canonicalPath && left.exactSize == right.exactSize &&
		left.backendID == right.backendID && left.rootIdentity == right.rootIdentity &&
		left.ownedOutputObject == right.ownedOutputObject
}

// ValidateCheckpointTransition enforces monotonic state/checkpoint generations
// and prevents a crash-recovered writer from shrinking already verified ranges.
func ValidateCheckpointTransition(previous, next FileCheckpointV1) error {
	if err := previous.valid(); err != nil {
		return err
	}
	if err := next.valid(); err != nil {
		return err
	}
	if !CheckpointIdentityEqual(previous, next) {
		return ErrFileCheckpointBinding
	}
	if next.stateGeneration < previous.stateGeneration || next.checkpointGeneration < previous.checkpointGeneration {
		return ErrFileCheckpointGeneration
	}
	if next.checkpointGeneration == previous.checkpointGeneration {
		if previous.commitState == FileCheckpointCommitCandidate {
			// Candidate -> committed is the atomic data cut and therefore keeps both
			// generations unchanged.
			if next.commitState <= previous.commitState ||
				next.stateGeneration != previous.stateGeneration ||
				!slices.Equal(previous.verifiedRanges, next.verifiedRanges) ||
				(previous.phase != next.phase &&
					(previous.phase != FileCheckpointPhasePublishing || next.phase != FileCheckpointPhasePublished)) {
				return ErrFileCheckpointGeneration
			}
		} else if next.stateGeneration <= previous.stateGeneration ||
			!slices.Equal(previous.verifiedRanges, next.verifiedRanges) ||
			!validCheckpointLifecycleTransition(previous, next) {
			// Lifecycle is independent of data generation. Publishing an empty file,
			// for example, must advance durable state while its verified range set and
			// checkpoint generation both remain empty/zero.
			return ErrFileCheckpointGeneration
		}
	}
	if previous.commitState == FileCheckpointCommitPublished || previous.phase == FileCheckpointPhasePublished {
		return fmt.Errorf("%w: published record is immutable", ErrFileCheckpointGeneration)
	}
	if !checkpointRangesContain(next.verifiedRanges, previous.verifiedRanges) {
		return fmt.Errorf("%w: verified ranges regressed", ErrFileCheckpointGeneration)
	}
	return nil
}

func validCheckpointLifecycleTransition(previous, next FileCheckpointV1) bool {
	if next.commitState < previous.commitState {
		return false
	}
	switch next.phase {
	case FileCheckpointPhasePublished:
		if next.commitState != FileCheckpointCommitPublished {
			return false
		}
	case FileCheckpointPhaseQuarantined:
		if next.commitState != FileCheckpointCommitQuarantined {
			return false
		}
	default:
		if next.commitState != FileCheckpointCommitVerified {
			return false
		}
	}
	switch previous.phase {
	case FileCheckpointPhaseReserved:
		return next.phase == FileCheckpointPhaseActive || next.phase == FileCheckpointPhaseRetired ||
			next.phase == FileCheckpointPhaseQuarantined
	case FileCheckpointPhaseActive:
		return next.phase == FileCheckpointPhasePaused || next.phase == FileCheckpointPhasePublishing ||
			next.phase == FileCheckpointPhaseRetired || next.phase == FileCheckpointPhaseQuarantined
	case FileCheckpointPhasePaused:
		return next.phase == FileCheckpointPhaseActive || next.phase == FileCheckpointPhasePublishing ||
			next.phase == FileCheckpointPhaseRetired || next.phase == FileCheckpointPhaseQuarantined
	case FileCheckpointPhasePublishing:
		return next.phase == FileCheckpointPhasePaused || next.phase == FileCheckpointPhasePublished ||
			next.phase == FileCheckpointPhaseRetired || next.phase == FileCheckpointPhaseQuarantined
	default:
		return false
	}
}

func checkpointRangesContain(container, required []FileCheckpointRange) bool {
	for _, wanted := range required {
		found := false
		for _, current := range container {
			if current.Offset <= wanted.Offset && current.End >= wanted.End {
				found = true
				break
			}
			if current.Offset > wanted.Offset {
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// SelectVerifiedCheckpoint ignores candidates by design.  Only a committed
// (verified or published) record can grant have-state after a crash; candidates
// are merely write-ahead evidence and must be discarded or retried by the
// backend transaction.
func SelectVerifiedCheckpoint(records ...FileCheckpointV1) (FileCheckpointV1, error) {
	var selected FileCheckpointV1
	for _, candidate := range records {
		if err := candidate.valid(); err != nil {
			return FileCheckpointV1{}, err
		}
		if candidate.commitState != FileCheckpointCommitVerified && candidate.commitState != FileCheckpointCommitPublished {
			continue
		}
		if selected.recordID.IsZero() {
			selected = candidate
			continue
		}
		if !CheckpointIdentityEqual(selected, candidate) {
			return FileCheckpointV1{}, ErrFileCheckpointBinding
		}
		if candidate.checkpointGeneration > selected.checkpointGeneration {
			selected = candidate
		} else if candidate.checkpointGeneration == selected.checkpointGeneration &&
			!bytes.Equal(candidate.canonicalPayload(), selected.canonicalPayload()) {
			// Two committed records at one generation disagree about ranges or
			// lifecycle state.  There is no ordering evidence to resolve that crash
			// cut, so fail closed instead of selecting whichever directory entry wins.
			return FileCheckpointV1{}, ErrFileCheckpointCrashBoundary
		}
	}
	if selected.recordID.IsZero() {
		return FileCheckpointV1{}, ErrFileCheckpointRecovery
	}
	return selected, nil
}

// RecoverFileCheckpoint is a convenient two-slot crash-cut operation.  A
// malformed candidate never displaces the last committed record.
func RecoverFileCheckpoint(committed, candidate *FileCheckpointV1) (FileCheckpointV1, error) {
	if committed == nil {
		return FileCheckpointV1{}, ErrFileCheckpointRecovery
	}
	if _, err := SelectVerifiedCheckpoint(*committed); err != nil {
		return FileCheckpointV1{}, err
	}
	if candidate == nil {
		return *committed, nil
	}
	if err := candidate.valid(); err != nil || !CheckpointIdentityEqual(*committed, *candidate) {
		return *committed, nil
	}
	if candidate.commitState == FileCheckpointCommitCandidate {
		return *committed, nil
	}
	if candidate.checkpointGeneration < committed.checkpointGeneration {
		return *committed, nil
	}
	if candidate.checkpointGeneration == committed.checkpointGeneration &&
		!bytes.Equal(candidate.canonicalPayload(), committed.canonicalPayload()) {
		return *committed, nil
	}
	return *candidate, nil
}

// CheckpointGenerationAdvance produces the next candidate without changing the
// immutable identity.  Backends should write this candidate, flush it, then mark
// the same record Verified only after reopen/identity verification.
func CheckpointGenerationAdvance(
	previous FileCheckpointV1,
	ranges []FileCheckpointRange,
	phase FileCheckpointPhase,
	commitState FileCheckpointCommitState,
) (FileCheckpointV1, error) {
	if err := previous.valid(); err != nil {
		return FileCheckpointV1{}, err
	}
	if previous.stateGeneration == ^uint64(0) || previous.checkpointGeneration == ^uint64(0) {
		return FileCheckpointV1{}, ErrFileCheckpointGeneration
	}
	nextSpec := FileCheckpointSpec{
		OwnershipMarker: previous.ownershipMarker, Namespace: previous.namespace,
		TransferIntentDigest: previous.intentDigest, FileID: previous.fileID, FileRevision: previous.fileRevision,
		CanonicalPath: previous.canonicalPath, ExactSize: previous.exactSize, BackendID: string(previous.backendID),
		RootIdentity: previous.rootIdentity.Bytes(), OwnedOutputObject: previous.ownedOutputObject.Bytes(),
		StateGeneration: previous.stateGeneration + 1, CheckpointGeneration: previous.checkpointGeneration + 1,
		VerifiedRanges: ranges, Phase: phase, CommitState: commitState,
		QuarantineReason:      previous.quarantineReason,
		PhaseBeforeQuarantine: previous.phaseBeforeQuarantine,
		RetirementReason:      previous.retirementReason,
	}
	next, err := NewFileCheckpointV1(nextSpec)
	if err != nil {
		return FileCheckpointV1{}, err
	}
	if err := ValidateCheckpointTransition(previous, next); err != nil {
		return FileCheckpointV1{}, err
	}
	return next, nil
}

// AdvanceCheckpointState records a lifecycle-only transition without claiming
// new durable bytes. State generation advances independently while checkpoint
// generation and verified ranges remain unchanged.
func AdvanceCheckpointState(
	previous FileCheckpointV1,
	stateGeneration uint64,
	phase FileCheckpointPhase,
	commitState FileCheckpointCommitState,
	quarantineReason QuarantineReason,
	phaseBeforeQuarantine FilePhase,
	retirementReason RetirementReason,
) (FileCheckpointV1, error) {
	if err := previous.valid(); err != nil || stateGeneration <= previous.stateGeneration {
		return FileCheckpointV1{}, ErrFileCheckpointGeneration
	}
	next, err := NewFileCheckpointV1(FileCheckpointSpec{
		OwnershipMarker: previous.ownershipMarker, Namespace: previous.namespace,
		TransferIntentDigest: previous.intentDigest, FileID: previous.fileID, FileRevision: previous.fileRevision,
		CanonicalPath: previous.canonicalPath, ExactSize: previous.exactSize, BackendID: string(previous.backendID),
		RootIdentity: previous.rootIdentity.Bytes(), OwnedOutputObject: previous.ownedOutputObject.Bytes(),
		StateGeneration: stateGeneration, CheckpointGeneration: previous.checkpointGeneration,
		VerifiedRanges: previous.verifiedRanges, Phase: phase, CommitState: commitState,
		QuarantineReason: quarantineReason, PhaseBeforeQuarantine: phaseBeforeQuarantine,
		RetirementReason: retirementReason,
	})
	if err != nil {
		return FileCheckpointV1{}, err
	}
	if err := ValidateCheckpointTransition(previous, next); err != nil {
		return FileCheckpointV1{}, err
	}
	return next, nil
}

// PromoteCheckpoint performs the candidate -> committed cut without changing
// the data generation.  Backends call it only after data/journal flush and
// reopen/identity verification; callers must still atomically replace the
// persisted bytes.
func PromoteCheckpoint(candidate FileCheckpointV1, phase FileCheckpointPhase, commitState FileCheckpointCommitState) (FileCheckpointV1, error) {
	if err := candidate.valid(); err != nil {
		return FileCheckpointV1{}, err
	}
	if candidate.commitState != FileCheckpointCommitCandidate ||
		(commitState != FileCheckpointCommitVerified && commitState != FileCheckpointCommitPublished) {
		return FileCheckpointV1{}, ErrFileCheckpointCrashBoundary
	}
	next, err := NewFileCheckpointV1(FileCheckpointSpec{
		OwnershipMarker: candidate.ownershipMarker, Namespace: candidate.namespace,
		TransferIntentDigest: candidate.intentDigest, FileID: candidate.fileID, FileRevision: candidate.fileRevision,
		CanonicalPath: candidate.canonicalPath, ExactSize: candidate.exactSize, BackendID: string(candidate.backendID),
		RootIdentity: candidate.rootIdentity.Bytes(), OwnedOutputObject: candidate.ownedOutputObject.Bytes(),
		StateGeneration: candidate.stateGeneration, CheckpointGeneration: candidate.checkpointGeneration,
		VerifiedRanges: candidate.verifiedRanges, Phase: phase, CommitState: commitState,
		QuarantineReason:      candidate.quarantineReason,
		PhaseBeforeQuarantine: candidate.phaseBeforeQuarantine,
		RetirementReason:      candidate.retirementReason,
	})
	if err != nil {
		return FileCheckpointV1{}, err
	}
	if err := ValidateCheckpointTransition(candidate, next); err != nil {
		return FileCheckpointV1{}, err
	}
	return next, nil
}
