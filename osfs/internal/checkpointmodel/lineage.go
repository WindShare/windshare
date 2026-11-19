package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const CheckpointLineageDomain = "windshare/checkpoint-lineage/v1"

type CheckpointLineageID [sha256.Size]byte

func (id CheckpointLineageID) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id CheckpointLineageID) IsZero() bool  { return id == CheckpointLineageID{} }

// CheckpointLineageSpec deliberately omits physical record and progress fields.
// A clean revision change must find the occupied logical slot without granting
// authority over any previous record's object or verified ranges.
type CheckpointLineageSpec struct {
	OperationID                  receivecontract.OperationID
	ReceiveIntentDigest          transfer.ReceiveIntentDigest
	MaterializationBindingDigest receivecontract.BindingDigest
	FileID                       catalog.FileID
	CanonicalPath                string
	MaterializerKind             MaterializerKind
	AuthorityRef                 receivecontract.AuthorityRef
}

func (spec CheckpointLineageSpec) validate() error {
	if spec.OperationID.IsZero() || spec.ReceiveIntentDigest.IsZero() ||
		spec.MaterializationBindingDigest.IsZero() || spec.FileID.IsZero() ||
		!spec.MaterializerKind.Valid() || spec.AuthorityRef.IsZero() {
		return fmt.Errorf("%w: checkpoint lineage identity", ErrRecordBinding)
	}
	if _, err := canonicalCheckpointPath(spec.CanonicalPath); err != nil {
		return fmt.Errorf("%w: checkpoint lineage canonical path", ErrRecordBinding)
	}
	return nil
}

func SameCheckpointLineageSpec(left, right CheckpointLineageSpec) bool {
	if left.validate() != nil || right.validate() != nil {
		return false
	}
	return left.OperationID == right.OperationID &&
		left.ReceiveIntentDigest == right.ReceiveIntentDigest &&
		left.MaterializationBindingDigest == right.MaterializationBindingDigest &&
		left.FileID == right.FileID && left.CanonicalPath == right.CanonicalPath &&
		left.MaterializerKind == right.MaterializerKind && left.AuthorityRef == right.AuthorityRef
}

// CanonicalCheckpointLineageBytes freezes an eight-byte big-endian frame length
// for every field. The path field is an outer frame around its segment-count and
// segment frames, preventing joined-text and segment-boundary ambiguity.
func CanonicalCheckpointLineageBytes(spec CheckpointLineageSpec) ([]byte, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	var encoded bytes.Buffer
	_, _ = encoded.WriteString(CheckpointLineageDomain)
	writeRecordFrame(&encoded, spec.OperationID.Bytes())
	writeRecordFrame(&encoded, spec.ReceiveIntentDigest.Bytes())
	writeRecordFrame(&encoded, spec.MaterializationBindingDigest.Bytes())
	writeRecordFrame(&encoded, spec.FileID.Bytes())
	writeRecordFrame(&encoded, canonicalPathBytes(spec.CanonicalPath))
	writeRecordFrame(&encoded, []byte{byte(spec.MaterializerKind)})
	writeRecordFrame(&encoded, spec.AuthorityRef.Bytes())
	return encoded.Bytes(), nil
}

func DeriveCheckpointLineageID(spec CheckpointLineageSpec) (CheckpointLineageID, error) {
	canonical, err := CanonicalCheckpointLineageBytes(spec)
	if err != nil {
		return CheckpointLineageID{}, err
	}
	return sha256.Sum256(canonical), nil
}

func (record Record) CheckpointLineageSpec() (CheckpointLineageSpec, error) {
	if err := record.validate(); err != nil {
		return CheckpointLineageSpec{}, err
	}
	return CheckpointLineageSpec{
		OperationID:                  record.operationID,
		ReceiveIntentDigest:          record.receiveIntentDigest,
		MaterializationBindingDigest: record.materializationBindingDigest,
		FileID:                       record.fileID,
		CanonicalPath:                record.canonicalPath,
		MaterializerKind:             record.materializerKind,
		AuthorityRef:                 record.authorityRef,
	}, nil
}

func (record Record) CheckpointLineageID() (CheckpointLineageID, error) {
	spec, err := record.CheckpointLineageSpec()
	if err != nil {
		return CheckpointLineageID{}, err
	}
	return DeriveCheckpointLineageID(spec)
}

func (record Record) CheckpointLineageCanonicalBytes() ([]byte, error) {
	spec, err := record.CheckpointLineageSpec()
	if err != nil {
		return nil, err
	}
	return CanonicalCheckpointLineageBytes(spec)
}

// SameCheckpointLineage compares all seven logical coordinates. Callers may use
// the digest as a bounded index, but digest equality alone never grants authority.
func SameCheckpointLineage(left, right Record) bool {
	leftSpec, leftErr := left.CheckpointLineageSpec()
	rightSpec, rightErr := right.CheckpointLineageSpec()
	return leftErr == nil && rightErr == nil && SameCheckpointLineageSpec(leftSpec, rightSpec)
}

type CheckpointLineageDecision uint8

const (
	CheckpointLineageDecisionAbsent CheckpointLineageDecision = iota + 1
	CheckpointLineageDecisionExact
	CheckpointLineageDecisionRevisionConflict
	CheckpointLineageDecisionOwnershipConflict
	CheckpointLineageDecisionInvalid
	CheckpointLineageDecisionMetadataMatch
	CheckpointLineageDecisionReuseStateBlocked
	CheckpointLineageDecisionRevisionEvidenceUnavailable
)

func (decision CheckpointLineageDecision) Valid() bool {
	return decision >= CheckpointLineageDecisionAbsent && decision <= CheckpointLineageDecisionRevisionEvidenceUnavailable
}

func (decision CheckpointLineageDecision) SelectsRecord() bool {
	return decision == CheckpointLineageDecisionExact || decision == CheckpointLineageDecisionMetadataMatch
}

func (decision CheckpointLineageDecision) String() string {
	switch decision {
	case CheckpointLineageDecisionAbsent:
		return "absent"
	case CheckpointLineageDecisionExact:
		return "exact"
	case CheckpointLineageDecisionRevisionConflict:
		return "revision-conflict"
	case CheckpointLineageDecisionOwnershipConflict:
		return "ownership-conflict"
	case CheckpointLineageDecisionInvalid:
		return "invalid"
	case CheckpointLineageDecisionMetadataMatch:
		return "metadata-match"
	case CheckpointLineageDecisionReuseStateBlocked:
		return "reuse-state-blocked"
	case CheckpointLineageDecisionRevisionEvidenceUnavailable:
		return "revision-evidence-unavailable"
	default:
		return ""
	}
}

type CheckpointLineageRequest = content.RevisionSourceEvidence

type CheckpointLineageAssessment struct {
	Decision   CheckpointLineageDecision
	Comparison content.RevisionComparisonEvidence
}

func (assessment CheckpointLineageAssessment) Valid() bool {
	switch assessment.Decision {
	case CheckpointLineageDecisionRevisionConflict:
		return assessment.Comparison.Comparison == content.RevisionComparisonMismatch &&
			(assessment.Comparison.Reason == content.ComparisonSizeChanged || assessment.Comparison.Reason == content.ComparisonMetadataChanged)
	case CheckpointLineageDecisionRevisionEvidenceUnavailable:
		return assessment.Comparison.Comparison == content.RevisionComparisonUnavailable &&
			assessment.Comparison.Reason == content.ComparisonMetadataUnavailable
	default:
		return assessment.Decision.Valid() && assessment.Comparison == (content.RevisionComparisonEvidence{})
	}
}

func (assessment CheckpointLineageAssessment) RetainsSourceEvidence() bool {
	return assessment.Decision == CheckpointLineageDecisionRevisionConflict ||
		assessment.Decision == CheckpointLineageDecisionRevisionEvidenceUnavailable
}

func (record Record) SourceEvidence() content.RevisionSourceEvidence {
	return content.RevisionSourceEvidence{
		FileRevision: record.FileRevision(), ExactSize: record.ExactSize(), SourceMetadata: record.SourceMetadata(),
	}
}

type CheckpointLineageEvidence struct {
	FileRevision   content.FileRevision
	ExactSize      uint64
	OwnedObjectID  ObjectID
	SourceMetadata content.SourceMetadata
	Phase          Phase
	CommitState    CommitState
}

// ClassifyCheckpointLineage shares the admission rule between lookup and atomic
// create. Missing evidence can defer reuse only where state and ownership allow it.
func ClassifyCheckpointLineage(
	request CheckpointLineageRequest,
	evidence []CheckpointLineageEvidence,
	crossLineageOwnershipConflict bool,
) CheckpointLineageAssessment {
	invalid := CheckpointLineageAssessment{Decision: CheckpointLineageDecisionInvalid}
	if request.FileRevision.IsZero() {
		return invalid
	}
	if len(evidence) == 0 {
		return CheckpointLineageAssessment{Decision: CheckpointLineageDecisionAbsent}
	}

	var differentRevision, reuseStateBlocked bool
	var changed, unavailable content.RevisionComparisonEvidence
	ownershipConflict := crossLineageOwnershipConflict
	for _, record := range evidence {
		if record.FileRevision.IsZero() || record.OwnedObjectID.IsZero() {
			return invalid
		}
		ownershipConflict = ownershipConflict || record.OwnedObjectID != evidence[0].OwnedObjectID
		comparison := content.CompareRevisionMetadata(content.RevisionSourceEvidence{
			FileRevision: record.FileRevision, ExactSize: record.ExactSize, SourceMetadata: record.SourceMetadata,
		}, request)
		if record.FileRevision == request.FileRevision {
			if comparison.Comparison == content.RevisionComparisonMismatch {
				return invalid
			}
			continue
		}

		differentRevision = true
		switch comparison.Comparison {
		case content.RevisionComparisonMismatch:
			if changed.Reason == "" {
				changed = comparison
			}
		case content.RevisionComparisonUnavailable:
			unavailable = comparison
		}
		if record.CommitState != CommitVerified || (record.Phase != PhaseActive && record.Phase != PhasePaused) {
			reuseStateBlocked = true
		}
	}
	switch {
	case changed.Reason != "":
		return CheckpointLineageAssessment{CheckpointLineageDecisionRevisionConflict, changed}
	case reuseStateBlocked:
		return CheckpointLineageAssessment{Decision: CheckpointLineageDecisionReuseStateBlocked}
	case ownershipConflict:
		return CheckpointLineageAssessment{Decision: CheckpointLineageDecisionOwnershipConflict}
	case unavailable.Reason != "":
		return CheckpointLineageAssessment{CheckpointLineageDecisionRevisionEvidenceUnavailable, unavailable}
	case differentRevision:
		return CheckpointLineageAssessment{Decision: CheckpointLineageDecisionMetadataMatch}
	default:
		return CheckpointLineageAssessment{Decision: CheckpointLineageDecisionExact}
	}
}
