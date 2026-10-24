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
	if _, err := canonicalCheckpointPath(spec.CanonicalPath, spec.MaterializerKind); err != nil {
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
)

func (decision CheckpointLineageDecision) Valid() bool {
	return decision >= CheckpointLineageDecisionAbsent && decision <= CheckpointLineageDecisionInvalid
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
	default:
		return ""
	}
}

type CheckpointLineageRequest struct {
	FileRevision content.FileRevision
	ExactSize    uint64
}

type CheckpointLineageEvidence struct {
	FileRevision  content.FileRevision
	ExactSize     uint64
	OwnedObjectID ObjectID
}

// ClassifyCheckpointLineage is the single mixed-conflict precedence for startup
// indexing and atomic create. Invalid same-revision binding wins before revision
// conflict, which wins before object ownership ambiguity.
func ClassifyCheckpointLineage(
	request CheckpointLineageRequest,
	evidence []CheckpointLineageEvidence,
	crossLineageOwnershipConflict bool,
) CheckpointLineageDecision {
	if request.FileRevision.IsZero() {
		return CheckpointLineageDecisionInvalid
	}
	if len(evidence) == 0 {
		return CheckpointLineageDecisionAbsent
	}

	for _, record := range evidence {
		if record.FileRevision.IsZero() || record.OwnedObjectID.IsZero() {
			return CheckpointLineageDecisionInvalid
		}
		if record.FileRevision == request.FileRevision && record.ExactSize != request.ExactSize {
			return CheckpointLineageDecisionInvalid
		}
	}
	for _, record := range evidence {
		if record.FileRevision != request.FileRevision {
			return CheckpointLineageDecisionRevisionConflict
		}
	}

	firstObject := evidence[0].OwnedObjectID
	if crossLineageOwnershipConflict {
		return CheckpointLineageDecisionOwnershipConflict
	}
	for _, record := range evidence[1:] {
		if record.OwnedObjectID != firstObject {
			return CheckpointLineageDecisionOwnershipConflict
		}
	}
	return CheckpointLineageDecisionExact
}
