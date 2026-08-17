// Package fileexecution owns one native DirectTree file transaction. A
// FileCheckpointV2 is the only source of verified durable ranges.
package fileexecution

import (
	"context"
	"io"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const MaximumObjectAllocationAttempts = 8

type CheckpointKey struct {
	operation       receivecontract.OperationID
	intent          transfer.ReceiveIntentDigest
	materialization receivecontract.BindingDigest
	fileID          catalog.FileID
	revision        content.FileRevision
	path            string
	exactSize       uint64
	materializer    checkpointmodel.MaterializerKind
	authority       receivecontract.AuthorityRef
}

func (key CheckpointKey) OperationID() receivecontract.OperationID          { return key.operation }
func (key CheckpointKey) ReceiveIntentDigest() transfer.ReceiveIntentDigest { return key.intent }
func (key CheckpointKey) MaterializationBindingDigest() receivecontract.BindingDigest {
	return key.materialization
}
func (key CheckpointKey) FileID() catalog.FileID                             { return key.fileID }
func (key CheckpointKey) FileRevision() content.FileRevision                 { return key.revision }
func (key CheckpointKey) CanonicalPath() string                              { return key.path }
func (key CheckpointKey) ExactSize() uint64                                  { return key.exactSize }
func (key CheckpointKey) MaterializerKind() checkpointmodel.MaterializerKind { return key.materializer }
func (key CheckpointKey) AuthorityRef() receivecontract.AuthorityRef         { return key.authority }

func (key CheckpointKey) valid() bool {
	return !key.operation.IsZero() && !key.intent.IsZero() && !key.materialization.IsZero() &&
		!key.fileID.IsZero() && !key.revision.IsZero() && key.path != "" &&
		key.exactSize <= catalog.MaxFileSize && key.materializer.Valid() && !key.authority.IsZero()
}

func (key CheckpointKey) matches(record checkpointmodel.Record) bool {
	return key.valid() && record.Valid() && record.OperationID() == key.operation &&
		record.ReceiveIntentDigest() == key.intent &&
		record.MaterializationBindingDigest() == key.materialization &&
		record.FileID() == key.fileID && record.FileRevision() == key.revision &&
		record.CanonicalPath() == key.path && record.ExactSize() == key.exactSize &&
		record.MaterializerKind() == key.materializer && record.AuthorityRef() == key.authority
}

type CheckpointObservation struct {
	present bool
	record  checkpointmodel.Record
}

func MissingCheckpoint() CheckpointObservation { return CheckpointObservation{} }

func ObservedCheckpoint(record checkpointmodel.Record) (CheckpointObservation, error) {
	if !record.Valid() {
		return CheckpointObservation{}, ErrInvalidObservation
	}
	return CheckpointObservation{present: true, record: record}, nil
}

func (observation CheckpointObservation) Record() (checkpointmodel.Record, bool) {
	return observation.record, observation.present && observation.record.Valid()
}

func (observation CheckpointObservation) valid() bool {
	return !observation.present || observation.record.Valid()
}

type OwnedCondition uint8

const (
	OwnedAbsent OwnedCondition = iota + 1
	OwnedReady
	OwnedObjectCollision
	OwnedAnchorMissing
	OwnedAnchorUnsafe
	OwnedStageMissing
	OwnedStageMismatch
	OwnedStageUnsafe
)

func (condition OwnedCondition) valid() bool {
	return condition >= OwnedAbsent && condition <= OwnedStageUnsafe
}

type OwnedObservation struct {
	object    checkpointmodel.ObjectID
	condition OwnedCondition
}

func NewOwnedObservation(object checkpointmodel.ObjectID, condition OwnedCondition) (OwnedObservation, error) {
	if object.IsZero() || !condition.valid() {
		return OwnedObservation{}, ErrInvalidObservation
	}
	return OwnedObservation{object: object, condition: condition}, nil
}

func (observation OwnedObservation) ObjectID() checkpointmodel.ObjectID { return observation.object }
func (observation OwnedObservation) Condition() OwnedCondition          { return observation.condition }
func (observation OwnedObservation) validFor(object checkpointmodel.ObjectID) bool {
	return !object.IsZero() && observation.object == object && observation.condition.valid()
}

func (observation OwnedObservation) ValidForCleanup(object checkpointmodel.ObjectID) bool {
	return observation.validFor(object)
}

type FinalCondition uint8

const (
	FinalAbsent FinalCondition = iota + 1
	FinalCollision
	FinalOwnedExact
	FinalUnsafe
)

func (condition FinalCondition) valid() bool {
	return condition >= FinalAbsent && condition <= FinalUnsafe
}

type FinalObservation struct{ condition FinalCondition }

func ObserveFinal(condition FinalCondition) (FinalObservation, error) {
	if !condition.valid() {
		return FinalObservation{}, ErrInvalidObservation
	}
	return FinalObservation{condition: condition}, nil
}

func (observation FinalObservation) Condition() FinalCondition { return observation.condition }
func (observation FinalObservation) valid() bool               { return observation.condition.valid() }

type FinalExpectation struct {
	object    transfer.OwnedObjectID
	exactSize uint64
}

func NewFinalExpectation(
	object transfer.OwnedObjectID,
	exactSize uint64,
) (FinalExpectation, error) {
	if object.IsZero() || exactSize > catalog.MaxFileSize {
		return FinalExpectation{}, ErrInvalidObservation
	}
	return FinalExpectation{object: object, exactSize: exactSize}, nil
}

func (expectation FinalExpectation) ObjectIdentity() transfer.OwnedObjectID {
	return expectation.object
}
func (expectation FinalExpectation) ExactSize() uint64 { return expectation.exactSize }

type RetirementStep uint8

const (
	RetirementRemoveStage RetirementStep = iota + 1
	RetirementSyncStageNamespace
	RetirementRemoveAnchor
	RetirementSyncAnchorNamespace
)

type FileDestination interface {
	Target() transfer.FileMaterializationTarget
	ObserveFinal(context.Context, FinalExpectation) (FinalObservation, error)
	ObserveFinalPresence(context.Context) (FinalObservation, error)
	PublishNoReplace(context.Context, OwnedFile, FinalExpectation) (FinalObservation, error)
	SyncFinalParent(context.Context) error
	Close() error
}

type OwnedFinalObserver interface {
	ObserveOwnedFinal(context.Context, OwnedFile, FinalExpectation) (FinalObservation, error)
}

type DirectoryAuthority interface {
	BindFile(
		context.Context,
		transfer.MaterializationFile,
		transfer.OutputDestinationPath,
	) (FileDestination, error)
}

type OwnedFile interface {
	ObjectID() checkpointmodel.ObjectID
	WriteAt([]byte, int64) (int, error)
	Sync() error
	SetModifiedTime(catalog.ModifiedTime) error
	MetadataMatches(uint64, catalog.ModifiedTime) (bool, error)
	Close() error
}

// LiveOwnedFile carries the already-journaled cleanup ticket beside the single
// public-profile data object. It implements the same transaction port as a
// resumable file but intentionally cannot manufacture durable ranges.
type LiveOwnedFile struct {
	object checkpointmodel.ObjectID
	file   outputcap.MutableFile
	ticket checkpointmodel.LiveCleanupTicket
}

func NewLiveOwnedFile(
	object checkpointmodel.ObjectID,
	file outputcap.MutableFile,
	ticket checkpointmodel.LiveCleanupTicket,
) (*LiveOwnedFile, error) {
	if object.IsZero() || file == nil || !ticket.Valid() ||
		ticket.State() != checkpointmodel.LiveCleanupStageCreated {
		return nil, ErrInvalidConfiguration
	}
	return &LiveOwnedFile{object: object, file: file, ticket: ticket}, nil
}

func (file *LiveOwnedFile) ObjectID() checkpointmodel.ObjectID {
	if file == nil {
		return checkpointmodel.ObjectID{}
	}
	return file.object
}
func (file *LiveOwnedFile) NativeFile() outputcap.MutableFile {
	if file == nil {
		return nil
	}
	return file.file
}
func (file *LiveOwnedFile) CleanupTicket() checkpointmodel.LiveCleanupTicket {
	if file == nil {
		return checkpointmodel.LiveCleanupTicket{}
	}
	return file.ticket
}
func (file *LiveOwnedFile) WriteAt(value []byte, offset int64) (int, error) {
	if file == nil || file.file == nil {
		return 0, outputcap.ErrUnsafeNamespace
	}
	return file.file.WriteAt(value, offset)
}
func (file *LiveOwnedFile) Sync() error {
	if file == nil || file.file == nil {
		return outputcap.ErrUnsafeNamespace
	}
	return file.file.Sync()
}
func (file *LiveOwnedFile) SetModifiedTime(modified catalog.ModifiedTime) error {
	if file == nil || file.file == nil {
		return outputcap.ErrUnsafeNamespace
	}
	return file.file.SetModifiedTime(modified)
}
func (file *LiveOwnedFile) MetadataMatches(size uint64, modified catalog.ModifiedTime) (bool, error) {
	if file == nil || file.file == nil {
		return false, outputcap.ErrUnsafeNamespace
	}
	return file.file.MetadataMatches(size, modified)
}
func (file *LiveOwnedFile) Close() error {
	if file == nil || file.file == nil {
		return nil
	}
	err := file.file.Close()
	file.file = nil
	return err
}

type Platform interface {
	ObserveOwnedObject(context.Context, checkpointmodel.ObjectID, uint64) (OwnedObservation, error)
	CreateOwnedFile(context.Context, FileDestination, checkpointmodel.ObjectID, uint64) (OwnedFile, OwnedObservation, error)
	OpenOwnedFile(context.Context, checkpointmodel.ObjectID, uint64, bool) (OwnedFile, OwnedObservation, error)
	ApplyRetirement(context.Context, checkpointmodel.ObjectID, RetirementStep) (OwnedObservation, error)
}

type CheckpointRepository interface {
	Lookup(context.Context, CheckpointKey) (checkpointmodel.Record, bool, error)
	Store(context.Context, *checkpointmodel.Record, checkpointmodel.Record) (CheckpointObservation, error)
}

// CheckpointAbandoner drops only the process-local lookup authority for an
// unusable record. Its durable image and any foreign partial stay untouched;
// a new checkpoint can then own the same authenticated file coordinate.
type CheckpointAbandoner interface {
	Abandon(context.Context, checkpointmodel.Record) error
}

type Config struct {
	Intent      transfer.ReceiveIntent
	Ownership   checkpointmodel.Ownership
	SessionID   transfer.OutputSessionID
	Directories DirectoryAuthority
	Platform    Platform
	Checkpoints CheckpointRepository
	Random      io.Reader
	Trace       TraceSink
}
