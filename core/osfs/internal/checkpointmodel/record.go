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
)

const (
	SchemaVersion uint8 = 1

	recordDomain         = "windshare/file-checkpoint-record/v1"
	recordChecksumDomain = "windshare/file-checkpoint-checksum/v1"
	recordMagic          = "WSFCPV1\x00"

	maximumBackendBytes = transfer.MaxOutputBackendIDBytes
	maximumPathBytes    = catalog.MaxPathBytes
	maximumRanges       = 16_384

	MaxCheckpointRecordsPerIntent          = 1_048_576
	MaxCheckpointAuxiliaryEntriesPerIntent = MaxCheckpointRecordsPerIntent
	MaxCheckpointShardDirectories          = 256
)

var (
	ErrInvalidRecord        = errors.New("file checkpoint v1 is invalid")
	ErrRecordChecksum       = errors.New("file checkpoint v1 checksum is invalid")
	ErrRecordNonCanonical   = errors.New("file checkpoint v1 encoding is not canonical")
	ErrRecordGeneration     = errors.New("file checkpoint v1 generation is invalid")
	ErrRecordBinding        = errors.New("file checkpoint v1 binding is invalid")
	ErrRecordObjectConflict = errors.New("file checkpoint v1 output object has multiple owners")
	ErrRecordRecovery       = errors.New("file checkpoint v1 has no verified committed record")
	ErrRecordCrashBoundary  = errors.New("file checkpoint v1 crash cut is not recoverable")
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

func RootIdentityFromBytes(raw []byte) (RootIdentity, error) {
	value, err := fixedID(raw, "root identity")
	return RootIdentity(value), err
}

func ObjectIDFromBytes(raw []byte) (ObjectID, error) {
	return fixedID(raw, "owned output object")
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
// values remain frozen because they are part of the V1 payload.
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
	OwnershipMarker      string
	Namespace            string
	TransferIntentDigest transfer.TransferIntentDigest
	FileID               catalog.FileID
	FileRevision         content.FileRevision
	CanonicalPath        string
	ExactSize            uint64
	BackendID            string
	RootIdentity         []byte
	OwnedOutputObject    []byte
	StateGeneration      uint64
	CheckpointGeneration uint64
	VerifiedRanges       []Range
	Phase                Phase
	CommitState          CommitState
	QuarantineReason     QuarantineReason
	QuarantineOrigin     QuarantineOrigin
	RetirementReason     RetirementReason
}

// Record has private storage so an admitted immutable binding or verified range
// set cannot be changed behind a repository's authority.
type Record struct {
	ownershipMarker      string
	namespace            string
	recordID             RecordID
	intentDigest         transfer.TransferIntentDigest
	fileID               catalog.FileID
	fileRevision         content.FileRevision
	canonicalPath        string
	exactSize            uint64
	backendID            transfer.OutputBackendID
	rootIdentity         RootIdentity
	ownedOutputObject    ObjectID
	stateGeneration      uint64
	checkpointGeneration uint64
	verifiedRanges       []Range
	phase                Phase
	commitState          CommitState
	quarantineReason     QuarantineReason
	quarantineOrigin     QuarantineOrigin
	retirementReason     RetirementReason
	checksum             Checksum
}

func NewRecord(spec RecordSpec) (Record, error) {
	if spec.OwnershipMarker == "" {
		spec.OwnershipMarker = OwnershipMarker
	}
	if spec.Namespace == "" {
		spec.Namespace = NamespaceName
	}
	if spec.Phase == 0 {
		spec.Phase = PhaseActive
	}
	if spec.CommitState == 0 {
		spec.CommitState = CommitCandidate
	}
	if err := validateMarkerAndNamespace(spec.OwnershipMarker, spec.Namespace); err != nil {
		return Record{}, err
	}
	backend, err := transfer.NewOutputBackendID(spec.BackendID)
	if err != nil {
		return Record{}, fmt.Errorf("%w: backend: %w", ErrRecordBinding, err)
	}
	root, err := RootIdentityFromBytes(spec.RootIdentity)
	if err != nil {
		return Record{}, err
	}
	object, err := ObjectIDFromBytes(spec.OwnedOutputObject)
	if err != nil {
		return Record{}, err
	}
	canonical, err := catalog.CanonicalPath(spec.CanonicalPath)
	if err != nil || canonical != spec.CanonicalPath {
		return Record{}, fmt.Errorf("%w: canonical path", ErrRecordBinding)
	}
	if spec.TransferIntentDigest.IsZero() || spec.FileID.IsZero() || spec.FileRevision.IsZero() ||
		spec.ExactSize > catalog.MaxFileSize || spec.StateGeneration == 0 {
		return Record{}, fmt.Errorf("%w: immutable identity or generation", ErrRecordBinding)
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
		ownershipMarker:      spec.OwnershipMarker,
		namespace:            spec.Namespace,
		intentDigest:         spec.TransferIntentDigest,
		fileID:               spec.FileID,
		fileRevision:         spec.FileRevision,
		canonicalPath:        canonical,
		exactSize:            spec.ExactSize,
		backendID:            backend,
		rootIdentity:         root,
		ownedOutputObject:    object,
		stateGeneration:      spec.StateGeneration,
		checkpointGeneration: spec.CheckpointGeneration,
		verifiedRanges:       ranges,
		phase:                spec.Phase,
		commitState:          spec.CommitState,
		quarantineReason:     spec.QuarantineReason,
		quarantineOrigin:     spec.QuarantineOrigin,
		retirementReason:     spec.RetirementReason,
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
// the V1 byte meanings without granting repository or filesystem authority.
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

func (record Record) SchemaVersion() uint8                                { return SchemaVersion }
func (record Record) OwnershipMarker() string                             { return record.ownershipMarker }
func (record Record) Namespace() string                                   { return record.namespace }
func (record Record) RecordID() RecordID                                  { return record.recordID }
func (record Record) TransferIntentDigest() transfer.TransferIntentDigest { return record.intentDigest }
func (record Record) FileID() catalog.FileID                              { return record.fileID }
func (record Record) FileRevision() content.FileRevision                  { return record.fileRevision }
func (record Record) CanonicalPath() string                               { return record.canonicalPath }
func (record Record) ExactSize() uint64                                   { return record.exactSize }
func (record Record) BackendID() transfer.OutputBackendID                 { return record.backendID }
func (record Record) RootIdentity() RootIdentity                          { return record.rootIdentity }
func (record Record) OwnedOutputObject() ObjectID                         { return record.ownedOutputObject }
func (record Record) StateGeneration() uint64                             { return record.stateGeneration }
func (record Record) CheckpointGeneration() uint64                        { return record.checkpointGeneration }
func (record Record) VerifiedRanges() []Range                             { return slices.Clone(record.verifiedRanges) }
func (record Record) Phase() Phase                                        { return record.phase }
func (record Record) CommitState() CommitState                            { return record.commitState }
func (record Record) QuarantineReason() QuarantineReason                  { return record.quarantineReason }
func (record Record) QuarantineOrigin() QuarantineOrigin                  { return record.quarantineOrigin }
func (record Record) RetirementReason() RetirementReason                  { return record.retirementReason }
func (record Record) Checksum() Checksum                                  { return record.checksum }

func (record Record) Valid() bool {
	return record.validate() == nil
}

func (record Record) validate() error {
	if err := validateMarkerAndNamespace(record.ownershipMarker, record.namespace); err != nil {
		return err
	}
	if _, err := transfer.NewOutputBackendID(string(record.backendID)); err != nil {
		return fmt.Errorf("%w: backend", ErrRecordBinding)
	}
	if record.intentDigest.IsZero() || record.fileID.IsZero() || record.fileRevision.IsZero() ||
		record.rootIdentity.IsZero() || record.ownedOutputObject.IsZero() || record.recordID.IsZero() ||
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
	writeRecordBytes(hash, []byte(recordDomain))
	writeRecordBytes(hash, []byte{SchemaVersion})
	writeRecordBytes(hash, []byte(record.ownershipMarker))
	writeRecordBytes(hash, []byte(record.namespace))
	writeRecordBytes(hash, record.intentDigest.Bytes())
	writeRecordBytes(hash, record.fileID.Bytes())
	writeRecordBytes(hash, record.fileRevision.Bytes())
	writeRecordBytes(hash, []byte(record.canonicalPath))
	writeRecordU64(hash, record.exactSize)
	writeRecordBytes(hash, []byte(record.backendID))
	writeRecordBytes(hash, record.rootIdentity[:])
	writeRecordBytes(hash, record.ownedOutputObject[:])
	var result RecordID
	copy(result[:], hash.Sum(nil))
	return result
}

func (record Record) canonicalPayload() []byte {
	var encoded bytes.Buffer
	writeRecordString(&encoded, recordDomain)
	encoded.WriteByte(SchemaVersion)
	writeRecordString(&encoded, record.ownershipMarker)
	writeRecordString(&encoded, record.namespace)
	encoded.Write(record.recordID[:])
	encoded.Write(record.intentDigest.Bytes())
	encoded.Write(record.fileID.Bytes())
	encoded.Write(record.fileRevision.Bytes())
	writeRecordString(&encoded, record.canonicalPath)
	writeRecordU64(&encoded, record.exactSize)
	writeRecordString(&encoded, string(record.backendID))
	encoded.Write(record.rootIdentity[:])
	encoded.Write(record.ownedOutputObject[:])
	writeRecordU64(&encoded, record.stateGeneration)
	writeRecordU64(&encoded, record.checkpointGeneration)
	writeRecordU32(&encoded, uint32(len(record.verifiedRanges)))
	for _, current := range record.verifiedRanges {
		writeRecordU64(&encoded, current.Offset)
		writeRecordU64(&encoded, current.End)
	}
	encoded.WriteByte(byte(record.phase))
	encoded.WriteByte(byte(record.commitState))
	encoded.WriteByte(byte(record.quarantineReason))
	encoded.WriteByte(byte(record.quarantineOrigin))
	encoded.WriteByte(byte(record.retirementReason))
	return encoded.Bytes()
}

func (record Record) derivedChecksum() Checksum {
	hash := sha256.New()
	_, _ = hash.Write([]byte(recordChecksumDomain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(record.canonicalPayload())
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
