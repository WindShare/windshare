package osfs

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

// The public facade keeps checkpoint values opaque.  Native storage proof and
// codec details remain in the internal package, while callers still get a
// stable, testable contract and cannot forge a record by mutating fields.
const (
	FileCheckpointV1SchemaVersion = 1
	FileCheckpointSchemaVersion   = FileCheckpointV1SchemaVersion
	FileCheckpointV1Version       = FileCheckpointV1SchemaVersion
	FileCheckpointOwnershipMarker = "windshare/file-checkpoint/v1"
	FileCheckpointNamespace       = ".windshare-output/checkpoints-v1"
	TransferIntentDigestBytes     = transfer.TransferIntentDigestBytes
	FileCheckpointOwnershipFile   = "ownership.marker"
	FileCheckpointCleanupState    = "cleanup.state"
	FileCheckpointCleanupLock     = "cleanup.lock"
	FileCheckpointQuarantine      = "quarantine"
)

var (
	ErrInvalidFileCheckpoint       = errors.New("file checkpoint v1 is invalid")
	ErrFileCheckpointChecksum      = errors.New("file checkpoint v1 checksum is invalid")
	ErrFileCheckpointNonCanonical  = errors.New("file checkpoint v1 encoding is not canonical")
	ErrFileCheckpointGeneration    = errors.New("file checkpoint v1 generation is invalid")
	ErrFileCheckpointBinding       = errors.New("file checkpoint v1 binding is invalid")
	ErrFileCheckpointRecovery      = errors.New("file checkpoint v1 has no verified committed record")
	ErrFileCheckpointOwnership     = errors.New("file checkpoint v1 ownership marker is invalid")
	ErrFileCheckpointCrashBoundary = errors.New("file checkpoint v1 crash cut is not recoverable")
	ErrCheckpointCleanerBusy       = errors.New("file checkpoint cleaner is already running")
	ErrCheckpointCleanerOwnership  = errors.New("file checkpoint cleaner cannot prove namespace ownership")
	ErrCheckpointCleanerState      = errors.New("file checkpoint cleaner state is corrupt")
	ErrCheckpointCleanerLimit      = errors.New("file checkpoint cleaner inspection limit exceeded")
)

type TransferIntentDigest = transfer.TransferIntentDigest

type (
	FileCheckpointRecordID [sha256.Size]byte
	FileCheckpointRootID   [sha256.Size]byte
	FileCheckpointObjectID [sha256.Size]byte
	FileCheckpointChecksum [sha256.Size]byte
)

func copyCheckpointID[T ~[sha256.Size]byte](value []byte) (T, error) {
	var result T
	if len(value) != sha256.Size {
		return result, ErrFileCheckpointBinding
	}
	copy(result[:], value)
	var zero T
	if result == zero {
		return result, ErrFileCheckpointBinding
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

func FileCheckpointRecordIDFromBytes(value []byte) (FileCheckpointRecordID, error) {
	return copyCheckpointID[FileCheckpointRecordID](value)
}
func FileCheckpointRootIDFromBytes(value []byte) (FileCheckpointRootID, error) {
	return copyCheckpointID[FileCheckpointRootID](value)
}
func FileCheckpointObjectIDFromBytes(value []byte) (FileCheckpointObjectID, error) {
	return copyCheckpointID[FileCheckpointObjectID](value)
}
func FileCheckpointChecksumFromBytes(value []byte) (FileCheckpointChecksum, error) {
	return copyCheckpointID[FileCheckpointChecksum](value)
}

func NewTransferIntentDigest(raw []byte) (TransferIntentDigest, error) {
	return transfer.TransferIntentDigestFromBytes(raw)
}

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

type FileCheckpointCommitState uint8

const (
	FileCheckpointCommitCandidate FileCheckpointCommitState = iota + 1
	FileCheckpointCommitVerified
	FileCheckpointCommitPublished
	FileCheckpointCommitQuarantined
)

func (phase FileCheckpointPhase) Valid() bool {
	return phase >= FileCheckpointPhaseReserved && phase <= FileCheckpointPhaseRetired
}

func (state FileCheckpointCommitState) Valid() bool {
	return state >= FileCheckpointCommitCandidate && state <= FileCheckpointCommitQuarantined
}

type FileCheckpointQuarantineReason uint8

const (
	FileCheckpointQuarantineAnchorMissing FileCheckpointQuarantineReason = iota + 1
	FileCheckpointQuarantineAnchorUnsafe
	FileCheckpointQuarantineStageMissing
	FileCheckpointQuarantineStageMismatch
	FileCheckpointQuarantineStageUnsafe
	FileCheckpointQuarantineFinalMismatch
	FileCheckpointQuarantineFinalUnsafe
	FileCheckpointQuarantinePartialObjectCreation
	FileCheckpointQuarantinePublicationHistory
	FileCheckpointQuarantineMetadataMismatch
	FileCheckpointQuarantineUpdateTemporary
	FileCheckpointQuarantineOutputObjectDuplicate
)

type FileCheckpointQuarantineOrigin uint8

const (
	FileCheckpointOriginReserved FileCheckpointQuarantineOrigin = iota + 1
	FileCheckpointOriginWitnessed
	FileCheckpointOriginPublishing
	FileCheckpointOriginPublishBlocked
	FileCheckpointOriginPublished
	FileCheckpointOriginRetiring
)

type FileCheckpointRetirementReason uint8

const (
	FileCheckpointRetirementPublished FileCheckpointRetirementReason = iota + 1
	FileCheckpointRetirementIsolatedFailure
	FileCheckpointRetirementPreObjectCollision
	FileCheckpointRetirementInvalidatedRevision
)

const (
	CheckpointPhaseReserved     = FileCheckpointPhaseReserved
	CheckpointPhaseActive       = FileCheckpointPhaseActive
	CheckpointPhasePaused       = FileCheckpointPhasePaused
	CheckpointPhasePublishing   = FileCheckpointPhasePublishing
	CheckpointPhasePublished    = FileCheckpointPhasePublished
	CheckpointPhaseQuarantined  = FileCheckpointPhaseQuarantined
	CheckpointPhaseRetired      = FileCheckpointPhaseRetired
	CheckpointCommitCandidate   = FileCheckpointCommitCandidate
	CheckpointCommitVerified    = FileCheckpointCommitVerified
	CheckpointCommitPublished   = FileCheckpointCommitPublished
	CheckpointCommitQuarantined = FileCheckpointCommitQuarantined
)

type FileCheckpointSpec struct {
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
	VerifiedRanges       []FileCheckpointRange
	Phase                FileCheckpointPhase
	CommitState          FileCheckpointCommitState
	QuarantineReason     FileCheckpointQuarantineReason
	QuarantineOrigin     FileCheckpointQuarantineOrigin
	RetirementReason     FileCheckpointRetirementReason
}

type FileCheckpointV1 struct{ encoded []byte }
type FileCheckpointRecord = FileCheckpointV1
type FileCheckpointRecordSpec = FileCheckpointSpec

func internalSpec(spec FileCheckpointSpec) resumestate.FileCheckpointSpec {
	ranges := make([]resumestate.FileCheckpointRange, len(spec.VerifiedRanges))
	for index, current := range spec.VerifiedRanges {
		ranges[index] = resumestate.FileCheckpointRange{Offset: current.Offset, End: current.End}
	}
	return resumestate.FileCheckpointSpec{
		OwnershipMarker: spec.OwnershipMarker, Namespace: spec.Namespace,
		TransferIntentDigest: spec.TransferIntentDigest, FileID: spec.FileID, FileRevision: spec.FileRevision,
		CanonicalPath: spec.CanonicalPath, ExactSize: spec.ExactSize, BackendID: spec.BackendID,
		RootIdentity: spec.RootIdentity, OwnedOutputObject: spec.OwnedOutputObject,
		StateGeneration: spec.StateGeneration, CheckpointGeneration: spec.CheckpointGeneration,
		VerifiedRanges: ranges, Phase: resumestate.FileCheckpointPhase(spec.Phase),
		CommitState:           resumestate.FileCheckpointCommitState(spec.CommitState),
		QuarantineReason:      resumestate.QuarantineReason(spec.QuarantineReason),
		PhaseBeforeQuarantine: resumestate.FilePhase(spec.QuarantineOrigin),
		RetirementReason:      resumestate.RetirementReason(spec.RetirementReason),
	}
}

func publicCheckpoint(internal resumestate.FileCheckpointV1) (FileCheckpointV1, error) {
	encoded, err := resumestate.EncodeFileCheckpointV1(internal)
	if err != nil {
		return FileCheckpointV1{}, wrapCheckpointError(err)
	}
	return FileCheckpointV1{encoded: encoded}, nil
}

func (checkpoint FileCheckpointV1) internal() (resumestate.FileCheckpointV1, error) {
	if len(checkpoint.encoded) == 0 {
		return resumestate.FileCheckpointV1{}, ErrInvalidFileCheckpoint
	}
	internal, err := resumestate.DecodeFileCheckpointV1(checkpoint.encoded)
	if err != nil {
		return resumestate.FileCheckpointV1{}, wrapCheckpointError(err)
	}
	return internal, nil
}

func NewFileCheckpointV1(spec FileCheckpointSpec) (FileCheckpointV1, error) {
	checkpoint, err := resumestate.NewFileCheckpointV1(internalSpec(spec))
	if err != nil {
		return FileCheckpointV1{}, wrapCheckpointError(err)
	}
	return publicCheckpoint(checkpoint)
}
func NewFileCheckpointRecord(spec FileCheckpointRecordSpec) (FileCheckpointRecord, error) {
	return NewFileCheckpointV1(spec)
}
func EncodeFileCheckpointV1(checkpoint FileCheckpointV1) ([]byte, error) {
	if _, err := checkpoint.internal(); err != nil {
		return nil, err
	}
	return append([]byte(nil), checkpoint.encoded...), nil
}
func EncodeFileCheckpointRecord(record FileCheckpointRecord) ([]byte, error) {
	return EncodeFileCheckpointV1(record)
}
func DecodeFileCheckpointV1(encoded []byte) (FileCheckpointV1, error) {
	checkpoint, err := resumestate.DecodeFileCheckpointV1(encoded)
	if err != nil {
		return FileCheckpointV1{}, wrapCheckpointError(err)
	}
	return publicCheckpoint(checkpoint)
}
func DecodeFileCheckpointRecord(encoded []byte) (FileCheckpointRecord, error) {
	return DecodeFileCheckpointV1(encoded)
}

func ReadFileCheckpointV1(reader io.Reader) (FileCheckpointV1, error) {
	checkpoint, err := resumestate.ReadFileCheckpointV1(reader)
	if err != nil {
		return FileCheckpointV1{}, wrapCheckpointError(err)
	}
	return publicCheckpoint(checkpoint)
}

func ReadFileCheckpointRecord(reader io.Reader) (FileCheckpointRecord, error) {
	return ReadFileCheckpointV1(reader)
}

func WriteFileCheckpointV1(writer io.Writer, checkpoint FileCheckpointV1) error {
	internal, err := checkpoint.internal()
	if err != nil {
		return err
	}
	return wrapCheckpointError(resumestate.WriteFileCheckpointV1(writer, internal))
}

func WriteFileCheckpointRecord(writer io.Writer, record FileCheckpointRecord) error {
	return WriteFileCheckpointV1(writer, record)
}

func (checkpoint FileCheckpointV1) Encode() ([]byte, error) {
	return EncodeFileCheckpointV1(checkpoint)
}
func (checkpoint FileCheckpointV1) CanonicalBytes() []byte {
	internal, err := checkpoint.internal()
	if err != nil {
		return nil
	}
	return internal.CanonicalBytes()
}
func (checkpoint FileCheckpointV1) Valid() bool {
	_, err := checkpoint.internal()
	return err == nil
}
func (checkpoint FileCheckpointV1) Bytes() []byte        { return checkpoint.CanonicalBytes() }
func (checkpoint FileCheckpointV1) SchemaVersion() uint8 { return FileCheckpointV1SchemaVersion }
func (checkpoint FileCheckpointV1) OwnershipMarker() string {
	internal, _ := checkpoint.internal()
	return internal.OwnershipMarker()
}
func (checkpoint FileCheckpointV1) Namespace() string {
	internal, _ := checkpoint.internal()
	return internal.Namespace()
}
func (checkpoint FileCheckpointV1) RecordID() FileCheckpointRecordID {
	internal, err := checkpoint.internal()
	if err != nil {
		return FileCheckpointRecordID{}
	}
	var result FileCheckpointRecordID
	copy(result[:], internal.RecordID().Bytes())
	return result
}
func (checkpoint FileCheckpointV1) TransferIntentDigest() transfer.TransferIntentDigest {
	internal, _ := checkpoint.internal()
	return internal.TransferIntentDigest()
}
func (checkpoint FileCheckpointV1) FileID() catalog.FileID {
	internal, _ := checkpoint.internal()
	return internal.FileID()
}
func (checkpoint FileCheckpointV1) FileRevision() content.FileRevision {
	internal, _ := checkpoint.internal()
	return internal.FileRevision()
}
func (checkpoint FileCheckpointV1) CanonicalPath() string {
	internal, _ := checkpoint.internal()
	return internal.CanonicalPath()
}
func (checkpoint FileCheckpointV1) ExactSize() uint64 {
	internal, _ := checkpoint.internal()
	return internal.ExactSize()
}
func (checkpoint FileCheckpointV1) BackendID() transfer.OutputBackendID {
	internal, _ := checkpoint.internal()
	return internal.BackendID()
}
func (checkpoint FileCheckpointV1) RootIdentity() FileCheckpointRootID {
	internal, err := checkpoint.internal()
	if err != nil {
		return FileCheckpointRootID{}
	}
	var result FileCheckpointRootID
	copy(result[:], internal.RootIdentity().Bytes())
	return result
}
func (checkpoint FileCheckpointV1) OwnedOutputObject() FileCheckpointObjectID {
	internal, err := checkpoint.internal()
	if err != nil {
		return FileCheckpointObjectID{}
	}
	var result FileCheckpointObjectID
	copy(result[:], internal.OwnedOutputObject().Bytes())
	return result
}
func (checkpoint FileCheckpointV1) OutputObject() FileCheckpointObjectID {
	return checkpoint.OwnedOutputObject()
}
func (checkpoint FileCheckpointV1) StateGeneration() uint64 {
	internal, _ := checkpoint.internal()
	return internal.StateGeneration()
}
func (checkpoint FileCheckpointV1) CheckpointGeneration() uint64 {
	internal, _ := checkpoint.internal()
	return internal.CheckpointGeneration()
}
func (checkpoint FileCheckpointV1) VerifiedRanges() []FileCheckpointRange {
	internal, err := checkpoint.internal()
	if err != nil {
		return nil
	}
	ranges := internal.VerifiedRanges()
	result := make([]FileCheckpointRange, len(ranges))
	for index, current := range ranges {
		result[index] = FileCheckpointRange{Offset: current.Offset, End: current.End}
	}
	return result
}
func (checkpoint FileCheckpointV1) Ranges() []FileCheckpointRange { return checkpoint.VerifiedRanges() }
func (checkpoint FileCheckpointV1) Phase() FileCheckpointPhase {
	internal, _ := checkpoint.internal()
	return FileCheckpointPhase(internal.Phase())
}
func (checkpoint FileCheckpointV1) CommitState() FileCheckpointCommitState {
	internal, _ := checkpoint.internal()
	return FileCheckpointCommitState(internal.CommitState())
}
func (checkpoint FileCheckpointV1) QuarantineReason() FileCheckpointQuarantineReason {
	internal, _ := checkpoint.internal()
	return FileCheckpointQuarantineReason(internal.QuarantineReason())
}
func (checkpoint FileCheckpointV1) QuarantineOrigin() FileCheckpointQuarantineOrigin {
	internal, _ := checkpoint.internal()
	return FileCheckpointQuarantineOrigin(internal.PhaseBeforeQuarantine())
}
func (checkpoint FileCheckpointV1) RetirementReason() FileCheckpointRetirementReason {
	internal, _ := checkpoint.internal()
	return FileCheckpointRetirementReason(internal.RetirementReason())
}
func (checkpoint FileCheckpointV1) Checksum() FileCheckpointChecksum {
	internal, err := checkpoint.internal()
	if err != nil {
		return FileCheckpointChecksum{}
	}
	var result FileCheckpointChecksum
	copy(result[:], internal.Checksum().Bytes())
	return result
}

func NewFileCheckpointOwnership(backendID string, rootIdentity []byte) (FileCheckpointOwnership, error) {
	ownership, err := resumestate.NewFileCheckpointOwnership(backendID, rootIdentity)
	if err != nil {
		return FileCheckpointOwnership{}, wrapCheckpointError(err)
	}
	var root FileCheckpointRootID
	copy(root[:], ownership.RootIdentity.Bytes())
	return FileCheckpointOwnership{Marker: ownership.Marker, Namespace: ownership.Namespace, BackendID: ownership.BackendID, RootIdentity: root}, nil
}

type FileCheckpointOwnership struct {
	Marker       string
	Namespace    string
	BackendID    string
	RootIdentity FileCheckpointRootID
}

func (ownership FileCheckpointOwnership) Valid() bool {
	_, err := resumestate.EncodeFileCheckpointOwnership(internalOwnership(ownership))
	return err == nil
}

func (ownership FileCheckpointOwnership) CanonicalBytes() []byte {
	internal := internalOwnership(ownership)
	return internal.CanonicalBytes()
}

func internalOwnership(ownership FileCheckpointOwnership) resumestate.FileCheckpointOwnership {
	var root resumestate.FileCheckpointRootID
	copy(root[:], ownership.RootIdentity[:])
	return resumestate.FileCheckpointOwnership{Marker: ownership.Marker, Namespace: ownership.Namespace, BackendID: ownership.BackendID, RootIdentity: root}
}
func EncodeFileCheckpointOwnership(ownership FileCheckpointOwnership) ([]byte, error) {
	encoded, err := resumestate.EncodeFileCheckpointOwnership(internalOwnership(ownership))
	if err != nil {
		return nil, wrapCheckpointError(err)
	}
	return encoded, nil
}
func DecodeFileCheckpointOwnership(encoded []byte) (FileCheckpointOwnership, error) {
	ownership, err := resumestate.DecodeFileCheckpointOwnership(encoded)
	if err != nil {
		return FileCheckpointOwnership{}, wrapCheckpointError(err)
	}
	var root FileCheckpointRootID
	copy(root[:], ownership.RootIdentity.Bytes())
	return FileCheckpointOwnership{Marker: ownership.Marker, Namespace: ownership.Namespace, BackendID: ownership.BackendID, RootIdentity: root}, nil
}

func SelectVerifiedCheckpoint(records ...FileCheckpointV1) (FileCheckpointV1, error) {
	internalRecords := make([]resumestate.FileCheckpointV1, len(records))
	for index, record := range records {
		internal, err := record.internal()
		if err != nil {
			return FileCheckpointV1{}, err
		}
		internalRecords[index] = internal
	}
	selected, err := resumestate.SelectVerifiedCheckpoint(internalRecords...)
	if err != nil {
		return FileCheckpointV1{}, wrapCheckpointError(err)
	}
	return publicCheckpoint(selected)
}

func CheckpointIdentityEqual(left, right FileCheckpointV1) bool {
	leftInternal, leftErr := left.internal()
	rightInternal, rightErr := right.internal()
	return leftErr == nil && rightErr == nil && resumestate.CheckpointIdentityEqual(leftInternal, rightInternal)
}
func RecoverFileCheckpoint(committed, candidate *FileCheckpointV1) (FileCheckpointV1, error) {
	var committedInternal, candidateInternal *resumestate.FileCheckpointV1
	if committed != nil {
		value, err := committed.internal()
		if err != nil {
			return FileCheckpointV1{}, err
		}
		committedInternal = &value
	}
	if candidate != nil {
		value, err := candidate.internal()
		if err != nil {
			return FileCheckpointV1{}, err
		}
		candidateInternal = &value
	}
	selected, err := resumestate.RecoverFileCheckpoint(committedInternal, candidateInternal)
	if err != nil {
		return FileCheckpointV1{}, wrapCheckpointError(err)
	}
	return publicCheckpoint(selected)
}
func ValidateCheckpointTransition(previous, next FileCheckpointV1) error {
	left, err := previous.internal()
	if err != nil {
		return err
	}
	right, err := next.internal()
	if err != nil {
		return err
	}
	return wrapCheckpointError(resumestate.ValidateCheckpointTransition(left, right))
}
func CheckpointGenerationAdvance(previous FileCheckpointV1, ranges []FileCheckpointRange, phase FileCheckpointPhase, commitState FileCheckpointCommitState) (FileCheckpointV1, error) {
	internal, err := previous.internal()
	if err != nil {
		return FileCheckpointV1{}, err
	}
	converted := make([]resumestate.FileCheckpointRange, len(ranges))
	for index, current := range ranges {
		converted[index] = resumestate.FileCheckpointRange{Offset: current.Offset, End: current.End}
	}
	next, err := resumestate.CheckpointGenerationAdvance(internal, converted, resumestate.FileCheckpointPhase(phase), resumestate.FileCheckpointCommitState(commitState))
	if err != nil {
		return FileCheckpointV1{}, wrapCheckpointError(err)
	}
	return publicCheckpoint(next)
}
func PromoteCheckpoint(candidate FileCheckpointV1, phase FileCheckpointPhase, commitState FileCheckpointCommitState) (FileCheckpointV1, error) {
	internal, err := candidate.internal()
	if err != nil {
		return FileCheckpointV1{}, err
	}
	next, err := resumestate.PromoteCheckpoint(internal, resumestate.FileCheckpointPhase(phase), resumestate.FileCheckpointCommitState(commitState))
	if err != nil {
		return FileCheckpointV1{}, wrapCheckpointError(err)
	}
	return publicCheckpoint(next)
}
func CanonicalizeFileCheckpointRanges(ranges []FileCheckpointRange) ([]FileCheckpointRange, error) {
	converted := make([]resumestate.FileCheckpointRange, len(ranges))
	for index, current := range ranges {
		converted[index] = resumestate.FileCheckpointRange{Offset: current.Offset, End: current.End}
	}
	canonical, err := resumestate.CanonicalizeFileCheckpointRanges(converted)
	if err != nil {
		return nil, wrapCheckpointError(err)
	}
	result := make([]FileCheckpointRange, len(canonical))
	for index, current := range canonical {
		result[index] = FileCheckpointRange{Offset: current.Offset, End: current.End}
	}
	return result, nil
}

func wrapCheckpointError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, resumestate.ErrFileCheckpointChecksum):
		return fmt.Errorf("%w: %w", ErrFileCheckpointChecksum, err)
	case errors.Is(err, resumestate.ErrFileCheckpointGeneration):
		return fmt.Errorf("%w: %w", ErrFileCheckpointGeneration, err)
	case errors.Is(err, resumestate.ErrFileCheckpointBinding):
		return fmt.Errorf("%w: %w", ErrFileCheckpointBinding, err)
	case errors.Is(err, resumestate.ErrFileCheckpointRecovery):
		return fmt.Errorf("%w: %w", ErrFileCheckpointRecovery, err)
	case errors.Is(err, resumestate.ErrFileCheckpointOwnership):
		return fmt.Errorf("%w: %w", ErrFileCheckpointOwnership, err)
	case errors.Is(err, resumestate.ErrFileCheckpointNonCanonical):
		return fmt.Errorf("%w: %w", ErrFileCheckpointNonCanonical, err)
	case errors.Is(err, resumestate.ErrFileCheckpointCrashBoundary):
		return fmt.Errorf("%w: %w", ErrFileCheckpointCrashBoundary, err)
	default:
		return fmt.Errorf("%w: %w", ErrInvalidFileCheckpoint, err)
	}
}
