// Package fileexecution owns checkpoint-native execution for one path-local
// native output file. It deliberately has no selection or legacy session model:
// an outputsession FileClaim is the only runtime admission authority and a
// checkpointmodel Record is the only durable lifecycle authority.
package fileexecution

import (
	"context"
	"io"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

const MaximumObjectAllocationAttempts = 8

// CheckpointKey is the complete durable file identity except for the owned
// object, which is allocated only after absence at the public destination is
// proven. A repository must fail closed if more than one record matches a key.
type CheckpointKey struct {
	intent    transfer.TransferIntentDigest
	fileID    catalog.FileID
	revision  content.FileRevision
	path      string
	exactSize uint64
	backend   transfer.OutputBackendID
	root      checkpointmodel.RootIdentity
}

func (key CheckpointKey) TransferIntentDigest() transfer.TransferIntentDigest { return key.intent }
func (key CheckpointKey) FileID() catalog.FileID                              { return key.fileID }
func (key CheckpointKey) FileRevision() content.FileRevision                  { return key.revision }
func (key CheckpointKey) CanonicalPath() string                               { return key.path }
func (key CheckpointKey) ExactSize() uint64                                   { return key.exactSize }
func (key CheckpointKey) BackendID() transfer.OutputBackendID                 { return key.backend }
func (key CheckpointKey) RootIdentity() checkpointmodel.RootIdentity          { return key.root }

func (key CheckpointKey) valid() bool {
	return !key.intent.IsZero() && !key.fileID.IsZero() && !key.revision.IsZero() &&
		key.path != "" && key.exactSize <= catalog.MaxFileSize && key.backend != "" && !key.root.IsZero()
}

func (key CheckpointKey) matches(record checkpointmodel.Record) bool {
	return key.valid() && record.Valid() && record.TransferIntentDigest() == key.intent &&
		record.FileID() == key.fileID && record.FileRevision() == key.revision &&
		record.CanonicalPath() == key.path && record.ExactSize() == key.exactSize &&
		record.BackendID() == key.backend && record.RootIdentity() == key.root
}

// CheckpointObservation is a fresh, durable observation made after one exact
// create-or-replace attempt. It lets the pure reconciliation reducer distinguish
// unchanged, installed, and ambiguous cuts without interpreting error strings.
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

// OwnedObservation describes the exact stage/anchor relationship for one
// checkpoint object ID. Ready means both names identify the same exact-sized
// regular object. No other condition grants a writable handle.
type OwnedObservation struct {
	object    checkpointmodel.ObjectID
	condition OwnedCondition
}

func NewOwnedObservation(
	object checkpointmodel.ObjectID,
	condition OwnedCondition,
) (OwnedObservation, error) {
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

type FinalCondition uint8

const (
	FinalAbsent FinalCondition = iota + 1
	FinalCollision
	FinalOwnedExact
	FinalOwnedMetadataMismatch
	FinalUnsafe
)

func (condition FinalCondition) valid() bool {
	return condition >= FinalAbsent && condition <= FinalUnsafe
}

type FinalObservation struct {
	condition FinalCondition
}

func ObserveFinal(condition FinalCondition) (FinalObservation, error) {
	if !condition.valid() {
		return FinalObservation{}, ErrInvalidObservation
	}
	return FinalObservation{condition: condition}, nil
}

func (observation FinalObservation) Condition() FinalCondition { return observation.condition }
func (observation FinalObservation) valid() bool               { return observation.condition.valid() }

// FinalExpectation carries comparison facts, never placement authority. The
// claim-bound destination performs each actual observation under its own guard.
type FinalExpectation struct {
	object       transfer.OutputObjectIdentity
	exactSize    uint64
	modifiedTime catalog.ModifiedTime
}

func NewFinalExpectation(
	object transfer.OutputObjectIdentity,
	exactSize uint64,
	modifiedTime catalog.ModifiedTime,
) (FinalExpectation, error) {
	if object.IsZero() || exactSize > catalog.MaxFileSize {
		return FinalExpectation{}, ErrInvalidObservation
	}
	return FinalExpectation{object: object, exactSize: exactSize, modifiedTime: modifiedTime}, nil
}

func (expectation FinalExpectation) ObjectIdentity() transfer.OutputObjectIdentity {
	return expectation.object
}
func (expectation FinalExpectation) ExactSize() uint64 { return expectation.exactSize }
func (expectation FinalExpectation) ModifiedTime() catalog.ModifiedTime {
	return expectation.modifiedTime
}

type RetirementStep uint8

const (
	RetirementRemoveStage RetirementStep = iota + 1
	RetirementSyncStageNamespace
	RetirementRemoveAnchor
	RetirementSyncAnchorNamespace
)

// FileDestination is already bound to exactly one FileClaim. Implementations
// must reopen and revalidate retained ancestry for every public operation; this
// capability must never degrade into a pathname-only lookup.
type FileDestination interface {
	ClaimID() outputsession.ClaimID
	Target() transfer.OutputFileTarget
	ObserveFinal(context.Context, FinalExpectation) (FinalObservation, error)
	ObserveFinalPresence(context.Context) (FinalObservation, error)
	PublishNoReplace(context.Context, OwnedFile, FinalExpectation) (FinalObservation, error)
	SyncFinalParent(context.Context) error
	Close() error
}

type DirectoryAuthority interface {
	BindFile(context.Context, outputsession.FileClaim) (FileDestination, error)
}

// OwnedFile is the live stage capability. The platform that returns it has
// already proved that its private stage and anchor names identify this object.
type OwnedFile interface {
	ObjectID() checkpointmodel.ObjectID
	WriteAt([]byte, int64) (int, error)
	Sync() error
	SetModifiedTime(catalog.ModifiedTime) error
	MetadataMatches(uint64, catalog.ModifiedTime) (bool, error)
	Close() error
}

// Platform owns private stage/anchor mechanics. Retirement steps are separate
// so the engine can enforce stage -> sync -> anchor -> sync at every crash cut.
type Platform interface {
	CreateOwnedFile(context.Context, checkpointmodel.ObjectID, uint64) (OwnedFile, OwnedObservation, error)
	OpenOwnedFile(context.Context, checkpointmodel.ObjectID, uint64, bool) (OwnedFile, OwnedObservation, error)
	ApplyRetirement(context.Context, checkpointmodel.ObjectID, RetirementStep) (OwnedObservation, error)
}

// CheckpointRepository hides all native names, shards, and installation
// temporaries. Store is exact-create when previous is nil and exact-replace
// otherwise; it always returns a fresh durable observation of the target.
type CheckpointRepository interface {
	Lookup(context.Context, CheckpointKey) (checkpointmodel.Record, bool, error)
	Store(context.Context, *checkpointmodel.Record, checkpointmodel.Record) (CheckpointObservation, error)
}

type Config struct {
	Intent      transfer.TransferIntent
	Ownership   checkpointmodel.Ownership
	SessionID   transfer.OutputSessionID
	Directories DirectoryAuthority
	Platform    Platform
	Checkpoints CheckpointRepository
	Random      io.Reader
	Trace       TraceSink
}
