package transfer

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"slices"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const ReceiveIntentV4 uint8 = 4

const (
	ReceiveIntentDigestBytes = sha256.Size
	TransferJobIdentityBytes = catalog.IdentityBytes
	receiveIntentDomain      = "windshare/receive-intent/v4"
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
	encoded = append(encoded, 0, ReceiveIntentV4)
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

// DecodeReceiveIntent is the only persistence decoder for ReceiveIntentV4. It
// reconstructs every nested value through the validated constructors and then
// requires an exact canonical re-encode before returning durable authority.
func DecodeReceiveIntent(encoded []byte) (ReceiveIntent, error) {
	cursor, err := newReceiveIntentDecoder(encoded, receiveIntentDomain, ReceiveIntentV4)
	if err != nil {
		return ReceiveIntent{}, err
	}
	selectionRaw, selectionFrameErr := cursor.frame(cursor.remaining())
	artifactRaw, artifactFrameErr := cursor.frame(cursor.remaining())
	planRaw, planFrameErr := cursor.frame(cursor.remaining())
	selection, selectionErr := decodeSelectionSpec(selectionRaw)
	artifact, artifactErr := receivecontract.DecodeArtifactSpec(artifactRaw)
	plan, planErr := receivecontract.DecodeMaterializationPlan(planRaw, artifact)
	if firstReceiveIntentDecodeError(
		selectionFrameErr, artifactFrameErr, planFrameErr,
		selectionErr, artifactErr, planErr,
	) != nil || !cursor.done() {
		return ReceiveIntent{}, ErrInvalidReceiveIntent
	}
	intent, err := NewReceiveIntent(selection, artifact, plan)
	derivedDigest := ReceiveIntentDigest(sha256.Sum256(encoded))
	if err != nil || !bytes.Equal(intent.CanonicalBytes(), encoded) || intent.Digest() != derivedDigest {
		return ReceiveIntent{}, ErrInvalidReceiveIntent
	}
	return intent, nil
}

func decodeSelectionSpec(encoded []byte) (SelectionSpec, error) {
	cursor, err := newReceiveIntentDecoder(encoded, selectionSpecDomain, SelectionSpecV1)
	if err != nil {
		return SelectionSpec{}, err
	}
	shareRaw, shareFrameErr := cursor.fixedFrame(catalog.IdentityBytes)
	rootRaw, rootFrameErr := cursor.fixedFrame(catalog.IdentityBytes)
	mode, modeErr := cursor.framedByte()
	defaultSelected, defaultErr := cursor.framedBool()
	ruleCount, countErr := cursor.rawUint64()
	share, shareErr := catalog.ShareInstanceFromBytes(shareRaw)
	root, rootErr := catalog.DirectoryIDFromBytes(rootRaw)
	if firstReceiveIntentDecodeError(
		shareFrameErr, rootFrameErr, modeErr, defaultErr, countErr, shareErr, rootErr,
	) != nil {
		return SelectionSpec{}, ErrInvalidReceiveIntent
	}

	rules, err := decodeSelectionRules(&cursor, SelectionMode(mode), defaultSelected, ruleCount)
	if err != nil || !cursor.done() {
		return SelectionSpec{}, ErrInvalidReceiveIntent
	}
	selection, err := NewSelectionSpec(share, root, rules)
	if err != nil || !bytes.Equal(selection.CanonicalBytes(), encoded) {
		return SelectionSpec{}, ErrInvalidReceiveIntent
	}
	return selection, nil
}

func decodeSelectionRules(
	cursor *receiveIntentDecoder,
	mode SelectionMode,
	defaultSelected bool,
	ruleCount uint64,
) (SelectionRules, error) {
	switch mode {
	case SelectionByNodeID:
		return decodeNodeSelectionRules(cursor, defaultSelected, ruleCount)
	case SelectionByCatalogPath:
		return decodePathSelectionRules(cursor, defaultSelected, ruleCount)
	default:
		return SelectionRules{}, ErrInvalidReceiveIntent
	}
}

func decodeNodeSelectionRules(
	cursor *receiveIntentDecoder,
	defaultSelected bool,
	ruleCount uint64,
) (SelectionRules, error) {
	if ruleCount > MaxSelectionRuleOverrides {
		return SelectionRules{}, ErrInvalidReceiveIntent
	}
	overrides := make([]SelectionOverride, 0, int(ruleCount))
	for range ruleCount {
		override, err := decodeSelectionOverride(cursor)
		if err != nil {
			return SelectionRules{}, ErrInvalidReceiveIntent
		}
		overrides = append(overrides, override)
	}
	return NewSelectionRules(defaultSelected, overrides)
}

func decodeSelectionOverride(cursor *receiveIntentDecoder) (SelectionOverride, error) {
	kind, kindErr := cursor.framedByte()
	identityRaw, identityErr := cursor.fixedFrame(catalog.IdentityBytes)
	selected, selectedErr := cursor.framedBool()
	if firstReceiveIntentDecodeError(kindErr, identityErr, selectedErr) != nil {
		return SelectionOverride{}, ErrInvalidReceiveIntent
	}
	switch kind {
	case 1:
		directory, err := catalog.DirectoryIDFromBytes(identityRaw)
		if err != nil {
			return SelectionOverride{}, ErrInvalidReceiveIntent
		}
		return SelectionOverride{DirectoryID: directory, Selected: selected}, nil
	case 2:
		file, err := catalog.FileIDFromBytes(identityRaw)
		if err != nil {
			return SelectionOverride{}, ErrInvalidReceiveIntent
		}
		return SelectionOverride{FileID: file, Selected: selected}, nil
	default:
		return SelectionOverride{}, ErrInvalidReceiveIntent
	}
}

func decodePathSelectionRules(
	cursor *receiveIntentDecoder,
	defaultSelected bool,
	ruleCount uint64,
) (SelectionRules, error) {
	if defaultSelected || ruleCount == 0 || ruleCount > MaxSelectionPathTargets {
		return SelectionRules{}, ErrInvalidReceiveIntent
	}
	paths := make([]string, 0, int(ruleCount))
	totalBytes := uint64(0)
	for range ruleCount {
		pathRaw, err := cursor.frame(catalog.MaxPathBytes)
		if err != nil {
			return SelectionRules{}, ErrInvalidReceiveIntent
		}
		totalBytes += uint64(len(pathRaw))
		if totalBytes > MaxSelectionPathTargetBytes {
			return SelectionRules{}, ErrInvalidReceiveIntent
		}
		paths = append(paths, string(pathRaw))
	}
	return NewPathSelectionRules(paths)
}

type receiveIntentDecoder struct {
	encoded []byte
	offset  int
}

func newReceiveIntentDecoder(encoded []byte, domain string, version uint8) (receiveIntentDecoder, error) {
	prefix := append(append([]byte(nil), domain...), 0, version)
	if !bytes.HasPrefix(encoded, prefix) {
		return receiveIntentDecoder{}, ErrInvalidReceiveIntent
	}
	return receiveIntentDecoder{encoded: encoded, offset: len(prefix)}, nil
}

func (cursor *receiveIntentDecoder) remaining() int { return len(cursor.encoded) - cursor.offset }
func (cursor *receiveIntentDecoder) done() bool     { return cursor.offset == len(cursor.encoded) }

func (cursor *receiveIntentDecoder) rawUint64() (uint64, error) {
	if cursor.remaining() < 8 {
		return 0, ErrInvalidReceiveIntent
	}
	value := binary.BigEndian.Uint64(cursor.encoded[cursor.offset : cursor.offset+8])
	cursor.offset += 8
	return value, nil
}

func (cursor *receiveIntentDecoder) frame(maximum int) ([]byte, error) {
	length, err := cursor.rawUint64()
	if err != nil || maximum < 0 || length > uint64(maximum) || length > uint64(cursor.remaining()) {
		return nil, ErrInvalidReceiveIntent
	}
	value := cursor.encoded[cursor.offset : cursor.offset+int(length)]
	cursor.offset += int(length)
	return value, nil
}

func (cursor *receiveIntentDecoder) fixedFrame(size int) ([]byte, error) {
	value, err := cursor.frame(size)
	if err != nil || len(value) != size {
		return nil, ErrInvalidReceiveIntent
	}
	return value, nil
}

func (cursor *receiveIntentDecoder) framedByte() (byte, error) {
	value, err := cursor.fixedFrame(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (cursor *receiveIntentDecoder) framedBool() (bool, error) {
	value, err := cursor.framedByte()
	if err != nil || value > 1 {
		return false, ErrInvalidReceiveIntent
	}
	return value == 1, nil
}

func firstReceiveIntentDecodeError(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}
