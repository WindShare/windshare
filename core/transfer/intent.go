package transfer

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	ReceiveIntentDigestBytes = sha256.Size
	TransferJobIdentityBytes = catalog.IdentityBytes
	receiveIntentDomain      = "windshare/receive-intent/v5"
)

var (
	ErrInvalidReceiveIntent       = errors.New("receive intent is invalid")
	ErrInvalidDirectoryAdmission  = errors.New("directory admission is invalid")
	ErrDirectoryAdmissionMismatch = errors.New("directory admission does not match the requested generation")
)

// ReceiveIntentDigest identifies one immutable receiver-local materialization
// authority. Runtime job/session IDs are deliberately excluded.
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

// ReceiveIntent binds selection, artifact semantics, and one validated
// materialization plan. Private fields prevent callers from assembling an
// artifact/binding combination that bypasses the receivecontract constructors.
type ReceiveIntent struct {
	selection SelectionSpec
	artifact  receivecontract.ArtifactSpec
	plan      receivecontract.MaterializationPlan
	digest    ReceiveIntentDigest
	valid     bool
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
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(receiveIntentDomain))
	_, _ = hasher.Write([]byte{0})
	writeHashField(hasher, selection.Digest().Bytes())
	writeHashField(hasher, artifact.Digest().Bytes())
	writeHashField(hasher, plan.BindingDigest().Bytes())
	var sum [ReceiveIntentDigestBytes]byte
	hasher.Sum(sum[:0])
	return ReceiveIntent{
		selection: selection,
		artifact:  artifact,
		plan:      plan,
		digest:    ReceiveIntentDigest(sum),
		valid:     true,
	}, nil
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
func (intent ReceiveIntent) Digest() ReceiveIntentDigest { return intent.digest }
func (intent ReceiveIntent) IsZero() bool                { return !intent.valid }

func (intent ReceiveIntent) Equal(other ReceiveIntent) bool {
	return intent.valid && other.valid && intent.digest == other.digest
}

type receiveIntentJSON struct {
	Selection SelectionSpec                `json:"selection"`
	Artifact  receivecontract.ArtifactSpec `json:"artifact"`
	Plan      json.RawMessage              `json:"plan"`
}

func (intent ReceiveIntent) MarshalJSON() ([]byte, error) {
	if !intent.valid {
		return nil, ErrInvalidReceiveIntent
	}
	planRaw, err := json.Marshal(intent.plan)
	if err != nil {
		return nil, err
	}
	return json.Marshal(receiveIntentJSON{
		Selection: intent.selection,
		Artifact:  intent.artifact,
		Plan:      planRaw,
	})
}

func (intent *ReceiveIntent) UnmarshalJSON(data []byte) error {
	var payload receiveIntentJSON
	if err := decodeStrictJSON(data, &payload); err != nil {
		return ErrInvalidReceiveIntent
	}
	if payload.Selection.IsZero() || payload.Artifact.IsZero() || len(payload.Plan) == 0 {
		return ErrInvalidReceiveIntent
	}
	plan, err := receivecontract.UnmarshalMaterializationPlanJSON(payload.Plan, payload.Artifact)
	if err != nil {
		return ErrInvalidReceiveIntent
	}
	rebuilt, err := NewReceiveIntent(payload.Selection, payload.Artifact, plan)
	if err != nil {
		return err
	}
	*intent = rebuilt
	return nil
}
