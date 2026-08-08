package resumeauthority

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
)

const discardAttentionDomain = "windshare/resume-discard-attention/v1"

// DiscardPlan is a pure policy result. ExpectedStatus becomes an actual
// settlement only after an executor applies every action and honors any
// per-action needs-attention result.
type DiscardPlan struct {
	actions        []Action
	attention      []Attention
	expectedStatus DiscardStatus
}

func (plan DiscardPlan) Actions() []Action             { return slices.Clone(plan.actions) }
func (plan DiscardPlan) Attention() []Attention        { return slices.Clone(plan.attention) }
func (plan DiscardPlan) ExpectedStatus() DiscardStatus { return plan.expectedStatus }

func (plan DiscardPlan) Valid() bool {
	if !plan.expectedStatus.Valid() {
		return false
	}
	for _, action := range plan.actions {
		if !action.Valid() {
			return false
		}
	}
	for _, attention := range plan.attention {
		if !attention.Valid() {
			return false
		}
	}
	switch plan.expectedStatus {
	case Discarded:
		return len(plan.actions) > 0 && len(plan.attention) == 0
	case AlreadyAbsent:
		return len(plan.actions) == 0 && len(plan.attention) == 0
	case DiscardNeedsAttention:
		return len(plan.actions) == 0 && len(plan.attention) > 0
	default:
		return false
	}
}

// ReduceDiscard deterministically authorizes only stage -> sync -> anchor ->
// sync -> record -> sync. It is total over malformed observations: invalid or
// incomplete evidence becomes needs-attention and never becomes an action.
func ReduceDiscard(
	snapshot RepositorySnapshot,
	publications []PublicationObservation,
) DiscardPlan {
	attention := append([]Attention(nil), snapshot.attention...)
	if !snapshot.namespaceEvidence.Valid() {
		attention = append(attention, derivedAttention(AttentionCorruptBinding, nil))
		return attentionPlan(attention)
	}
	if snapshot.namespaceEvidence != EvidenceExact {
		reason := evidenceAttention(snapshot.namespaceEvidence, AttentionMissingOwnership)
		attention = append(attention, derivedAttention(reason, nil))
		return attentionPlan(attention)
	}
	if !snapshot.binding.Valid() {
		attention = append(attention, derivedAttention(AttentionCorruptBinding, nil))
		return attentionPlan(attention)
	}

	checkpoints := slices.Clone(snapshot.checkpoints)
	slices.SortFunc(checkpoints, func(left, right CheckpointObservation) int {
		return bytes.Compare(left.recordID.Bytes(), right.recordID.Bytes())
	})
	publicationByRecord, publicationAttention := indexPublications(publications)
	attention = append(attention, publicationAttention...)
	invalidRecords := duplicateRecordIDs(checkpoints)
	for recordID := range duplicateObjectOwners(snapshot.binding, checkpoints) {
		invalidRecords[recordID] = struct{}{}
	}
	knownRecords := make(map[checkpointmodel.RecordID]struct{}, len(checkpoints))
	for _, checkpoint := range checkpoints {
		knownRecords[checkpoint.recordID] = struct{}{}
	}
	for recordID := range publicationByRecord {
		if _, known := knownRecords[recordID]; !known {
			attention = append(attention, derivedAttention(AttentionCorruptBinding, recordID.Bytes()))
		}
	}

	actions := make([]Action, 0, len(checkpoints)*6)
	processed := make(map[checkpointmodel.RecordID]struct{}, len(checkpoints))
	for _, checkpoint := range checkpoints {
		if _, seen := processed[checkpoint.recordID]; seen {
			continue
		}
		processed[checkpoint.recordID] = struct{}{}
		if _, invalid := invalidRecords[checkpoint.recordID]; invalid {
			attention = append(attention, derivedAttention(
				AttentionCorruptBinding,
				checkpoint.recordID.Bytes(),
			))
			continue
		}
		checkpointActions, checkpointAttention := reduceCheckpoint(
			snapshot.binding,
			checkpoint,
			publicationByRecord[checkpoint.recordID],
		)
		actions = append(actions, checkpointActions...)
		attention = append(attention, checkpointAttention...)
	}

	attention = canonicalAttention(attention)
	switch {
	case len(attention) > 0:
		// Intent-level preflight is atomic. An opaque sibling could claim the
		// same owned object as an otherwise valid record, so no deletion is
		// authorized until the entire selected intent is unambiguous.
		return DiscardPlan{attention: attention, expectedStatus: DiscardNeedsAttention}
	case len(actions) == 0:
		return DiscardPlan{expectedStatus: AlreadyAbsent}
	default:
		return DiscardPlan{actions: actions, expectedStatus: Discarded}
	}
}

func reduceCheckpoint(
	binding checkpointmodel.Binding,
	checkpoint CheckpointObservation,
	publications []PublicationObservation,
) ([]Action, []Attention) {
	recordKey := checkpoint.recordID.Bytes()
	if !checkpoint.validShape() {
		return nil, []Attention{derivedAttention(AttentionCorruptBinding, recordKey)}
	}
	switch checkpoint.recordEvidence {
	case EvidenceAbsent:
		if checkpoint.stageEvidence == EvidenceAbsent && checkpoint.anchorEvidence == EvidenceAbsent {
			return nil, nil
		}
		reason := artifactAttention(checkpoint.stageEvidence, checkpoint.anchorEvidence)
		return nil, []Attention{derivedAttention(reason, recordKey)}
	case EvidenceReplaced:
		return nil, []Attention{derivedAttention(AttentionReplacement, recordKey)}
	case EvidenceAmbiguous:
		return nil, []Attention{derivedAttention(AttentionCorruptBinding, recordKey)}
	case EvidenceExact:
	default:
		return nil, []Attention{derivedAttention(AttentionCorruptBinding, recordKey)}
	}

	record := checkpoint.record
	if !record.Valid() || !checkpointmodel.Committed(record) ||
		!binding.Matches(record, checkpoint.recordID) {
		return nil, []Attention{derivedAttention(AttentionCorruptBinding, recordKey)}
	}
	if reason, unsafe := unsafeArtifactEvidence(checkpoint.stageEvidence, checkpoint.anchorEvidence); unsafe {
		return nil, []Attention{derivedAttention(reason, recordKey)}
	}
	if len(publications) != 1 {
		return nil, []Attention{derivedAttention(AttentionAmbiguousPublication, recordKey)}
	}
	finalEvidence := publications[0].final
	if recordHasPublishedFinal(record) {
		if finalEvidence != EvidenceExact {
			reason := AttentionAmbiguousPublication
			if finalEvidence == EvidenceReplaced {
				reason = AttentionReplacement
			}
			return nil, []Attention{derivedAttention(reason, recordKey)}
		}
	} else if finalEvidence != EvidenceAbsent {
		return nil, []Attention{derivedAttention(AttentionAmbiguousPublication, recordKey)}
	}

	actions := make([]Action, 0, 6)
	if checkpoint.stageEvidence == EvidenceExact {
		actions = append(actions, Action{kind: ActionRemoveStage, record: record})
	}
	actions = append(actions, Action{kind: ActionSyncStages, record: record})
	if checkpoint.anchorEvidence == EvidenceExact {
		actions = append(actions, Action{kind: ActionRemoveAnchor, record: record})
	}
	actions = append(actions,
		Action{kind: ActionSyncAnchors, record: record},
		Action{kind: ActionRemoveRecord, record: record},
		Action{kind: ActionSyncRecords, record: record},
	)
	return actions, nil
}

func recordHasPublishedFinal(record checkpointmodel.Record) bool {
	if record.CommitState() == checkpointmodel.CommitPublished {
		return true
	}
	if record.Phase() == checkpointmodel.PhaseRetired {
		return record.RetirementReason() == checkpointmodel.RetirementPublished
	}
	return record.Phase() == checkpointmodel.PhaseQuarantined &&
		(record.QuarantineOrigin() == checkpointmodel.QuarantineOriginPublished ||
			record.QuarantineOrigin() == checkpointmodel.QuarantineOriginRetiring &&
				record.RetirementReason() == checkpointmodel.RetirementPublished)
}

func unsafeArtifactEvidence(stage, anchor Evidence) (AttentionReason, bool) {
	for _, evidence := range []Evidence{stage, anchor} {
		switch evidence {
		case EvidenceAbsent, EvidenceExact:
		case EvidenceReplaced:
			return AttentionReplacement, true
		case EvidenceAmbiguous:
			return AttentionMissingOwnership, true
		default:
			return AttentionCorruptBinding, true
		}
	}
	return "", false
}

func artifactAttention(evidence ...Evidence) AttentionReason {
	if slices.Contains(evidence, EvidenceReplaced) {
		return AttentionReplacement
	}
	return AttentionMissingOwnership
}

func evidenceAttention(evidence Evidence, fallback AttentionReason) AttentionReason {
	if evidence == EvidenceReplaced {
		return AttentionReplacement
	}
	if evidence == EvidenceAmbiguous || evidence == EvidenceAbsent {
		return fallback
	}
	return AttentionCorruptBinding
}

func indexPublications(
	publications []PublicationObservation,
) (map[checkpointmodel.RecordID][]PublicationObservation, []Attention) {
	indexed := make(map[checkpointmodel.RecordID][]PublicationObservation, len(publications))
	attention := make([]Attention, 0)
	for _, publication := range publications {
		if publication.recordID.IsZero() || !publication.final.Valid() {
			attention = append(attention, derivedAttention(AttentionAmbiguousPublication, nil))
			continue
		}
		indexed[publication.recordID] = append(indexed[publication.recordID], publication)
	}
	for recordID, observations := range indexed {
		if len(observations) != 1 {
			attention = append(attention, derivedAttention(
				AttentionAmbiguousPublication,
				recordID.Bytes(),
			))
		}
	}
	return indexed, attention
}

func duplicateRecordIDs(
	checkpoints []CheckpointObservation,
) map[checkpointmodel.RecordID]struct{} {
	counts := make(map[checkpointmodel.RecordID]uint64, len(checkpoints))
	for _, checkpoint := range checkpoints {
		counts[checkpoint.recordID]++
	}
	duplicates := make(map[checkpointmodel.RecordID]struct{})
	for recordID, count := range counts {
		if count > 1 {
			duplicates[recordID] = struct{}{}
		}
	}
	return duplicates
}

func duplicateObjectOwners(
	binding checkpointmodel.Binding,
	checkpoints []CheckpointObservation,
) map[checkpointmodel.RecordID]struct{} {
	owners := make(map[checkpointmodel.ObjectID][]checkpointmodel.RecordID)
	for _, checkpoint := range checkpoints {
		if checkpoint.recordEvidence != EvidenceExact || !checkpoint.record.Valid() ||
			!checkpointmodel.Committed(checkpoint.record) ||
			!binding.Matches(checkpoint.record, checkpoint.recordID) {
			continue
		}
		objectID := checkpoint.record.OwnedOutputObject()
		owners[objectID] = append(owners[objectID], checkpoint.recordID)
	}
	duplicates := make(map[checkpointmodel.RecordID]struct{})
	for _, recordIDs := range owners {
		if len(recordIDs) < 2 {
			continue
		}
		for _, recordID := range recordIDs {
			duplicates[recordID] = struct{}{}
		}
	}
	return duplicates
}

func attentionPlan(attention []Attention) DiscardPlan {
	return DiscardPlan{
		attention: canonicalAttention(attention), expectedStatus: DiscardNeedsAttention,
	}
}

func canonicalAttention(attention []Attention) []Attention {
	valid := make([]Attention, 0, len(attention))
	for _, current := range attention {
		if current.Valid() {
			valid = append(valid, current)
		} else {
			valid = append(valid, derivedAttention(AttentionCorruptBinding, nil))
		}
	}
	slices.SortFunc(valid, func(left, right Attention) int {
		if left.reason < right.reason {
			return -1
		}
		if left.reason > right.reason {
			return 1
		}
		if left.reference < right.reference {
			return -1
		}
		if left.reference > right.reference {
			return 1
		}
		return 0
	})
	return slices.Compact(valid)
}

func derivedAttention(reason AttentionReason, scope []byte) Attention {
	hash := sha256.New()
	_, _ = hash.Write([]byte(discardAttentionDomain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(reason))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(scope)
	attention, _ := NewAttention(reason, hex.EncodeToString(hash.Sum(nil)))
	return attention
}
