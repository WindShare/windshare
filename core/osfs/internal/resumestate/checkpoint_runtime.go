package resumestate

import (
	"fmt"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

// CheckpointRuntimeState is the process-local projection of FileCheckpointV1.
// It is intentionally not encoded: every durable transition is committed back
// to the checkpoint namespace before the output transaction exposes it.
type CheckpointRuntimeState = FileRecord

type BoundCheckpointRuntimeState = BoundFileRecord

type CheckpointRuntimeFile = ResumableFileAuthority

type CheckpointRuntimePhase = FilePhase

const (
	CheckpointRuntimeReserved       = FileReserved
	CheckpointRuntimeWitnessed      = FileWitnessed
	CheckpointRuntimePublishing     = FilePublishing
	CheckpointRuntimePublishBlocked = FilePublishBlocked
	CheckpointRuntimePublished      = FilePublished
	CheckpointRuntimeRetiring       = FileRetiring
	CheckpointRuntimeQuarantined    = FileQuarantined
)

// CheckpointRuntimeBinding is the process-local execution authority derived
// from the live FileCheckpointV1 namespace. It deliberately contains no
// selection-shaped header or legacy session namespace: the checkpoint remains
// the only durable authority and this value disappears when the process exits.
type CheckpointRuntimeBinding struct {
	SessionID    transfer.OutputSessionID
	IntentDigest transfer.TransferIntentDigest
	BackendID    transfer.OutputBackendID
	RootIdentity FileCheckpointRootID
}

func NewCheckpointRuntimeBinding(
	sessionID transfer.OutputSessionID,
	intentDigest transfer.TransferIntentDigest,
	backendID transfer.OutputBackendID,
	rootIdentity []byte,
) (CheckpointRuntimeBinding, error) {
	root, err := FileCheckpointRootIDFromBytes(rootIdentity)
	if err != nil || sessionID.IsZero() || intentDigest.IsZero() {
		return CheckpointRuntimeBinding{}, fmt.Errorf("%w: checkpoint runtime binding", ErrFileCheckpointBinding)
	}
	if _, err := transfer.NewOutputBackendID(string(backendID)); err != nil {
		return CheckpointRuntimeBinding{}, fmt.Errorf("%w: checkpoint runtime backend", ErrFileCheckpointBinding)
	}
	return CheckpointRuntimeBinding{
		SessionID: sessionID, IntentDigest: intentDigest, BackendID: backendID, RootIdentity: root,
	}, nil
}

func (binding CheckpointRuntimeBinding) valid() bool {
	if binding.SessionID.IsZero() || binding.IntentDigest.IsZero() || binding.RootIdentity.IsZero() {
		return false
	}
	_, err := transfer.NewOutputBackendID(string(binding.BackendID))
	return err == nil
}

func (binding CheckpointRuntimeBinding) validRecord(record FileRecord) bool {
	return binding.valid() && record.valid() && record.SessionID() == binding.SessionID
}

// NewCheckpointRuntimeFile creates the volatile native execution projection for
// a newly admitted file. The caller must install and verify FileCheckpointV1
// before exposing the returned transaction.
func NewCheckpointRuntimeFile(
	binding CheckpointRuntimeBinding,
	descriptor content.FileRevisionDescriptor,
	canonicalLocator string,
	outputObject OutputObjectID,
) (ResumableFileAuthority, error) {
	if !binding.valid() {
		return ResumableFileAuthority{}, fmt.Errorf("%w: checkpoint runtime binding", ErrInvalidState)
	}
	record, err := newFileRecordFromClaims(fileRecordClaims{
		sessionID: binding.SessionID, shareInstance: descriptor.ShareInstance(),
		fileID: descriptor.FileID(), revision: descriptor.FileRevision(),
		canonicalLocator: canonicalLocator, outputObject: outputObject,
		exactSize: descriptor.ExactSize(), chunkSize: descriptor.Geometry().ChunkSize(),
		stateGeneration: 1, phase: FileReserved,
		expectedMetadata: ExpectedMetadata{ModifiedTime: descriptor.ModifiedTime()},
	})
	if err != nil {
		return ResumableFileAuthority{}, err
	}
	bound, err := BindCheckpointRuntimeFile(binding, record)
	if err != nil {
		return ResumableFileAuthority{}, err
	}
	return BindResumableFile(bound, descriptor)
}

func BindCheckpointRuntimeFile(
	binding CheckpointRuntimeBinding,
	record FileRecord,
) (BoundFileRecord, error) {
	if !binding.validRecord(record) {
		return BoundFileRecord{}, fmt.Errorf("%w: checkpoint file runtime", ErrInvalidState)
	}
	return BoundFileRecord{checkpointRuntime: binding, record: record}, nil
}

func (bound BoundFileRecord) State() CheckpointRuntimeState { return bound.record }

func (authority ResumableFileAuthority) BoundState() BoundCheckpointRuntimeState {
	return authority.bound
}

func BindCheckpointRuntimeDescriptor(
	bound BoundCheckpointRuntimeState,
	descriptor content.FileRevisionDescriptor,
) (CheckpointRuntimeFile, error) {
	return BindResumableFile(bound, descriptor)
}

func PrepareCheckpointRuntimePublication(
	authority CheckpointRuntimeFile,
) (BoundCheckpointRuntimeState, error) {
	return PreparePublication(authority)
}

func PrepareCheckpointRuntimeIsolatedRetirement(
	bound BoundCheckpointRuntimeState,
) (BoundCheckpointRuntimeState, error) {
	return PrepareIsolatedRetirement(bound)
}

func PrepareCheckpointRuntimeInvalidatedRevisionRetirement(
	bound BoundCheckpointRuntimeState,
) (BoundCheckpointRuntimeState, error) {
	return PrepareInvalidatedRevisionRetirement(bound)
}

func PrepareCheckpointRuntimeUnsafeNamespaceQuarantine(
	bound BoundCheckpointRuntimeState,
	reason QuarantineReason,
) (BoundCheckpointRuntimeState, error) {
	return PrepareUnsafeNamespaceQuarantine(bound, reason)
}

func ApplyCheckpointRuntimeRecoveryDecision(
	bound BoundCheckpointRuntimeState,
	decision RecoveryDecision,
) (BoundCheckpointRuntimeState, error) {
	return ApplyRecoveryDecision(bound, decision)
}

func ReduceCheckpointRuntimeStateRecovery(
	bound BoundCheckpointRuntimeState,
	observation FileObservation,
) (RecoveryDecision, error) {
	return ReduceFileRecovery(bound, observation)
}

func ReduceCheckpointRuntimeFileRecovery(
	authority CheckpointRuntimeFile,
	observation FileObservation,
) (RecoveryDecision, error) {
	return ReduceResumableFileRecovery(authority, observation)
}

func ReduceCheckpointRuntimePublishResult(
	bound BoundCheckpointRuntimeState,
	result PublishResult,
) (RecoveryDecision, error) {
	return ReducePublishResult(bound, result)
}

// RestoreCheckpointRuntimeFile reconstructs only volatile execution state. All
// persistent identity, object, range, and lifecycle claims are validated against
// FileCheckpointV1 before the projection is returned.
func RestoreCheckpointRuntimeFile(
	binding CheckpointRuntimeBinding,
	descriptor content.FileRevisionDescriptor,
	checkpoint FileCheckpointV1,
) (ResumableFileAuthority, error) {
	if !binding.valid() {
		return ResumableFileAuthority{}, fmt.Errorf("%w: checkpoint runtime binding", ErrInvalidState)
	}
	if err := checkpoint.valid(); err != nil {
		return ResumableFileAuthority{}, err
	}
	if checkpoint.rootIdentity != binding.RootIdentity || checkpoint.backendID != binding.BackendID ||
		checkpoint.intentDigest != binding.IntentDigest || descriptor.FileID() != checkpoint.fileID ||
		descriptor.FileRevision() != checkpoint.fileRevision || descriptor.ExactSize() != checkpoint.exactSize {
		return ResumableFileAuthority{}, fmt.Errorf("%w: checkpoint runtime claims", ErrFileCheckpointBinding)
	}
	object, err := OutputObjectIDFromBytes(checkpoint.ownedOutputObject.Bytes())
	if err != nil {
		return ResumableFileAuthority{}, err
	}
	ranges := make([]content.Range, len(checkpoint.verifiedRanges))
	for index, current := range checkpoint.verifiedRanges {
		ranges[index] = content.Range{Offset: current.Offset, End: current.End}
	}
	durable, err := content.NewRangeSet(ranges)
	if err != nil {
		return ResumableFileAuthority{}, fmt.Errorf("%w: checkpoint durable ranges", ErrFileCheckpointBinding)
	}
	phase, err := restoredFilePhase(checkpoint)
	if err != nil {
		return ResumableFileAuthority{}, err
	}
	record, err := newFileRecordFromClaims(fileRecordClaims{
		sessionID: binding.SessionID, shareInstance: descriptor.ShareInstance(),
		fileID: descriptor.FileID(), revision: descriptor.FileRevision(),
		canonicalLocator: checkpoint.canonicalPath, outputObject: object,
		exactSize: descriptor.ExactSize(), chunkSize: descriptor.Geometry().ChunkSize(),
		stateGeneration: checkpoint.stateGeneration, checkpointGeneration: checkpoint.checkpointGeneration,
		durableRanges: durable, phase: phase,
		quarantineReason:      checkpoint.quarantineReason,
		phaseBeforeQuarantine: checkpoint.phaseBeforeQuarantine,
		expectedMetadata:      ExpectedMetadata{ModifiedTime: descriptor.ModifiedTime()},
		retirementReason:      checkpoint.retirementReason,
	})
	if err != nil {
		return ResumableFileAuthority{}, fmt.Errorf("%w: rebuild checkpoint runtime: %w", ErrFileCheckpointBinding, err)
	}
	bound, err := BindCheckpointRuntimeFile(binding, record)
	if err != nil {
		return ResumableFileAuthority{}, err
	}
	return BindResumableFile(bound, descriptor)
}
