package osfs

import (
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	FileCheckpointV2SchemaVersion = checkpointmodel.SchemaVersion
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
	FileCheckpointRecordID            = checkpointmodel.RecordID
	FileCheckpointLineageID           = checkpointmodel.CheckpointLineageID
	FileCheckpointObjectID            = checkpointmodel.ObjectID
	FileCheckpointChecksum            = checkpointmodel.Checksum
	FileCheckpointRange               = checkpointmodel.Range
	FileCheckpointPhase               = checkpointmodel.Phase
	FileCheckpointCommitState         = checkpointmodel.CommitState
	FileCheckpointQuarantineReason    = checkpointmodel.QuarantineReason
	FileCheckpointQuarantineOrigin    = checkpointmodel.QuarantineOrigin
	FileCheckpointRetirementReason    = checkpointmodel.RetirementReason
	FileCheckpointMaterializerKind    = checkpointmodel.MaterializerKind
	FileCheckpointCertification       = checkpointmodel.CertificationID
	FileCheckpointRootOpenDisposition = checkpointmodel.RootOpenDisposition
)

const (
	FileCheckpointReserved    = checkpointmodel.PhaseReserved
	FileCheckpointActive      = checkpointmodel.PhaseActive
	FileCheckpointPaused      = checkpointmodel.PhasePaused
	FileCheckpointPublishing  = checkpointmodel.PhasePublishing
	FileCheckpointPublished   = checkpointmodel.PhasePublished
	FileCheckpointRetired     = checkpointmodel.PhaseRetired
	FileCheckpointQuarantined = checkpointmodel.PhaseQuarantined

	FileCheckpointCandidate         = checkpointmodel.CommitCandidate
	FileCheckpointVerified          = checkpointmodel.CommitVerified
	FileCheckpointCommitted         = checkpointmodel.CommitPublished
	FileCheckpointQuarantinedCommit = checkpointmodel.CommitQuarantined

	FileCheckpointMaterializerNativeTree    = checkpointmodel.MaterializerNativeTree
	FileCheckpointMaterializerLegacyFSATree = checkpointmodel.MaterializerLegacyFSATree
	FileCheckpointMaterializerOriginPrivate = checkpointmodel.MaterializerOriginPrivate
	FileCheckpointMaterializerAtomicFile    = checkpointmodel.MaterializerAtomicFile
	FileCheckpointMaterializerFSATree       = checkpointmodel.MaterializerFSATree

	FileCheckpointCallerProvidedContainer = checkpointmodel.CallerProvidedContainer
	FileCheckpointAuthorityCreatedRoot    = checkpointmodel.AuthorityCreatedRoot

	FileCheckpointCertificationLinuxExt4ProcessRestart   = checkpointmodel.CertificationLinuxExt4ProcessRestart
	FileCheckpointCertificationWindowsNTFSProcessRestart = checkpointmodel.CertificationWindowsNTFSProcessRestart
)

type FileCheckpointSpec struct {
	OperationID                  receivecontract.OperationID
	ReceiveIntentDigest          transfer.ReceiveIntentDigest
	MaterializationBindingDigest receivecontract.BindingDigest
	FileID                       catalog.FileID
	FileRevision                 content.FileRevision
	CanonicalPath                string
	ExactSize                    uint64
	MaterializerKind             FileCheckpointMaterializerKind
	AuthorityRef                 []byte
	OwnedObjectID                []byte
	StateGeneration              uint64
	CheckpointGeneration         uint64
	VerifiedRanges               []FileCheckpointRange
	Phase                        FileCheckpointPhase
	CommitState                  FileCheckpointCommitState
	QuarantineReason             FileCheckpointQuarantineReason
	QuarantineOrigin             FileCheckpointQuarantineOrigin
	RetirementReason             FileCheckpointRetirementReason
}

// FileCheckpointV2 stays opaque so callers cannot mutate the immutable binding
// or verified ranges after the checkpoint reducer has admitted them.
type FileCheckpointV2 struct{ record checkpointmodel.Record }

func NewFileCheckpointV2(spec FileCheckpointSpec) (FileCheckpointV2, error) {
	record, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		OperationID: spec.OperationID, ReceiveIntentDigest: spec.ReceiveIntentDigest,
		MaterializationBindingDigest: spec.MaterializationBindingDigest,
		FileID:                       spec.FileID, FileRevision: spec.FileRevision,
		CanonicalPath: spec.CanonicalPath, ExactSize: spec.ExactSize,
		MaterializerKind: spec.MaterializerKind, AuthorityRef: spec.AuthorityRef,
		OwnedObjectID: spec.OwnedObjectID, StateGeneration: spec.StateGeneration,
		CheckpointGeneration: spec.CheckpointGeneration, VerifiedRanges: spec.VerifiedRanges,
		Phase: spec.Phase, CommitState: spec.CommitState,
		QuarantineReason: spec.QuarantineReason, QuarantineOrigin: spec.QuarantineOrigin,
		RetirementReason: spec.RetirementReason,
	})
	if err != nil {
		return FileCheckpointV2{}, err
	}
	return FileCheckpointV2{record: record}, nil
}

func EncodeFileCheckpointV2(checkpoint FileCheckpointV2) ([]byte, error) {
	return checkpointmodel.EncodeRecord(checkpoint.record)
}

func DecodeFileCheckpointV2(encoded []byte) (FileCheckpointV2, error) {
	record, err := checkpointmodel.DecodeRecord(encoded)
	if err != nil {
		return FileCheckpointV2{}, err
	}
	return FileCheckpointV2{record: record}, nil
}

func (checkpoint FileCheckpointV2) CanonicalBytes() []byte { return checkpoint.record.CanonicalBytes() }
func (checkpoint FileCheckpointV2) Valid() bool            { return checkpoint.record.Valid() }
func (checkpoint FileCheckpointV2) SchemaVersion() uint8   { return checkpoint.record.SchemaVersion() }
func (checkpoint FileCheckpointV2) OwnershipMarker() string {
	return checkpoint.record.OwnershipMarker()
}
func (checkpoint FileCheckpointV2) Namespace() string { return checkpoint.record.Namespace() }
func (checkpoint FileCheckpointV2) RecordID() FileCheckpointRecordID {
	return checkpoint.record.RecordID()
}
func (checkpoint FileCheckpointV2) CheckpointLineageID() (FileCheckpointLineageID, error) {
	return checkpoint.record.CheckpointLineageID()
}
func (checkpoint FileCheckpointV2) CheckpointLineageCanonicalBytes() ([]byte, error) {
	return checkpoint.record.CheckpointLineageCanonicalBytes()
}
func (checkpoint FileCheckpointV2) OperationID() receivecontract.OperationID {
	return checkpoint.record.OperationID()
}
func (checkpoint FileCheckpointV2) ReceiveIntentDigest() transfer.ReceiveIntentDigest {
	return checkpoint.record.ReceiveIntentDigest()
}
func (checkpoint FileCheckpointV2) MaterializationBindingDigest() receivecontract.BindingDigest {
	return checkpoint.record.MaterializationBindingDigest()
}
func (checkpoint FileCheckpointV2) FileID() catalog.FileID { return checkpoint.record.FileID() }
func (checkpoint FileCheckpointV2) FileRevision() content.FileRevision {
	return checkpoint.record.FileRevision()
}
func (checkpoint FileCheckpointV2) CanonicalPath() string { return checkpoint.record.CanonicalPath() }
func (checkpoint FileCheckpointV2) ExactSize() uint64     { return checkpoint.record.ExactSize() }
func (checkpoint FileCheckpointV2) MaterializerKind() FileCheckpointMaterializerKind {
	return checkpoint.record.MaterializerKind()
}
func (checkpoint FileCheckpointV2) AuthorityRef() receivecontract.AuthorityRef {
	return checkpoint.record.AuthorityRef()
}
func (checkpoint FileCheckpointV2) OwnedObjectID() FileCheckpointObjectID {
	return checkpoint.record.OwnedObjectID()
}
func (checkpoint FileCheckpointV2) StateGeneration() uint64 {
	return checkpoint.record.StateGeneration()
}
func (checkpoint FileCheckpointV2) CheckpointGeneration() uint64 {
	return checkpoint.record.CheckpointGeneration()
}
func (checkpoint FileCheckpointV2) VerifiedRanges() []FileCheckpointRange {
	return checkpoint.record.VerifiedRanges()
}
func (checkpoint FileCheckpointV2) Phase() FileCheckpointPhase {
	return checkpoint.record.Phase()
}
func (checkpoint FileCheckpointV2) CommitState() FileCheckpointCommitState {
	return checkpoint.record.CommitState()
}
func (checkpoint FileCheckpointV2) QuarantineReason() FileCheckpointQuarantineReason {
	return checkpoint.record.QuarantineReason()
}
func (checkpoint FileCheckpointV2) QuarantineOrigin() FileCheckpointQuarantineOrigin {
	return checkpoint.record.QuarantineOrigin()
}
func (checkpoint FileCheckpointV2) RetirementReason() FileCheckpointRetirementReason {
	return checkpoint.record.RetirementReason()
}
func (checkpoint FileCheckpointV2) Checksum() FileCheckpointChecksum {
	return checkpoint.record.Checksum()
}

type FileCheckpointOwnershipSpec struct {
	Materializer        FileCheckpointMaterializerKind
	Certification       FileCheckpointCertification
	AuthorityRef        []byte
	RootOpenDisposition FileCheckpointRootOpenDisposition
}

type FileCheckpointOwnership struct{ ownership checkpointmodel.Ownership }

func NewFileCheckpointOwnership(spec FileCheckpointOwnershipSpec) (FileCheckpointOwnership, error) {
	ownership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Materializer: spec.Materializer, Certification: spec.Certification,
		AuthorityRef: spec.AuthorityRef, RootOpenDisposition: spec.RootOpenDisposition,
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
func (ownership FileCheckpointOwnership) Valid() bool { return ownership.ownership.Valid() }
func (ownership FileCheckpointOwnership) CanonicalBytes() []byte {
	return ownership.ownership.CanonicalBytes()
}
func (ownership FileCheckpointOwnership) MaterializerKind() FileCheckpointMaterializerKind {
	return ownership.ownership.MaterializerKind()
}
func (ownership FileCheckpointOwnership) Certification() FileCheckpointCertification {
	return ownership.ownership.Certification()
}
func (ownership FileCheckpointOwnership) AuthorityRef() receivecontract.AuthorityRef {
	return ownership.ownership.AuthorityRef()
}
func (ownership FileCheckpointOwnership) RootOpenDisposition() FileCheckpointRootOpenDisposition {
	return ownership.ownership.RootOpenDisposition()
}
