package checkpointmodel

import (
	"bytes"
	"fmt"
	"slices"
)

func IdentityEqual(left, right Record) bool {
	return left.recordID == right.recordID && left.operationID == right.operationID &&
		left.receiveIntentDigest == right.receiveIntentDigest &&
		left.materializationBindingDigest == right.materializationBindingDigest &&
		left.fileID == right.fileID && left.fileRevision == right.fileRevision &&
		left.canonicalPath == right.canonicalPath && left.exactSize == right.exactSize &&
		left.materializerKind == right.materializerKind && left.authorityRef == right.authorityRef &&
		left.ownedObjectID == right.ownedObjectID
}

// ValidateTransition prevents stale writers from changing immutable identity,
// regressing generations, or shrinking already verified ranges.
func ValidateTransition(previous, next Record) error {
	if err := previous.validate(); err != nil {
		return err
	}
	if err := next.validate(); err != nil {
		return err
	}
	if !IdentityEqual(previous, next) {
		return ErrRecordBinding
	}
	if next.stateGeneration < previous.stateGeneration ||
		next.checkpointGeneration < previous.checkpointGeneration {
		return ErrRecordGeneration
	}
	if err := validateGenerationCut(previous, next); err != nil {
		return err
	}
	if previous.commitState == CommitPublished || previous.phase == PhasePublished {
		return fmt.Errorf("%w: published record is immutable", ErrRecordGeneration)
	}
	if !rangesContain(next.verifiedRanges, previous.verifiedRanges) {
		return fmt.Errorf("%w: verified ranges regressed", ErrRecordGeneration)
	}
	return nil
}

func validateGenerationCut(previous, next Record) error {
	if next.checkpointGeneration == previous.checkpointGeneration {
		return validateStateCut(previous, next)
	}
	return validateCheckpointCut(previous, next)
}

func validateStateCut(previous, next Record) error {
	if previous.commitState == CommitCandidate {
		return validateCandidatePromotion(previous, next)
	}
	if next.stateGeneration <= previous.stateGeneration ||
		!slices.Equal(previous.verifiedRanges, next.verifiedRanges) ||
		!ValidLifecycleTransition(
			previous.phase,
			previous.commitState,
			next.phase,
			next.commitState,
		) {
		return ErrRecordGeneration
	}
	return nil
}

func validateCandidatePromotion(previous, next Record) error {
	// Promotion commits the existing write-ahead image, so it cannot create a
	// new generation or change the bytes whose durability it is certifying.
	phasePreserved := previous.phase == next.phase
	publishingCommitted := previous.phase == PhasePublishing && next.phase == PhasePublished
	if next.commitState <= previous.commitState ||
		next.stateGeneration != previous.stateGeneration ||
		!slices.Equal(previous.verifiedRanges, next.verifiedRanges) ||
		(!phasePreserved && !publishingCommitted) {
		return ErrRecordGeneration
	}
	return nil
}

func validateCheckpointCut(previous, next Record) error {
	if previous.commitState != CommitVerified ||
		next.checkpointGeneration != previous.checkpointGeneration+1 ||
		next.stateGeneration != previous.stateGeneration+1 ||
		next.commitState != CommitCandidate || next.phase != previous.phase {
		// A new range generation is a write-ahead candidate at exactly one
		// successor cut. Phase changes are separate committed state transitions.
		return ErrRecordGeneration
	}
	return nil
}

// ValidLifecycleTransition is the pure v2 phase reducer used by temporary
// runtime projections. It does not grant persistence or filesystem authority.
func ValidLifecycleTransition(
	previousPhase Phase,
	previousCommit CommitState,
	nextPhase Phase,
	nextCommit CommitState,
) bool {
	if nextCommit < previousCommit {
		return false
	}
	switch nextPhase {
	case PhasePublished:
		if nextCommit != CommitPublished {
			return false
		}
	case PhaseQuarantined:
		if nextCommit != CommitQuarantined {
			return false
		}
	default:
		if nextCommit != CommitVerified {
			return false
		}
	}
	switch previousPhase {
	case PhaseReserved:
		return nextPhase == PhaseActive || nextPhase == PhaseRetired ||
			nextPhase == PhaseQuarantined
	case PhaseActive:
		return nextPhase == PhasePaused || nextPhase == PhasePublishing ||
			nextPhase == PhaseRetired || nextPhase == PhaseQuarantined
	case PhasePaused:
		return nextPhase == PhaseActive || nextPhase == PhasePublishing ||
			nextPhase == PhaseRetired || nextPhase == PhaseQuarantined
	case PhasePublishing:
		return nextPhase == PhasePaused || nextPhase == PhasePublished ||
			nextPhase == PhaseRetired || nextPhase == PhaseQuarantined
	case PhasePublished, PhaseRetired:
		// Terminal file results remain authoritative, but later exact-object or
		// final observations may expose item-local cleanup/publication uncertainty.
		// Quarantine records that debt without reopening mutation authority.
		return nextPhase == PhaseQuarantined
	default:
		return false
	}
}

func rangesContain(container, required []Range) bool {
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

// SelectVerified ignores write-ahead candidates. Only a committed image can
// grant have-state after a crash.
func SelectVerified(records ...Record) (Record, error) {
	var selected Record
	for _, candidate := range records {
		if err := candidate.validate(); err != nil {
			return Record{}, err
		}
		if candidate.commitState != CommitVerified && candidate.commitState != CommitPublished {
			continue
		}
		if selected.recordID.IsZero() {
			selected = candidate
			continue
		}
		if !IdentityEqual(selected, candidate) {
			return Record{}, ErrRecordBinding
		}
		if candidate.checkpointGeneration > selected.checkpointGeneration {
			selected = candidate
		} else if candidate.checkpointGeneration == selected.checkpointGeneration &&
			!bytes.Equal(candidate.canonicalPayload(), selected.canonicalPayload()) {
			return Record{}, ErrRecordCrashBoundary
		}
	}
	if selected.recordID.IsZero() {
		return Record{}, ErrRecordRecovery
	}
	return selected, nil
}

// Recover resolves the supported stable-record/candidate crash cut. Ambiguous,
// malformed, foreign, and write-ahead candidates never displace the last
// committed image.
func Recover(committed, candidate *Record) (Record, error) {
	if committed == nil {
		return Record{}, ErrRecordRecovery
	}
	if _, err := SelectVerified(*committed); err != nil {
		return Record{}, err
	}
	if candidate == nil {
		return *committed, nil
	}
	if err := candidate.validate(); err != nil || !IdentityEqual(*committed, *candidate) {
		return *committed, nil
	}
	if candidate.commitState == CommitCandidate {
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

func AdvanceGeneration(
	previous Record,
	ranges []Range,
	phase Phase,
	commitState CommitState,
) (Record, error) {
	if err := previous.validate(); err != nil {
		return Record{}, err
	}
	if previous.stateGeneration == ^uint64(0) ||
		previous.checkpointGeneration == ^uint64(0) {
		return Record{}, ErrRecordGeneration
	}
	next, err := NewRecord(RecordSpec{
		OwnershipMarker: previous.ownershipMarker, Namespace: previous.namespace,
		OperationID: previous.operationID, ReceiveIntentDigest: previous.receiveIntentDigest,
		MaterializationBindingDigest: previous.materializationBindingDigest,
		FileID:                       previous.fileID, FileRevision: previous.fileRevision,
		CanonicalPath: previous.canonicalPath, ExactSize: previous.exactSize,
		MaterializerKind: previous.materializerKind, AuthorityRef: previous.authorityRef.Bytes(),
		OwnedObjectID: previous.ownedObjectID.Bytes(), StateGeneration: previous.stateGeneration + 1,
		CheckpointGeneration: previous.checkpointGeneration + 1, VerifiedRanges: ranges,
		Phase: phase, CommitState: commitState, QuarantineReason: previous.quarantineReason,
		QuarantineOrigin: previous.quarantineOrigin, RetirementReason: previous.retirementReason,
	})
	if err != nil {
		return Record{}, err
	}
	if err := ValidateTransition(previous, next); err != nil {
		return Record{}, err
	}
	return next, nil
}

func AdvanceState(
	previous Record,
	stateGeneration uint64,
	phase Phase,
	commitState CommitState,
	quarantineReason QuarantineReason,
	quarantineOrigin QuarantineOrigin,
	retirementReason RetirementReason,
) (Record, error) {
	if err := previous.validate(); err != nil || stateGeneration <= previous.stateGeneration {
		return Record{}, ErrRecordGeneration
	}
	next, err := NewRecord(RecordSpec{
		OwnershipMarker: previous.ownershipMarker, Namespace: previous.namespace,
		OperationID: previous.operationID, ReceiveIntentDigest: previous.receiveIntentDigest,
		MaterializationBindingDigest: previous.materializationBindingDigest,
		FileID:                       previous.fileID, FileRevision: previous.fileRevision,
		CanonicalPath: previous.canonicalPath, ExactSize: previous.exactSize,
		MaterializerKind: previous.materializerKind, AuthorityRef: previous.authorityRef.Bytes(),
		OwnedObjectID: previous.ownedObjectID.Bytes(), StateGeneration: stateGeneration,
		CheckpointGeneration: previous.checkpointGeneration, VerifiedRanges: previous.verifiedRanges,
		Phase: phase, CommitState: commitState, QuarantineReason: quarantineReason,
		QuarantineOrigin: quarantineOrigin, RetirementReason: retirementReason,
	})
	if err != nil {
		return Record{}, err
	}
	if err := ValidateTransition(previous, next); err != nil {
		return Record{}, err
	}
	return next, nil
}

// Promote performs candidate-to-committed publication without changing the
// generation. The repository must still atomically replace the persisted image.
func Promote(candidate Record, phase Phase, commitState CommitState) (Record, error) {
	if err := candidate.validate(); err != nil {
		return Record{}, err
	}
	if candidate.commitState != CommitCandidate ||
		(commitState != CommitVerified && commitState != CommitPublished) {
		return Record{}, ErrRecordCrashBoundary
	}
	next, err := NewRecord(RecordSpec{
		OwnershipMarker: candidate.ownershipMarker, Namespace: candidate.namespace,
		OperationID: candidate.operationID, ReceiveIntentDigest: candidate.receiveIntentDigest,
		MaterializationBindingDigest: candidate.materializationBindingDigest,
		FileID:                       candidate.fileID, FileRevision: candidate.fileRevision,
		CanonicalPath: candidate.canonicalPath, ExactSize: candidate.exactSize,
		MaterializerKind: candidate.materializerKind, AuthorityRef: candidate.authorityRef.Bytes(),
		OwnedObjectID: candidate.ownedObjectID.Bytes(), StateGeneration: candidate.stateGeneration,
		CheckpointGeneration: candidate.checkpointGeneration, VerifiedRanges: candidate.verifiedRanges,
		Phase: phase, CommitState: commitState, QuarantineReason: candidate.quarantineReason,
		QuarantineOrigin: candidate.quarantineOrigin, RetirementReason: candidate.retirementReason,
	})
	if err != nil {
		return Record{}, err
	}
	if err := ValidateTransition(candidate, next); err != nil {
		return Record{}, err
	}
	return next, nil
}

func InitialCandidate(record Record) bool {
	return record.CommitState() == CommitCandidate && record.Phase() == PhaseActive &&
		record.CheckpointGeneration() == 0 && len(record.VerifiedRanges()) == 0
}

func Committed(record Record) bool {
	switch record.CommitState() {
	case CommitVerified, CommitPublished, CommitQuarantined:
		return true
	default:
		return false
	}
}

func PromoteInitialCandidate(record Record) (Record, error) {
	if !InitialCandidate(record) {
		return Record{}, ErrRecordGeneration
	}
	return Promote(record, PhaseActive, CommitVerified)
}
