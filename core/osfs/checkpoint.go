package osfs

import (
	"crypto/sha256"
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
)

const (
	FileCheckpointV1SchemaVersion = checkpointmodel.SchemaVersion
	FileCheckpointOwnershipMarker = checkpointmodel.OwnershipMarker
	FileCheckpointNamespace       = checkpointmodel.NamespaceName
)

var (
	ErrInvalidFileCheckpoint               = checkpointmodel.ErrInvalidRecord
	ErrFileCheckpointChecksum              = checkpointmodel.ErrRecordChecksum
	ErrFileCheckpointNonCanonical          = checkpointmodel.ErrRecordNonCanonical
	ErrFileCheckpointBinding               = checkpointmodel.ErrRecordBinding
	ErrFileCheckpointOwnership             = checkpointmodel.ErrInvalidOwnership
	ErrFileCheckpointOwnershipChecksum     = checkpointmodel.ErrOwnershipChecksum
	ErrFileCheckpointOwnershipNonCanonical = checkpointmodel.ErrOwnershipNonCanonical
)

type (
	FileCheckpointRecordID [sha256.Size]byte
	FileCheckpointRootID   [sha256.Size]byte
	FileCheckpointObjectID [sha256.Size]byte
	FileCheckpointChecksum [sha256.Size]byte
)

func (id FileCheckpointRecordID) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id FileCheckpointRootID) Bytes() []byte   { return append([]byte(nil), id[:]...) }
func (id FileCheckpointObjectID) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id FileCheckpointChecksum) Bytes() []byte { return append([]byte(nil), id[:]...) }
func (id FileCheckpointRecordID) IsZero() bool  { return id == FileCheckpointRecordID{} }
func (id FileCheckpointRootID) IsZero() bool    { return id == FileCheckpointRootID{} }
func (id FileCheckpointObjectID) IsZero() bool  { return id == FileCheckpointObjectID{} }
func (id FileCheckpointChecksum) IsZero() bool  { return id == FileCheckpointChecksum{} }

type FileCheckpointRange struct {
	Offset uint64
	End    uint64
}

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

func (phase FileCheckpointPhase) Valid() bool {
	return checkpointmodel.Phase(phase).Valid()
}

type FileCheckpointCommitState uint8

const (
	FileCheckpointCommitCandidate FileCheckpointCommitState = iota + 1
	FileCheckpointCommitVerified
	FileCheckpointCommitPublished
	FileCheckpointCommitQuarantined
)

func (state FileCheckpointCommitState) Valid() bool {
	return checkpointmodel.CommitState(state).Valid()
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

// FileCheckpointV1 is opaque so only checkpointmodel can construct durable
// state or interpret its bytes.
type FileCheckpointV1 struct {
	record checkpointmodel.Record
}

func NewFileCheckpointV1(spec FileCheckpointSpec) (FileCheckpointV1, error) {
	ranges := make([]checkpointmodel.Range, len(spec.VerifiedRanges))
	for index, current := range spec.VerifiedRanges {
		ranges[index] = checkpointmodel.Range{Offset: current.Offset, End: current.End}
	}
	record, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		OwnershipMarker:      spec.OwnershipMarker,
		Namespace:            spec.Namespace,
		TransferIntentDigest: spec.TransferIntentDigest,
		FileID:               spec.FileID,
		FileRevision:         spec.FileRevision,
		CanonicalPath:        spec.CanonicalPath,
		ExactSize:            spec.ExactSize,
		BackendID:            spec.BackendID,
		RootIdentity:         spec.RootIdentity,
		OwnedOutputObject:    spec.OwnedOutputObject,
		StateGeneration:      spec.StateGeneration,
		CheckpointGeneration: spec.CheckpointGeneration,
		VerifiedRanges:       ranges,
		Phase:                checkpointmodel.Phase(spec.Phase),
		CommitState:          checkpointmodel.CommitState(spec.CommitState),
		QuarantineReason:     checkpointmodel.QuarantineReason(spec.QuarantineReason),
		QuarantineOrigin:     checkpointmodel.QuarantineOrigin(spec.QuarantineOrigin),
		RetirementReason:     checkpointmodel.RetirementReason(spec.RetirementReason),
	})
	if err != nil {
		return FileCheckpointV1{}, err
	}
	return FileCheckpointV1{record: record}, nil
}

func EncodeFileCheckpointV1(checkpoint FileCheckpointV1) ([]byte, error) {
	return checkpointmodel.EncodeRecord(checkpoint.record)
}

func DecodeFileCheckpointV1(encoded []byte) (FileCheckpointV1, error) {
	record, err := checkpointmodel.DecodeRecord(encoded)
	if err != nil {
		return FileCheckpointV1{}, err
	}
	return FileCheckpointV1{record: record}, nil
}

func (checkpoint FileCheckpointV1) CanonicalBytes() []byte {
	return checkpoint.record.CanonicalBytes()
}

func (checkpoint FileCheckpointV1) Valid() bool {
	return checkpoint.record.Valid()
}

func (checkpoint FileCheckpointV1) SchemaVersion() uint8 {
	return checkpoint.record.SchemaVersion()
}

func (checkpoint FileCheckpointV1) OwnershipMarker() string {
	return checkpoint.record.OwnershipMarker()
}

func (checkpoint FileCheckpointV1) Namespace() string {
	return checkpoint.record.Namespace()
}

func (checkpoint FileCheckpointV1) RecordID() FileCheckpointRecordID {
	return FileCheckpointRecordID(checkpoint.record.RecordID())
}

func (checkpoint FileCheckpointV1) TransferIntentDigest() transfer.TransferIntentDigest {
	return checkpoint.record.TransferIntentDigest()
}

func (checkpoint FileCheckpointV1) FileID() catalog.FileID {
	return checkpoint.record.FileID()
}

func (checkpoint FileCheckpointV1) FileRevision() content.FileRevision {
	return checkpoint.record.FileRevision()
}

func (checkpoint FileCheckpointV1) CanonicalPath() string {
	return checkpoint.record.CanonicalPath()
}

func (checkpoint FileCheckpointV1) ExactSize() uint64 {
	return checkpoint.record.ExactSize()
}

func (checkpoint FileCheckpointV1) BackendID() transfer.OutputBackendID {
	return checkpoint.record.BackendID()
}

func (checkpoint FileCheckpointV1) RootIdentity() FileCheckpointRootID {
	return FileCheckpointRootID(checkpoint.record.RootIdentity())
}

func (checkpoint FileCheckpointV1) OwnedOutputObject() FileCheckpointObjectID {
	return FileCheckpointObjectID(checkpoint.record.OwnedOutputObject())
}

func (checkpoint FileCheckpointV1) StateGeneration() uint64 {
	return checkpoint.record.StateGeneration()
}

func (checkpoint FileCheckpointV1) CheckpointGeneration() uint64 {
	return checkpoint.record.CheckpointGeneration()
}

func (checkpoint FileCheckpointV1) VerifiedRanges() []FileCheckpointRange {
	ranges := checkpoint.record.VerifiedRanges()
	result := make([]FileCheckpointRange, len(ranges))
	for index, current := range ranges {
		result[index] = FileCheckpointRange{Offset: current.Offset, End: current.End}
	}
	return result
}

func (checkpoint FileCheckpointV1) Phase() FileCheckpointPhase {
	return FileCheckpointPhase(checkpoint.record.Phase())
}

func (checkpoint FileCheckpointV1) CommitState() FileCheckpointCommitState {
	return FileCheckpointCommitState(checkpoint.record.CommitState())
}

func (checkpoint FileCheckpointV1) QuarantineReason() FileCheckpointQuarantineReason {
	return FileCheckpointQuarantineReason(checkpoint.record.QuarantineReason())
}

func (checkpoint FileCheckpointV1) QuarantineOrigin() FileCheckpointQuarantineOrigin {
	return FileCheckpointQuarantineOrigin(checkpoint.record.QuarantineOrigin())
}

func (checkpoint FileCheckpointV1) RetirementReason() FileCheckpointRetirementReason {
	return FileCheckpointRetirementReason(checkpoint.record.RetirementReason())
}

func (checkpoint FileCheckpointV1) Checksum() FileCheckpointChecksum {
	return FileCheckpointChecksum(checkpoint.record.Checksum())
}

type FileCheckpointRootOpenDisposition string

const (
	FileCheckpointCallerProvidedContainer FileCheckpointRootOpenDisposition = "caller-provided-container"
	FileCheckpointAuthorityCreatedRoot    FileCheckpointRootOpenDisposition = "authority-created-root"
)

type FileCheckpointCertification string

const (
	FileCheckpointCertificationLinuxExt4ProcessRestart   FileCheckpointCertification = "linux/ext4/process-restart/v2"
	FileCheckpointCertificationWindowsNTFSProcessRestart FileCheckpointCertification = "windows/ntfs/process-restart/v1"
)

type FileCheckpointOwnershipSpec struct {
	BackendID           string
	Certification       FileCheckpointCertification
	RootIdentity        []byte
	RootOpenDisposition FileCheckpointRootOpenDisposition
}

type FileCheckpointOwnership struct {
	ownership checkpointmodel.Ownership
}

func NewFileCheckpointOwnership(spec FileCheckpointOwnershipSpec) (FileCheckpointOwnership, error) {
	backend, err := transfer.NewOutputBackendID(spec.BackendID)
	if err != nil {
		return FileCheckpointOwnership{}, errors.Join(ErrFileCheckpointOwnership, err)
	}
	ownership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Backend:             backend,
		Certification:       checkpointmodel.CertificationID(spec.Certification),
		RootIdentity:        spec.RootIdentity,
		RootOpenDisposition: checkpointmodel.RootOpenDisposition(spec.RootOpenDisposition),
	})
	if err != nil {
		return FileCheckpointOwnership{}, err
	}
	return FileCheckpointOwnership{ownership: ownership}, nil
}

func EncodeFileCheckpointOwnership(ownership FileCheckpointOwnership) ([]byte, error) {
	return checkpointmodel.EncodeOwnership(ownership.ownership)
}

func DecodeFileCheckpointOwnership(encoded []byte) (FileCheckpointOwnership, error) {
	ownership, err := checkpointmodel.DecodeOwnership(encoded)
	if err != nil {
		return FileCheckpointOwnership{}, err
	}
	return FileCheckpointOwnership{ownership: ownership}, nil
}

func (ownership FileCheckpointOwnership) Valid() bool {
	return ownership.ownership.Valid()
}

func (ownership FileCheckpointOwnership) CanonicalBytes() []byte {
	return ownership.ownership.CanonicalBytes()
}

func (ownership FileCheckpointOwnership) BackendID() transfer.OutputBackendID {
	return ownership.ownership.Backend()
}

func (ownership FileCheckpointOwnership) Certification() FileCheckpointCertification {
	return FileCheckpointCertification(ownership.ownership.Certification())
}

func (ownership FileCheckpointOwnership) RootIdentity() FileCheckpointRootID {
	return FileCheckpointRootID(ownership.ownership.RootIdentity())
}

func (ownership FileCheckpointOwnership) RootOpenDisposition() FileCheckpointRootOpenDisposition {
	return FileCheckpointRootOpenDisposition(ownership.ownership.RootOpenDisposition())
}
