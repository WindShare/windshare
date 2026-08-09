// Package checkpointmodel owns the transport-neutral durable checkpoint values
// and their pure reducers. Filesystem repositories and runtime adapters may
// project these values, but they cannot reinterpret persisted bytes.
package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	SchemaVersion uint8 = 2

	recordDomain         = "windshare/file-checkpoint-record/v2"
	recordChecksumDomain = "windshare/file-checkpoint-checksum/v2"
	recordMagic          = "WSFCPV2\x00"

	maximumPathBytes = catalog.MaxPathBytes
	maximumRanges    = 16_384

	MaxCheckpointRecordsPerOperation          = 1_048_576
	MaxCheckpointAuxiliaryEntriesPerOperation = MaxCheckpointRecordsPerOperation
	CheckpointShardBuckets                    = 256
)

var (
	ErrInvalidRecord        = errors.New("file checkpoint v2 is invalid")
	ErrRecordChecksum       = errors.New("file checkpoint v2 checksum is invalid")
	ErrRecordNonCanonical   = errors.New("file checkpoint v2 encoding is not canonical")
	ErrRecordGeneration     = errors.New("file checkpoint v2 generation is invalid")
	ErrRecordBinding        = errors.New("file checkpoint v2 binding is invalid")
	ErrRecordObjectConflict = errors.New("file checkpoint v2 output object has multiple owners")
	ErrRecordRecovery       = errors.New("file checkpoint v2 has no verified committed record")
	ErrRecordCrashBoundary  = errors.New("file checkpoint v2 crash cut is not recoverable")
)

type (
	RecordID [sha256.Size]byte
	ObjectID [sha256.Size]byte
	Checksum [sha256.Size]byte
)

func fixedID(raw []byte, name string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	if len(raw) != sha256.Size {
		return result, fmt.Errorf("%w: %s must be %d bytes", ErrRecordBinding, name, sha256.Size)
	}
	copy(result[:], raw)
	if result == ([sha256.Size]byte{}) {
		return result, fmt.Errorf("%w: %s is zero", ErrRecordBinding, name)
	}
	return result, nil
}

func (id RecordID) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id ObjectID) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id Checksum) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id RecordID) IsZero() bool  { return id == RecordID{} }
func (id ObjectID) IsZero() bool  { return id == ObjectID{} }
func (id Checksum) IsZero() bool  { return id == Checksum{} }

func RecordIDFromBytes(raw []byte) (RecordID, error) {
	return fixedID(raw, "record ID")
}

func ObjectIDFromBytes(raw []byte) (ObjectID, error) {
	return fixedID(raw, "owned object")
}

func ChecksumFromBytes(raw []byte) (Checksum, error) {
	return fixedID(raw, "checksum")
}

// Range is [Offset, End). Persisted ranges must already be sorted,
// non-overlapping, and non-adjacent so equality is deterministic.
type Range struct {
	Offset uint64
	End    uint64
}

func (r Range) Length() uint64 {
	if r.End <= r.Offset {
		return 0
	}
	return r.End - r.Offset
}

func validateRanges(ranges []Range, exactSize uint64) error {
	if len(ranges) > maximumRanges {
		return fmt.Errorf("%w: too many verified ranges", ErrInvalidRecord)
	}
	for index, current := range ranges {
		if current.Offset >= current.End || current.End > exactSize {
			return fmt.Errorf("%w: range %d is outside exact size", ErrInvalidRecord, index)
		}
		if index > 0 && current.Offset <= ranges[index-1].End {
			return fmt.Errorf("%w: ranges must be sorted, non-overlapping, and non-adjacent", ErrInvalidRecord)
		}
	}
	return nil
}

// CanonicalizeRanges is a caller-boundary helper. NewRecord still validates and
// never repairs persisted input.
func CanonicalizeRanges(ranges []Range) ([]Range, error) {
	owned := slices.Clone(ranges)
	slices.SortFunc(owned, func(left, right Range) int {
		switch {
		case left.Offset < right.Offset:
			return -1
		case left.Offset > right.Offset:
			return 1
		case left.End < right.End:
			return -1
		case left.End > right.End:
			return 1
		default:
			return 0
		}
	})
	merged := make([]Range, 0, len(owned))
	for _, current := range owned {
		if current.Offset >= current.End {
			return nil, fmt.Errorf("%w: empty range", ErrInvalidRecord)
		}
		if len(merged) == 0 || current.Offset > merged[len(merged)-1].End {
			merged = append(merged, current)
			continue
		}
		if current.End > merged[len(merged)-1].End {
			merged[len(merged)-1].End = current.End
		}
	}
	return merged, nil
}

type Phase uint8

const (
	PhaseReserved Phase = iota + 1
	PhaseActive
	PhasePaused
	PhasePublishing
	PhasePublished
	PhaseQuarantined
	PhaseRetired
)

func (phase Phase) Valid() bool {
	return phase >= PhaseReserved && phase <= PhaseRetired
}

type CommitState uint8

const (
	CommitCandidate CommitState = iota + 1
	CommitVerified
	CommitPublished
	CommitQuarantined
)

func (state CommitState) Valid() bool {
	return state >= CommitCandidate && state <= CommitQuarantined
}

type QuarantineReason uint8

const (
	QuarantineAnchorMissing QuarantineReason = iota + 1
	QuarantineAnchorUnsafe
	QuarantineStageMissing
	QuarantineStageMismatch
	QuarantineStageUnsafe
	QuarantineFinalMismatch
	QuarantineFinalUnsafe
	QuarantinePartialObjectCreation
	QuarantinePublicationHistory
	QuarantineMetadataMismatch
	QuarantineUpdateTemporary
	QuarantineOutputObjectDuplicate
)

func (reason QuarantineReason) Valid() bool {
	return reason >= QuarantineAnchorMissing && reason <= QuarantineOutputObjectDuplicate
}

// QuarantineOrigin records the last non-quarantined runtime phase. Its numeric
// values remain frozen because they are part of the V2 payload.
type QuarantineOrigin uint8

const (
	QuarantineOriginReserved QuarantineOrigin = iota + 1
	QuarantineOriginWitnessed
	QuarantineOriginPublishing
	QuarantineOriginPublishBlocked
	QuarantineOriginPublished
	QuarantineOriginRetiring
)

func (origin QuarantineOrigin) Valid() bool {
	return origin >= QuarantineOriginReserved && origin <= QuarantineOriginRetiring
}

type RetirementReason uint8

const (
	RetirementPublished RetirementReason = iota + 1
	RetirementIsolatedFailure
	RetirementPreObjectCollision
	RetirementInvalidatedRevision
)

func (reason RetirementReason) Valid() bool {
	return reason >= RetirementPublished && reason <= RetirementInvalidatedRevision
}

type RecordSpec struct {
	OwnershipMarker              string
	Namespace                    string
	OperationID                  receivecontract.OperationID
	ReceiveIntentDigest          transfer.ReceiveIntentDigest
	MaterializationBindingDigest receivecontract.BindingDigest
	FileID                       catalog.FileID
	FileRevision                 content.FileRevision
	CanonicalPath                string
	ExactSize                    uint64
	MaterializerKind             MaterializerKind
	AuthorityRef                 []byte
	OwnedObjectID                []byte
	StateGeneration              uint64
	CheckpointGeneration         uint64
	VerifiedRanges               []Range
	Phase                        Phase
	CommitState                  CommitState
	QuarantineReason             QuarantineReason
	QuarantineOrigin             QuarantineOrigin
	RetirementReason             RetirementReason
}

// Record has private storage so an admitted immutable binding or verified range
// set cannot be changed behind a repository's authority.
type Record struct {
	ownershipMarker              string
	namespace                    string
	recordID                     RecordID
	operationID                  receivecontract.OperationID
	receiveIntentDigest          transfer.ReceiveIntentDigest
	materializationBindingDigest receivecontract.BindingDigest
	fileID                       catalog.FileID
	fileRevision                 content.FileRevision
	canonicalPath                string
	exactSize                    uint64
	materializerKind             MaterializerKind
	authorityRef                 receivecontract.AuthorityRef
	ownedObjectID                ObjectID
	stateGeneration              uint64
	checkpointGeneration         uint64
	verifiedRanges               []Range
	phase                        Phase
	commitState                  CommitState
	quarantineReason             QuarantineReason
	quarantineOrigin             QuarantineOrigin
	retirementReason             RetirementReason
	checksum                     Checksum
}

func NewRecord(spec RecordSpec) (Record, error) {
	if spec.OwnershipMarker == "" {
		spec.OwnershipMarker = OwnershipMarker
	}
	if spec.Namespace == "" {
		spec.Namespace = NamespaceName
	}
	if err := validateMarkerAndNamespace(spec.OwnershipMarker, spec.Namespace); err != nil {
		return Record{}, err
	}
	if spec.OperationID.IsZero() || spec.ReceiveIntentDigest.IsZero() ||
		spec.MaterializationBindingDigest.IsZero() || !spec.MaterializerKind.Valid() ||
		spec.FileID.IsZero() || spec.FileRevision.IsZero() || spec.StateGeneration == 0 {
		return Record{}, fmt.Errorf("%w: immutable identity or generation", ErrRecordBinding)
	}
	authority, err := receivecontract.AuthorityRefFromBytes(spec.AuthorityRef)
	if err != nil {
		return Record{}, err
	}
	object, err := ObjectIDFromBytes(spec.OwnedObjectID)
	if err != nil {
		return Record{}, err
	}
	canonical, err := catalog.CanonicalPath(spec.CanonicalPath)
	if err != nil || canonical != spec.CanonicalPath {
		return Record{}, fmt.Errorf("%w: canonical path", ErrRecordBinding)
	}
	if spec.ExactSize > catalog.MaxFileSize {
		return Record{}, fmt.Errorf("%w: exact size", ErrRecordBinding)
	}
	if !spec.Phase.Valid() || !spec.CommitState.Valid() {
		return Record{}, fmt.Errorf("%w: phase or commit state", ErrInvalidRecord)
	}
	if err := validatePhaseCommit(spec.Phase, spec.CommitState); err != nil {
		return Record{}, err
	}
	if err := ValidateLifecycleClaims(
		spec.Phase,
		spec.QuarantineReason,
		spec.QuarantineOrigin,
		spec.RetirementReason,
	); err != nil {
		return Record{}, err
	}
	ranges := slices.Clone(spec.VerifiedRanges)
	if err := validateRanges(ranges, spec.ExactSize); err != nil {
		return Record{}, err
	}
	record := Record{
		ownershipMarker:              spec.OwnershipMarker,
		namespace:                    spec.Namespace,
		operationID:                  spec.OperationID,
		receiveIntentDigest:          spec.ReceiveIntentDigest,
		materializationBindingDigest: spec.MaterializationBindingDigest,
		fileID:                       spec.FileID,
		fileRevision:                 spec.FileRevision,
		canonicalPath:                canonical,
		exactSize:                    spec.ExactSize,
		materializerKind:             spec.MaterializerKind,
		authorityRef:                 authority,
		ownedObjectID:                object,
		stateGeneration:              spec.StateGeneration,
		checkpointGeneration:         spec.CheckpointGeneration,
		verifiedRanges:               ranges,
		phase:                        spec.Phase,
		commitState:                  spec.CommitState,
		quarantineReason:             spec.QuarantineReason,
		quarantineOrigin:             spec.QuarantineOrigin,
		retirementReason:             spec.RetirementReason,
	}
	record.recordID = record.derivedRecordID()
	record.checksum = record.derivedChecksum()
	return record, nil
}

func validateMarkerAndNamespace(marker, namespace string) error {
	if marker != OwnershipMarker || len(marker) > maximumMarkerBytes || !utf8.ValidString(marker) {
		return fmt.Errorf("%w: ownership marker", ErrInvalidRecord)
	}
	if namespace != NamespaceName || len(namespace) > maximumNamespaceBytes || !utf8.ValidString(namespace) {
		return fmt.Errorf("%w: namespace", ErrInvalidRecord)
	}
	return nil
}

func validatePhaseCommit(phase Phase, commit CommitState) error {
	valid := false
	switch commit {
	case CommitCandidate:
		valid = phase == PhaseReserved || phase == PhaseActive ||
			phase == PhasePaused || phase == PhasePublishing
	case CommitVerified:
		valid = phase == PhaseReserved || phase == PhaseActive ||
			phase == PhasePaused || phase == PhasePublishing || phase == PhaseRetired
	case CommitPublished:
		valid = phase == PhasePublished
	case CommitQuarantined:
		valid = phase == PhaseQuarantined
	}
	if !valid {
		return fmt.Errorf("%w: phase and commit state disagree", ErrInvalidRecord)
	}
	return nil
}

// ValidateLifecycleClaims is exported for volatile runtime projections. It owns
// the V2 byte meanings without granting repository or filesystem authority.
func ValidateLifecycleClaims(
	phase Phase,
	quarantineReason QuarantineReason,
	quarantineOrigin QuarantineOrigin,
	retirementReason RetirementReason,
) error {
	if phase == PhaseQuarantined {
		if !quarantineReason.Valid() || !quarantineOrigin.Valid() ||
			!validQuarantineHistory(quarantineOrigin, quarantineReason) ||
			(quarantineOrigin == QuarantineOriginRetiring) != retirementReason.Valid() {
			return fmt.Errorf("%w: quarantined lifecycle claims", ErrInvalidRecord)
		}
		return nil
	}
	if quarantineReason != 0 || quarantineOrigin != 0 {
		return fmt.Errorf("%w: quarantine claims outside quarantined phase", ErrInvalidRecord)
	}
	if phase == PhaseRetired {
		if !retirementReason.Valid() {
			return fmt.Errorf("%w: retired lifecycle claim", ErrInvalidRecord)
		}
		return nil
	}
	if retirementReason != 0 {
		return fmt.Errorf("%w: retirement claim outside retired phase", ErrInvalidRecord)
	}
	return nil
}

func validQuarantineHistory(origin QuarantineOrigin, reason QuarantineReason) bool {
	switch reason {
	case QuarantineAnchorMissing:
		return origin == QuarantineOriginWitnessed || origin == QuarantineOriginPublishing ||
			origin == QuarantineOriginPublishBlocked || origin == QuarantineOriginPublished
	case QuarantineAnchorUnsafe, QuarantineStageUnsafe, QuarantineUpdateTemporary,
		QuarantineOutputObjectDuplicate, QuarantineStageMismatch:
		return origin.Valid()
	case QuarantineStageMissing:
		return origin == QuarantineOriginWitnessed || origin == QuarantineOriginPublishing ||
			origin == QuarantineOriginPublishBlocked
	case QuarantineFinalMismatch:
		return origin == QuarantineOriginPublished
	case QuarantineFinalUnsafe:
		return origin >= QuarantineOriginReserved && origin <= QuarantineOriginPublished
	case QuarantinePartialObjectCreation:
		return origin == QuarantineOriginReserved || origin == QuarantineOriginRetiring
	case QuarantinePublicationHistory:
		return origin == QuarantineOriginReserved || origin == QuarantineOriginWitnessed ||
			origin == QuarantineOriginPublishing || origin == QuarantineOriginPublishBlocked
	case QuarantineMetadataMismatch:
		return origin == QuarantineOriginPublishing || origin == QuarantineOriginPublished
	default:
		return false
	}
}

func (record Record) SchemaVersion() uint8                     { return SchemaVersion }
func (record Record) OwnershipMarker() string                  { return record.ownershipMarker }
func (record Record) Namespace() string                        { return record.namespace }
func (record Record) RecordID() RecordID                       { return record.recordID }
func (record Record) OperationID() receivecontract.OperationID { return record.operationID }
func (record Record) ReceiveIntentDigest() transfer.ReceiveIntentDigest {
	return record.receiveIntentDigest
}
func (record Record) MaterializationBindingDigest() receivecontract.BindingDigest {
	return record.materializationBindingDigest
}
func (record Record) FileID() catalog.FileID                     { return record.fileID }
func (record Record) FileRevision() content.FileRevision         { return record.fileRevision }
func (record Record) CanonicalPath() string                      { return record.canonicalPath }
func (record Record) ExactSize() uint64                          { return record.exactSize }
func (record Record) MaterializerKind() MaterializerKind         { return record.materializerKind }
func (record Record) AuthorityRef() receivecontract.AuthorityRef { return record.authorityRef }
func (record Record) OwnedObjectID() ObjectID                    { return record.ownedObjectID }
func (record Record) StateGeneration() uint64                    { return record.stateGeneration }
func (record Record) CheckpointGeneration() uint64               { return record.checkpointGeneration }
func (record Record) VerifiedRanges() []Range                    { return slices.Clone(record.verifiedRanges) }
func (record Record) Phase() Phase                               { return record.phase }
func (record Record) CommitState() CommitState                   { return record.commitState }
func (record Record) QuarantineReason() QuarantineReason         { return record.quarantineReason }
func (record Record) QuarantineOrigin() QuarantineOrigin         { return record.quarantineOrigin }
func (record Record) RetirementReason() RetirementReason         { return record.retirementReason }
func (record Record) Checksum() Checksum                         { return record.checksum }

func (record Record) Valid() bool {
	return record.validate() == nil
}

func (record Record) validate() error {
	if err := validateMarkerAndNamespace(record.ownershipMarker, record.namespace); err != nil {
		return err
	}
	if record.operationID.IsZero() || record.receiveIntentDigest.IsZero() ||
		record.materializationBindingDigest.IsZero() || !record.materializerKind.Valid() ||
		record.fileID.IsZero() || record.fileRevision.IsZero() ||
		record.authorityRef.IsZero() || record.ownedObjectID.IsZero() || record.recordID.IsZero() ||
		record.exactSize > catalog.MaxFileSize || record.stateGeneration == 0 {
		return fmt.Errorf("%w: identity or generation", ErrRecordBinding)
	}
	canonical, err := catalog.CanonicalPath(record.canonicalPath)
	if err != nil || canonical != record.canonicalPath {
		return fmt.Errorf("%w: canonical path", ErrRecordBinding)
	}
	if !record.phase.Valid() || !record.commitState.Valid() {
		return fmt.Errorf("%w: phase or commit state", ErrInvalidRecord)
	}
	if err := validatePhaseCommit(record.phase, record.commitState); err != nil {
		return err
	}
	if err := ValidateLifecycleClaims(
		record.phase,
		record.quarantineReason,
		record.quarantineOrigin,
		record.retirementReason,
	); err != nil {
		return err
	}
	if err := validateRanges(record.verifiedRanges, record.exactSize); err != nil {
		return err
	}
	if record.recordID != record.derivedRecordID() {
		return fmt.Errorf("%w: record ID does not match immutable binding", ErrRecordBinding)
	}
	if !record.checksum.IsZero() && record.checksum != record.derivedChecksum() {
		return ErrRecordChecksum
	}
	return nil
}

func (record Record) derivedRecordID() RecordID {
	hash := sha256.New()
	writeRecordPrefix(hash)
	writeRecordFrame(hash, []byte(record.ownershipMarker))
	writeRecordFrame(hash, []byte(record.namespace))
	writeRecordFrame(hash, record.operationID.Bytes())
	writeRecordFrame(hash, record.receiveIntentDigest.Bytes())
	writeRecordFrame(hash, record.materializationBindingDigest.Bytes())
	writeRecordFrame(hash, record.fileID.Bytes())
	writeRecordFrame(hash, record.fileRevision.Bytes())
	writeRecordFrame(hash, canonicalPathBytes(record.canonicalPath))
	writeRecordFramedU64(hash, record.exactSize)
	writeRecordFrame(hash, []byte{byte(record.materializerKind)})
	writeRecordFrame(hash, record.authorityRef.Bytes())
	writeRecordFrame(hash, record.ownedObjectID[:])
	var result RecordID
	copy(result[:], hash.Sum(nil))
	return result
}

func (record Record) canonicalPayload() []byte {
	var encoded bytes.Buffer
	writeRecordPrefix(&encoded)
	writeRecordFrame(&encoded, []byte(record.ownershipMarker))
	writeRecordFrame(&encoded, []byte(record.namespace))
	writeRecordFrame(&encoded, record.recordID[:])
	writeRecordFrame(&encoded, record.operationID.Bytes())
	writeRecordFrame(&encoded, record.receiveIntentDigest.Bytes())
	writeRecordFrame(&encoded, record.materializationBindingDigest.Bytes())
	writeRecordFrame(&encoded, record.fileID.Bytes())
	writeRecordFrame(&encoded, record.fileRevision.Bytes())
	writeRecordFrame(&encoded, canonicalPathBytes(record.canonicalPath))
	writeRecordFramedU64(&encoded, record.exactSize)
	writeRecordFrame(&encoded, []byte{byte(record.materializerKind)})
	writeRecordFrame(&encoded, record.authorityRef.Bytes())
	writeRecordFrame(&encoded, record.ownedObjectID[:])
	writeRecordFramedU64(&encoded, record.stateGeneration)
	writeRecordFramedU64(&encoded, record.checkpointGeneration)
	writeRecordU64(&encoded, uint64(len(record.verifiedRanges)))
	for _, current := range record.verifiedRanges {
		writeRecordFramedU64(&encoded, current.Offset)
		writeRecordFramedU64(&encoded, current.End)
	}
	writeRecordFrame(&encoded, []byte{byte(record.phase)})
	writeRecordFrame(&encoded, []byte{byte(record.commitState)})
	writeRecordFrame(&encoded, []byte{byte(record.quarantineReason)})
	writeRecordFrame(&encoded, []byte{byte(record.quarantineOrigin)})
	writeRecordFrame(&encoded, []byte{byte(record.retirementReason)})
	return encoded.Bytes()
}

func (record Record) derivedChecksum() Checksum {
	hash := sha256.New()
	_, _ = hash.Write([]byte(recordChecksumDomain))
	_, _ = hash.Write([]byte{0, SchemaVersion})
	writeRecordFrame(hash, record.canonicalPayload())
	var result Checksum
	copy(result[:], hash.Sum(nil))
	return result
}

// CanonicalBytes excludes the storage envelope checksum.
func (record Record) CanonicalBytes() []byte {
	if record.validate() != nil {
		return nil
	}
	return record.canonicalPayload()
}

func EncodeRecord(record Record) ([]byte, error) {
	if err := record.validate(); err != nil {
		return nil, err
	}
	payload := record.canonicalPayload()
	checksum := record.derivedChecksum()
	encoded := make([]byte, 0, len(recordMagic)+4+len(payload)+len(checksum))
	encoded = append(encoded, recordMagic...)
	var length [4]byte
	length[0] = byte(uint32(len(payload)) >> 24)
	length[1] = byte(uint32(len(payload)) >> 16)
	length[2] = byte(uint32(len(payload)) >> 8)
	length[3] = byte(uint32(len(payload)))
	encoded = append(encoded, length[:]...)
	encoded = append(encoded, payload...)
	encoded = append(encoded, checksum[:]...)
	return encoded, nil
}
