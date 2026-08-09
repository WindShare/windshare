package transfer

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"slices"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const ReceiveIntentV1 uint8 = 1

const (
	ReceiveIntentDigestBytes = sha256.Size
	TransferJobIdentityBytes = catalog.IdentityBytes
	receiveIntentDomain      = "windshare/receive-intent/v1"
)

var (
	ErrInvalidReceiveIntent       = errors.New("receive intent is invalid")
	ErrInvalidDirectoryAdmission  = errors.New("directory admission is invalid")
	ErrDirectoryAdmissionMismatch = errors.New("directory admission does not match the requested generation")
)

// ReceiveIntentDigest identifies one immutable receiver-local materialization
// authority. Runtime job/session IDs and any later workspace publication target
// are deliberately excluded by the plan-specific canonical contract.
type ReceiveIntentDigest [ReceiveIntentDigestBytes]byte

func ReceiveIntentDigestFromBytes(raw []byte) (ReceiveIntentDigest, error) {
	if len(raw) != ReceiveIntentDigestBytes {
		return ReceiveIntentDigest{}, ErrInvalidReceiveIntent
	}
	var digest ReceiveIntentDigest
	copy(digest[:], raw)
	if digest.IsZero() {
		return ReceiveIntentDigest{}, ErrInvalidReceiveIntent
	}
	return digest, nil
}

func (digest ReceiveIntentDigest) Bytes() []byte { return append([]byte(nil), digest[:]...) }
func (digest ReceiveIntentDigest) IsZero() bool  { return digest == ReceiveIntentDigest{} }

// TransferJobID is per-run correlation and never substitutes for OperationID.
type TransferJobID [TransferJobIdentityBytes]byte

func NewTransferJobID() (TransferJobID, error) {
	var raw [TransferJobIdentityBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return TransferJobID{}, err
	}
	return TransferJobIDFromBytes(raw[:])
}

func TransferJobIDFromBytes(raw []byte) (TransferJobID, error) {
	if len(raw) != TransferJobIdentityBytes {
		return TransferJobID{}, ErrInvalidReceiveIntent
	}
	var id TransferJobID
	copy(id[:], raw)
	if id.IsZero() {
		return TransferJobID{}, ErrInvalidReceiveIntent
	}
	return id, nil
}

func (id TransferJobID) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id TransferJobID) IsZero() bool  { return id == TransferJobID{} }

// ReceiveIntent binds selection, artifact semantics, and exactly one validated
// materialization plan. Private fields prevent callers from assembling an
// artifact/binding combination that bypasses the receivecontract constructors.
type ReceiveIntent struct {
	selection SelectionSpec
	artifact  receivecontract.ArtifactSpec
	plan      receivecontract.MaterializationPlan
	encoded   []byte
	digest    ReceiveIntentDigest
}

func NewReceiveIntent(
	selection SelectionSpec,
	artifact receivecontract.ArtifactSpec,
	plan receivecontract.MaterializationPlan,
) (ReceiveIntent, error) {
	if selection.IsZero() || artifact.IsZero() || plan.IsZero() ||
		plan.ArtifactDigest() != artifact.Digest() || plan.OperationID().IsZero() {
		return ReceiveIntent{}, ErrInvalidReceiveIntent
	}
	encoded := canonicalReceiveIntentBytes(selection, artifact, plan)
	sum := sha256.Sum256(encoded)
	return ReceiveIntent{
		selection: selection, artifact: artifact, plan: plan,
		encoded: encoded, digest: ReceiveIntentDigest(sum),
	}, nil
}

func canonicalReceiveIntentBytes(
	selection SelectionSpec,
	artifact receivecontract.ArtifactSpec,
	plan receivecontract.MaterializationPlan,
) []byte {
	encoded := make([]byte, 0, len(receiveIntentDomain)+2+
		len(selection.encoded)+len(artifact.CanonicalBytes())+len(plan.CanonicalBytes())+24)
	encoded = append(encoded, receiveIntentDomain...)
	encoded = append(encoded, 0, ReceiveIntentV1)
	encoded = appendCanonicalField(encoded, selection.CanonicalBytes())
	encoded = appendCanonicalField(encoded, artifact.CanonicalBytes())
	encoded = appendCanonicalField(encoded, plan.CanonicalBytes())
	return encoded
}

func (intent ReceiveIntent) SelectionSpec() SelectionSpec { return intent.selection }
func (intent ReceiveIntent) ShareInstance() catalog.ShareInstance {
	return intent.selection.ShareInstance()
}
func (intent ReceiveIntent) SyntheticRoot() catalog.DirectoryID {
	return intent.selection.SyntheticRoot()
}
func (intent ReceiveIntent) SelectionRules() SelectionRules { return intent.selection.SelectionRules() }
func (intent ReceiveIntent) SelectionMode() SelectionMode {
	return intent.selection.SelectionRules().Mode()
}
func (intent ReceiveIntent) ArtifactSpec() receivecontract.ArtifactSpec {
	return intent.artifact
}
func (intent ReceiveIntent) MaterializationPlan() receivecontract.MaterializationPlan {
	return intent.plan
}
func (intent ReceiveIntent) OperationID() receivecontract.OperationID {
	return intent.plan.OperationID()
}
func (intent ReceiveIntent) BindingDigest() receivecontract.BindingDigest {
	return intent.plan.BindingDigest()
}
func (intent ReceiveIntent) CanonicalBytes() []byte      { return slices.Clone(intent.encoded) }
func (intent ReceiveIntent) Bytes() []byte               { return intent.CanonicalBytes() }
func (intent ReceiveIntent) Digest() ReceiveIntentDigest { return intent.digest }
func (intent ReceiveIntent) IsZero() bool                { return !intent.valid() }

func (intent ReceiveIntent) EqualCanonical(other ReceiveIntent) bool {
	return intent.valid() && other.valid() && bytes.Equal(intent.encoded, other.encoded)
}

func (intent ReceiveIntent) valid() bool {
	if intent.selection.IsZero() || intent.artifact.IsZero() || intent.plan.IsZero() ||
		intent.plan.ArtifactDigest() != intent.artifact.Digest() || intent.plan.OperationID().IsZero() {
		return false
	}
	canonical := canonicalReceiveIntentBytes(intent.selection, intent.artifact, intent.plan)
	digest := sha256.Sum256(canonical)
	return bytes.Equal(intent.encoded, canonical) && intent.digest == ReceiveIntentDigest(digest)
}
