// Package fileexecution owns one native DirectTree file transaction. A
// FileCheckpointV2 is the only source of verified durable ranges.
package fileexecution

import (
	"context"
	"io"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
)

const MaximumObjectAllocationAttempts = 8

type CheckpointKey struct {
	operation       receivecontract.OperationID
	intent          transfer.ReceiveIntentDigest
	materialization receivecontract.BindingDigest
	fileID          catalog.FileID
	revision        content.FileRevision
	metadata        content.SourceMetadata
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

func (key CheckpointKey) CheckpointLineageSpec() (checkpointmodel.CheckpointLineageSpec, error) {
	if !key.valid() {
		return checkpointmodel.CheckpointLineageSpec{}, ErrInvalidClaim
	}
	return checkpointmodel.CheckpointLineageSpec{
		OperationID: key.operation, ReceiveIntentDigest: key.intent,
		MaterializationBindingDigest: key.materialization,
		FileID:                       key.fileID, CanonicalPath: key.path, MaterializerKind: key.materializer,
		AuthorityRef: key.authority,
	}, nil
}

func (key CheckpointKey) CheckpointLineageRequest() checkpointmodel.CheckpointLineageRequest {
	return checkpointmodel.CheckpointLineageRequest{
		FileRevision: key.revision, ExactSize: key.exactSize, SourceMetadata: key.metadata,
	}
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

// CheckpointResolution carries the shared closed decision while exposing a
// accepted record only for exact/reusable progress. Conflicts retain candidate
// evidence without granting receiving authority.
type CheckpointResolution struct {
	assessment checkpointmodel.CheckpointLineageAssessment
	record     checkpointmodel.Record
}

func ResolveCheckpoint(
	assessment checkpointmodel.CheckpointLineageAssessment,
	record checkpointmodel.Record,
) (CheckpointResolution, error) {
	resolution := CheckpointResolution{assessment: assessment, record: record}
	if !resolution.valid() {
		return CheckpointResolution{}, ErrInvalidObservation
	}
	return resolution, nil
}

func (resolution CheckpointResolution) Decision() checkpointmodel.CheckpointLineageDecision {
	return resolution.assessment.Decision
}

func (resolution CheckpointResolution) Record() (checkpointmodel.Record, bool) {
	return resolution.record, resolution.Decision().SelectsRecord() && resolution.record.Valid()
}

func (resolution CheckpointResolution) Candidate() (checkpointmodel.Record, bool) {
	return resolution.record, resolution.assessment.RetainsSourceEvidence() && resolution.record.Valid()
}

func (resolution CheckpointResolution) valid() bool {
	return resolution.assessment.Valid() && (resolution.assessment.RetainsSourceEvidence() ||
		resolution.Decision().SelectsRecord() == resolution.record.Valid())
}

type InitialCheckpointObservation struct {
	resolution CheckpointResolution
	installed  bool
}

func ObserveInitialCheckpoint(
	resolution CheckpointResolution,
	installed bool,
) (InitialCheckpointObservation, error) {
	if !resolution.valid() || installed && resolution.Decision() != checkpointmodel.CheckpointLineageDecisionExact {
		return InitialCheckpointObservation{}, ErrInvalidObservation
	}
	return InitialCheckpointObservation{resolution: resolution, installed: installed}, nil
}

func (observation InitialCheckpointObservation) Resolution() CheckpointResolution {
	return observation.resolution
}

func (observation InitialCheckpointObservation) Installed() bool { return observation.installed }

func (observation InitialCheckpointObservation) valid() bool {
	return observation.resolution.valid() &&
		(!observation.installed || observation.resolution.Decision() == checkpointmodel.CheckpointLineageDecisionExact)
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
	// Object identity and length are not authentication of current bytes.
	FinalMatchesPublication
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
	WithPublication(context.Context, OwnedFile, FinalExpectation, func(FilePublication) error) error
	Close() error
}

// FilePublication is borrowed for one synchronous local namespace transaction.
// Its parent placement and owned-object binding remain valid until the callback
// returns. Network work and checkpoint persistence must stay outside this scope.
type FilePublication interface {
	Observe(context.Context) (FinalObservation, error)
	PublishNoReplace(context.Context) (FinalObservation, error)
	Sync(context.Context) error
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

type Platform interface {
	ObserveOwnedObject(context.Context, checkpointmodel.ObjectID, uint64) (OwnedObservation, error)
	CreateOwnedFile(context.Context, FileDestination, checkpointmodel.ObjectID, uint64) (OwnedFile, OwnedObservation, error)
	OpenOwnedFile(context.Context, checkpointmodel.ObjectID, uint64, bool) (OwnedFile, OwnedObservation, error)
	ApplyRetirement(context.Context, checkpointmodel.ObjectID, RetirementStep) (OwnedObservation, error)
}

type CheckpointRepository interface {
	Lookup(context.Context, CheckpointKey) (CheckpointResolution, error)
	// A nil InstallInitial error is the durability proof that permits owned-object
	// creation. A readable image accompanied by an install error is ambiguity,
	// not success, and must keep Installed false.
	InstallInitial(context.Context, CheckpointKey, checkpointmodel.Record) (InitialCheckpointObservation, error)
	// Replace must preserve every physical install error even when the requested
	// image can be reread; callers cannot advance ranges from readability alone.
	Replace(context.Context, checkpointmodel.Record, checkpointmodel.Record) (CheckpointObservation, error)
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
	Clock       func() time.Time
	AfterFunc   func(time.Duration, func()) CheckpointTimer
}
