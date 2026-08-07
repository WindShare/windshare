package resumestate

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

// FileCheckpointV1 is the file-local durable recovery contract.  It is kept in
// resumestate rather than in an output backend so native, FSA, and OPFS adapters
// cannot silently assign different meanings to a durable range.  The record ID
// covers only immutable binding fields; generation, ranges, and phase may advance
// while the same file remains resumable.
const (
	FileCheckpointV1SchemaVersion uint8 = 1
	FileCheckpointSchemaVersion         = FileCheckpointV1SchemaVersion
	FileCheckpointV1Version             = FileCheckpointV1SchemaVersion

	FileCheckpointOwnershipMarker = "windshare/file-checkpoint/v1"
	FileCheckpointNamespace       = ".windshare-output/checkpoints-v1"
	fileCheckpointDomain          = "windshare/file-checkpoint-record/v1"
	fileCheckpointChecksumDomain  = "windshare/file-checkpoint-checksum/v1"
	fileCheckpointMagic           = "WSFCPV1\x00"

	maxCheckpointMarkerBytes  = 128
	maxCheckpointNamespace    = 256
	maxCheckpointBackendBytes = transfer.MaxOutputBackendIDBytes
	maxCheckpointPathBytes    = catalog.MaxPathBytes
	maxCheckpointRanges       = 16_384
)

var (
	ErrInvalidFileCheckpoint        = errors.New("file checkpoint v1 is invalid")
	ErrFileCheckpointChecksum       = errors.New("file checkpoint v1 checksum is invalid")
	ErrFileCheckpointNonCanonical   = errors.New("file checkpoint v1 encoding is not canonical")
	ErrFileCheckpointGeneration     = errors.New("file checkpoint v1 generation is invalid")
	ErrFileCheckpointBinding        = errors.New("file checkpoint v1 binding is invalid")
	ErrFileCheckpointObjectConflict = errors.New("file checkpoint v1 output object has multiple owners")
	ErrFileCheckpointRecovery       = errors.New("file checkpoint v1 has no verified committed record")
	ErrFileCheckpointOwnership      = errors.New("file checkpoint v1 ownership marker is invalid")
	ErrFileCheckpointCrashBoundary  = errors.New("file checkpoint v1 crash cut is not recoverable")
)

type (
	// TransferIntentDigest is aliased here so checkpoint consumers can depend on
	// the durable contract without importing the whole TransferIntent aggregate.
	TransferIntentDigest     = transfer.TransferIntentDigest
	FileCheckpointRecordID   [sha256.Size]byte
	FileCheckpointRootID     [sha256.Size]byte
	FileCheckpointObjectID   [sha256.Size]byte
	FileCheckpointChecksum   [sha256.Size]byte
	CheckpointRecordID       = FileCheckpointRecordID
	CheckpointRootIdentity   = FileCheckpointRootID
	CheckpointObjectIdentity = FileCheckpointObjectID
)

const TransferIntentDigestBytes = transfer.TransferIntentDigestBytes

func NewTransferIntentDigest(raw []byte) (TransferIntentDigest, error) {
	return transfer.TransferIntentDigestFromBytes(raw)
}

func fixedCheckpointID(raw []byte, name string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	if len(raw) != sha256.Size {
		return result, fmt.Errorf("%w: %s must be %d bytes", ErrFileCheckpointBinding, name, sha256.Size)
	}
	copy(result[:], raw)
	if result == ([sha256.Size]byte{}) {
		return result, fmt.Errorf("%w: %s is zero", ErrFileCheckpointBinding, name)
	}
	return result, nil
}

func (id FileCheckpointRecordID) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id FileCheckpointRootID) Bytes() []byte   { return append([]byte(nil), id[:]...) }
func (id FileCheckpointObjectID) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id FileCheckpointChecksum) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id FileCheckpointRecordID) IsZero() bool  { return id == FileCheckpointRecordID{} }
func (id FileCheckpointRootID) IsZero() bool    { return id == FileCheckpointRootID{} }
func (id FileCheckpointObjectID) IsZero() bool  { return id == FileCheckpointObjectID{} }
func (id FileCheckpointChecksum) IsZero() bool  { return id == FileCheckpointChecksum{} }

func FileCheckpointRecordIDFromBytes(raw []byte) (FileCheckpointRecordID, error) {
	return fixedCheckpointID(raw, "record ID")
}
func FileCheckpointRootIDFromBytes(raw []byte) (FileCheckpointRootID, error) {
	return fixedCheckpointID(raw, "root identity")
}
func FileCheckpointObjectIDFromBytes(raw []byte) (FileCheckpointObjectID, error) {
	return fixedCheckpointID(raw, "owned output object")
}
func FileCheckpointChecksumFromBytes(raw []byte) (FileCheckpointChecksum, error) {
	return fixedCheckpointID(raw, "checksum")
}

// FileCheckpointRange is the only range representation accepted by the V1
// codec.  Ranges are [Offset, End), sorted by Offset, non-overlapping, and
// non-adjacent; this makes equality and replay deterministic across backends.
type FileCheckpointRange struct {
	Offset uint64
	End    uint64
}

type CheckpointRange = FileCheckpointRange

func (r FileCheckpointRange) Length() uint64 {
	if r.End <= r.Offset {
		return 0
	}
	return r.End - r.Offset
}

func validateCheckpointRanges(ranges []FileCheckpointRange, exactSize uint64) error {
	if len(ranges) > maxCheckpointRanges {
		return fmt.Errorf("%w: too many verified ranges", ErrInvalidFileCheckpoint)
	}
	for index, current := range ranges {
		if current.Offset >= current.End || current.End > exactSize {
			return fmt.Errorf("%w: range %d is outside exact size", ErrInvalidFileCheckpoint, index)
		}
		if index > 0 && current.Offset <= ranges[index-1].End {
			return fmt.Errorf("%w: ranges must be sorted, non-overlapping, and non-adjacent", ErrInvalidFileCheckpoint)
		}
	}
	return nil
}

// CanonicalizeFileCheckpointRanges is an explicit boundary helper for callers
// collecting ranges from workers.  The record constructor still validates the
// result and never silently repairs a persisted record.
func CanonicalizeFileCheckpointRanges(ranges []FileCheckpointRange) ([]FileCheckpointRange, error) {
	owned := slices.Clone(ranges)
	slices.SortFunc(owned, func(left, right FileCheckpointRange) int {
		if left.Offset < right.Offset {
			return -1
		}
		if left.Offset > right.Offset {
			return 1
		}
		if left.End < right.End {
			return -1
		}
		if left.End > right.End {
			return 1
		}
		return 0
	})
	merged := make([]FileCheckpointRange, 0, len(owned))
	for _, current := range owned {
		if current.Offset >= current.End {
			return nil, fmt.Errorf("%w: empty range", ErrInvalidFileCheckpoint)
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

type FileCheckpointPhase uint8

const (
	FileCheckpointPhaseReserved FileCheckpointPhase = iota + 1
	FileCheckpointPhaseActive
	FileCheckpointPhasePaused
	FileCheckpointPhasePublishing
	FileCheckpointPhasePublished
	FileCheckpointPhaseQuarantined
	FileCheckpointPhaseRetired
)

func (phase FileCheckpointPhase) Valid() bool {
	return phase >= FileCheckpointPhaseReserved && phase <= FileCheckpointPhaseRetired
}

type FileCheckpointCommitState uint8

const (
	FileCheckpointCommitCandidate FileCheckpointCommitState = iota + 1
	FileCheckpointCommitVerified
	FileCheckpointCommitPublished
	FileCheckpointCommitQuarantined
)

func (state FileCheckpointCommitState) Valid() bool {
	return state >= FileCheckpointCommitCandidate && state <= FileCheckpointCommitQuarantined
}

// Short aliases keep call sites readable without introducing a second state
// machine.  They are constants, not flags: exactly one phase and commit state is
// persisted in every record.
const (
	CheckpointPhaseReserved    = FileCheckpointPhaseReserved
	CheckpointPhaseActive      = FileCheckpointPhaseActive
	CheckpointPhasePaused      = FileCheckpointPhasePaused
	CheckpointPhasePublishing  = FileCheckpointPhasePublishing
	CheckpointPhasePublished   = FileCheckpointPhasePublished
	CheckpointPhaseQuarantined = FileCheckpointPhaseQuarantined
	CheckpointPhaseRetired     = FileCheckpointPhaseRetired

	CheckpointCommitCandidate   = FileCheckpointCommitCandidate
	CheckpointCommitVerified    = FileCheckpointCommitVerified
	CheckpointCommitPublished   = FileCheckpointCommitPublished
	CheckpointCommitQuarantined = FileCheckpointCommitQuarantined
)

// FileCheckpointSpec is the consumer-side input.  Root/object identities are
// byte slices at this boundary because each backend has a different native proof;
// NewFileCheckpointV1 copies and validates them before creating immutable state.
type FileCheckpointSpec struct {
	OwnershipMarker       string
	Namespace             string
	TransferIntentDigest  transfer.TransferIntentDigest
	FileID                catalog.FileID
	FileRevision          content.FileRevision
	CanonicalPath         string
	ExactSize             uint64
	BackendID             string
	RootIdentity          []byte
	OwnedOutputObject     []byte
	StateGeneration       uint64
	CheckpointGeneration  uint64
	VerifiedRanges        []FileCheckpointRange
	Phase                 FileCheckpointPhase
	CommitState           FileCheckpointCommitState
	QuarantineReason      QuarantineReason
	PhaseBeforeQuarantine FilePhase
	RetirementReason      RetirementReason
}

// FileCheckpointV1 has private storage so a caller cannot mutate an already
// validated identity or range set after it is admitted to a transaction.
type FileCheckpointV1 struct {
	ownershipMarker       string
	namespace             string
	recordID              FileCheckpointRecordID
	intentDigest          transfer.TransferIntentDigest
	fileID                catalog.FileID
	fileRevision          content.FileRevision
	canonicalPath         string
	exactSize             uint64
	backendID             transfer.OutputBackendID
	rootIdentity          FileCheckpointRootID
	ownedOutputObject     FileCheckpointObjectID
	stateGeneration       uint64
	checkpointGeneration  uint64
	verifiedRanges        []FileCheckpointRange
	phase                 FileCheckpointPhase
	commitState           FileCheckpointCommitState
	quarantineReason      QuarantineReason
	phaseBeforeQuarantine FilePhase
	retirementReason      RetirementReason
	checksum              FileCheckpointChecksum
}

// FileCheckpointRecord is a semantic alias used by storage adapters that call
// the persisted value a record rather than a checkpoint.
type FileCheckpointRecord = FileCheckpointV1
type FileCheckpointRecordSpec = FileCheckpointSpec

func NewFileCheckpointV1(spec FileCheckpointSpec) (FileCheckpointV1, error) {
	if spec.OwnershipMarker == "" {
		spec.OwnershipMarker = FileCheckpointOwnershipMarker
	}
	if spec.Namespace == "" {
		spec.Namespace = FileCheckpointNamespace
	}
	if spec.Phase == 0 {
		spec.Phase = FileCheckpointPhaseActive
	}
	if spec.CommitState == 0 {
		spec.CommitState = FileCheckpointCommitCandidate
	}
	if err := validateMarkerAndNamespace(spec.OwnershipMarker, spec.Namespace); err != nil {
		return FileCheckpointV1{}, err
	}
	backend, err := transfer.NewOutputBackendID(spec.BackendID)
	if err != nil {
		return FileCheckpointV1{}, fmt.Errorf("%w: backend: %w", ErrFileCheckpointBinding, err)
	}
	root, err := FileCheckpointRootIDFromBytes(spec.RootIdentity)
	if err != nil {
		return FileCheckpointV1{}, err
	}
	object, err := FileCheckpointObjectIDFromBytes(spec.OwnedOutputObject)
	if err != nil {
		return FileCheckpointV1{}, err
	}
	canonical, err := catalog.CanonicalPath(spec.CanonicalPath)
	if err != nil || canonical != spec.CanonicalPath {
		return FileCheckpointV1{}, fmt.Errorf("%w: canonical path", ErrFileCheckpointBinding)
	}
	if spec.TransferIntentDigest.IsZero() || spec.FileID.IsZero() || spec.FileRevision.IsZero() ||
		spec.ExactSize > catalog.MaxFileSize || spec.StateGeneration == 0 {
		return FileCheckpointV1{}, fmt.Errorf("%w: immutable identity or generation", ErrFileCheckpointBinding)
	}
	if !spec.Phase.Valid() || !spec.CommitState.Valid() {
		return FileCheckpointV1{}, fmt.Errorf("%w: phase or commit state", ErrInvalidFileCheckpoint)
	}
	if err := validateCheckpointLifecycleClaims(
		spec.Phase, spec.QuarantineReason, spec.PhaseBeforeQuarantine, spec.RetirementReason,
	); err != nil {
		return FileCheckpointV1{}, err
	}
	ranges := slices.Clone(spec.VerifiedRanges)
	if err := validateCheckpointRanges(ranges, spec.ExactSize); err != nil {
		return FileCheckpointV1{}, err
	}
	record := FileCheckpointV1{
		ownershipMarker: spec.OwnershipMarker, namespace: spec.Namespace,
		intentDigest: spec.TransferIntentDigest, fileID: spec.FileID, fileRevision: spec.FileRevision,
		canonicalPath: canonical, exactSize: spec.ExactSize, backendID: backend,
		rootIdentity: root, ownedOutputObject: object,
		stateGeneration: spec.StateGeneration, checkpointGeneration: spec.CheckpointGeneration,
		verifiedRanges: ranges, phase: spec.Phase, commitState: spec.CommitState,
		quarantineReason: spec.QuarantineReason, phaseBeforeQuarantine: spec.PhaseBeforeQuarantine,
		retirementReason: spec.RetirementReason,
	}
	record.recordID = record.derivedRecordID()
	record.checksum = record.derivedChecksum()
	return record, nil
}

func NewFileCheckpointRecord(spec FileCheckpointRecordSpec) (FileCheckpointRecord, error) {
	return NewFileCheckpointV1(spec)
}

func validateMarkerAndNamespace(marker, namespace string) error {
	if marker != FileCheckpointOwnershipMarker || len(marker) > maxCheckpointMarkerBytes || !utf8.ValidString(marker) {
		return fmt.Errorf("%w: ownership marker", ErrFileCheckpointOwnership)
	}
	if namespace != FileCheckpointNamespace || len(namespace) > maxCheckpointNamespace || !utf8.ValidString(namespace) {
		return fmt.Errorf("%w: namespace", ErrFileCheckpointOwnership)
	}
	return nil
}

func validateCheckpointLifecycleClaims(
	phase FileCheckpointPhase,
	quarantineReason QuarantineReason,
	phaseBeforeQuarantine FilePhase,
	retirementReason RetirementReason,
) error {
	if phase == FileCheckpointPhaseQuarantined {
		if !quarantineReason.Valid() || !phaseBeforeQuarantine.Valid() ||
			phaseBeforeQuarantine == FileQuarantined ||
			!validQuarantineHistory(phaseBeforeQuarantine, quarantineReason) ||
			(phaseBeforeQuarantine == FileRetiring) != retirementReason.Valid() {
			return fmt.Errorf("%w: quarantined lifecycle claims", ErrInvalidFileCheckpoint)
		}
		return nil
	}
	if quarantineReason != 0 || phaseBeforeQuarantine != 0 {
		return fmt.Errorf("%w: quarantine claims outside quarantined phase", ErrInvalidFileCheckpoint)
	}
	if phase == FileCheckpointPhaseRetired {
		if !retirementReason.Valid() {
			return fmt.Errorf("%w: retired lifecycle claim", ErrInvalidFileCheckpoint)
		}
		return nil
	}
	if retirementReason != 0 {
		return fmt.Errorf("%w: retirement claim outside retired phase", ErrInvalidFileCheckpoint)
	}
	return nil
}

func (checkpoint FileCheckpointV1) SchemaVersion() uint8    { return FileCheckpointV1SchemaVersion }
func (checkpoint FileCheckpointV1) OwnershipMarker() string { return checkpoint.ownershipMarker }
func (checkpoint FileCheckpointV1) Namespace() string       { return checkpoint.namespace }
func (checkpoint FileCheckpointV1) RecordID() FileCheckpointRecordID {
	return checkpoint.recordID
}
func (checkpoint FileCheckpointV1) TransferIntentDigest() transfer.TransferIntentDigest {
	return checkpoint.intentDigest
}
func (checkpoint FileCheckpointV1) FileID() catalog.FileID { return checkpoint.fileID }
func (checkpoint FileCheckpointV1) FileRevision() content.FileRevision {
	return checkpoint.fileRevision
}
func (checkpoint FileCheckpointV1) CanonicalPath() string               { return checkpoint.canonicalPath }
func (checkpoint FileCheckpointV1) ExactSize() uint64                   { return checkpoint.exactSize }
func (checkpoint FileCheckpointV1) BackendID() transfer.OutputBackendID { return checkpoint.backendID }
func (checkpoint FileCheckpointV1) RootIdentity() FileCheckpointRootID {
	return checkpoint.rootIdentity
}
func (checkpoint FileCheckpointV1) OwnedOutputObject() FileCheckpointObjectID {
	return checkpoint.ownedOutputObject
}
func (checkpoint FileCheckpointV1) OutputObject() FileCheckpointObjectID {
	return checkpoint.ownedOutputObject
}
func (checkpoint FileCheckpointV1) StateGeneration() uint64 { return checkpoint.stateGeneration }
func (checkpoint FileCheckpointV1) CheckpointGeneration() uint64 {
	return checkpoint.checkpointGeneration
}
func (checkpoint FileCheckpointV1) VerifiedRanges() []FileCheckpointRange {
	return slices.Clone(checkpoint.verifiedRanges)
}
func (checkpoint FileCheckpointV1) Ranges() []FileCheckpointRange { return checkpoint.VerifiedRanges() }
func (checkpoint FileCheckpointV1) Phase() FileCheckpointPhase    { return checkpoint.phase }
func (checkpoint FileCheckpointV1) CommitState() FileCheckpointCommitState {
	return checkpoint.commitState
}
func (checkpoint FileCheckpointV1) QuarantineReason() QuarantineReason {
	return checkpoint.quarantineReason
}
func (checkpoint FileCheckpointV1) PhaseBeforeQuarantine() FilePhase {
	return checkpoint.phaseBeforeQuarantine
}
func (checkpoint FileCheckpointV1) RetirementReason() RetirementReason {
	return checkpoint.retirementReason
}
func (checkpoint FileCheckpointV1) Checksum() FileCheckpointChecksum { return checkpoint.checksum }

func (checkpoint FileCheckpointV1) valid() error {
	if err := validateMarkerAndNamespace(checkpoint.ownershipMarker, checkpoint.namespace); err != nil {
		return err
	}
	if _, err := transfer.NewOutputBackendID(string(checkpoint.backendID)); err != nil {
		return fmt.Errorf("%w: backend", ErrFileCheckpointBinding)
	}
	if checkpoint.intentDigest.IsZero() || checkpoint.fileID.IsZero() || checkpoint.fileRevision.IsZero() ||
		checkpoint.rootIdentity.IsZero() || checkpoint.ownedOutputObject.IsZero() || checkpoint.recordID.IsZero() ||
		checkpoint.exactSize > catalog.MaxFileSize || checkpoint.stateGeneration == 0 {
		return fmt.Errorf("%w: identity or generation", ErrFileCheckpointBinding)
	}
	canonical, err := catalog.CanonicalPath(checkpoint.canonicalPath)
	if err != nil || canonical != checkpoint.canonicalPath {
		return fmt.Errorf("%w: canonical path", ErrFileCheckpointBinding)
	}
	if !checkpoint.phase.Valid() || !checkpoint.commitState.Valid() {
		return fmt.Errorf("%w: phase or commit state", ErrInvalidFileCheckpoint)
	}
	if err := validateCheckpointLifecycleClaims(
		checkpoint.phase, checkpoint.quarantineReason,
		checkpoint.phaseBeforeQuarantine, checkpoint.retirementReason,
	); err != nil {
		return err
	}
	if err := validateCheckpointRanges(checkpoint.verifiedRanges, checkpoint.exactSize); err != nil {
		return err
	}
	if checkpoint.recordID != checkpoint.derivedRecordID() {
		return fmt.Errorf("%w: record ID does not match immutable binding", ErrFileCheckpointBinding)
	}
	if !checkpoint.checksum.IsZero() && checkpoint.checksum != checkpoint.derivedChecksum() {
		return ErrFileCheckpointChecksum
	}
	return nil
}

func (checkpoint FileCheckpointV1) derivedRecordID() FileCheckpointRecordID {
	hash := sha256.New()
	writeCheckpointBytes(hash, []byte(fileCheckpointDomain))
	writeCheckpointBytes(hash, []byte{FileCheckpointV1SchemaVersion})
	writeCheckpointBytes(hash, []byte(checkpoint.ownershipMarker))
	writeCheckpointBytes(hash, []byte(checkpoint.namespace))
	writeCheckpointBytes(hash, checkpoint.intentDigest.Bytes())
	writeCheckpointBytes(hash, checkpoint.fileID.Bytes())
	writeCheckpointBytes(hash, checkpoint.fileRevision.Bytes())
	writeCheckpointBytes(hash, []byte(checkpoint.canonicalPath))
	writeCheckpointU64(hash, checkpoint.exactSize)
	writeCheckpointBytes(hash, []byte(checkpoint.backendID))
	writeCheckpointBytes(hash, checkpoint.rootIdentity[:])
	writeCheckpointBytes(hash, checkpoint.ownedOutputObject[:])
	var result FileCheckpointRecordID
	copy(result[:], hash.Sum(nil))
	return result
}

func (checkpoint FileCheckpointV1) canonicalPayload() []byte {
	var encoded bytes.Buffer
	writeCheckpointString(&encoded, fileCheckpointDomain)
	encoded.WriteByte(FileCheckpointV1SchemaVersion)
	writeCheckpointString(&encoded, checkpoint.ownershipMarker)
	writeCheckpointString(&encoded, checkpoint.namespace)
	encoded.Write(checkpoint.recordID[:])
	encoded.Write(checkpoint.intentDigest.Bytes())
	encoded.Write(checkpoint.fileID.Bytes())
	encoded.Write(checkpoint.fileRevision.Bytes())
	writeCheckpointString(&encoded, checkpoint.canonicalPath)
	writeCheckpointU64(&encoded, checkpoint.exactSize)
	writeCheckpointString(&encoded, string(checkpoint.backendID))
	encoded.Write(checkpoint.rootIdentity[:])
	encoded.Write(checkpoint.ownedOutputObject[:])
	writeCheckpointU64(&encoded, checkpoint.stateGeneration)
	writeCheckpointU64(&encoded, checkpoint.checkpointGeneration)
	writeCheckpointU32(&encoded, uint32(len(checkpoint.verifiedRanges)))
	for _, current := range checkpoint.verifiedRanges {
		writeCheckpointU64(&encoded, current.Offset)
		writeCheckpointU64(&encoded, current.End)
	}
	encoded.WriteByte(byte(checkpoint.phase))
	encoded.WriteByte(byte(checkpoint.commitState))
	encoded.WriteByte(byte(checkpoint.quarantineReason))
	encoded.WriteByte(byte(checkpoint.phaseBeforeQuarantine))
	encoded.WriteByte(byte(checkpoint.retirementReason))
	return encoded.Bytes()
}

func (checkpoint FileCheckpointV1) derivedChecksum() FileCheckpointChecksum {
	hash := sha256.New()
	_, _ = hash.Write([]byte(fileCheckpointChecksumDomain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(checkpoint.canonicalPayload())
	var result FileCheckpointChecksum
	copy(result[:], hash.Sum(nil))
	return result
}

// CanonicalBytes excludes the envelope checksum.  It is suitable for vectors
// and identity comparisons; EncodeFileCheckpointV1 adds the fixed envelope and
// checksum used for storage.
func (checkpoint FileCheckpointV1) CanonicalBytes() []byte {
	if checkpoint.valid() != nil {
		return nil
	}
	return checkpoint.canonicalPayload()
}

func (checkpoint FileCheckpointV1) Bytes() []byte { return checkpoint.CanonicalBytes() }

func (checkpoint FileCheckpointV1) Encode() ([]byte, error) {
	return EncodeFileCheckpointV1(checkpoint)
}

func EncodeFileCheckpointV1(checkpoint FileCheckpointV1) ([]byte, error) {
	if err := checkpoint.valid(); err != nil {
		return nil, err
	}
	payload := checkpoint.canonicalPayload()
	checksum := checkpoint.derivedChecksum()
	encoded := make([]byte, 0, len(fileCheckpointMagic)+4+len(payload)+len(checksum))
	encoded = append(encoded, fileCheckpointMagic...)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
	encoded = append(encoded, length[:]...)
	encoded = append(encoded, payload...)
	encoded = append(encoded, checksum[:]...)
	return encoded, nil
}

func EncodeFileCheckpointRecord(record FileCheckpointRecord) ([]byte, error) {
	return EncodeFileCheckpointV1(record)
}
