package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func (session *Session) beginFileFromIncrementalCheckpoint(
	ctx context.Context,
	file transfer.OutputFile,
	checkpoint resumestate.FileCheckpointV1,
) (resultStart transfer.FileStart, resultErr error) {
	resumable, err := resumestate.RestoreCheckpointRuntimeFile(
		session.checkpointRuntime, file.Descriptor, checkpoint,
	)
	if err != nil {
		return transfer.FileStart{}, pauseRequiredFileOutputFault(fileOutputFault("restore FileCheckpointV1", err))
	}
	return session.reduceFile(ctx, file, resumable)
}

// ensureIncrementalCheckpoint installs the write-ahead V1 identity as soon as a
// dynamic file transaction owns its stage/anchor pair. The transaction record
// remains a live implementation detail of the native adapter; only the
// authenticated V1 image written here can reconstruct this overlay after a
// restart.
func (session *Session) ensureIncrementalCheckpoint(
	descriptor content.FileRevisionDescriptor,
	record resumestate.CheckpointRuntimeState,
) (resumestate.FileCheckpointV1, error) {
	if session == nil {
		return resumestate.FileCheckpointV1{}, transfer.ErrInvalidOutputBinding
	}
	session.stateInstall.Lock()
	defer session.stateInstall.Unlock()
	if !session.incrementalAdmission || session.incrementalIntentDigest.IsZero() {
		return resumestate.FileCheckpointV1{}, nil
	}
	if session.stateWritesDisabled() {
		return resumestate.FileCheckpointV1{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed,
		)
	}
	live, found := session.incrementalFiles[record.CanonicalLocator()]
	if !found || live.Revision != descriptor.FileRevision() ||
		live.Selection.FileID != descriptor.FileID() || live.Selection.ExpectedSize != descriptor.ExactSize() {
		return resumestate.FileCheckpointV1{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSelection,
		)
	}
	key, err := live.Key()
	if err != nil {
		return resumestate.FileCheckpointV1{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if existing, ok := session.incrementalCheckpointByPath[record.CanonicalLocator()]; ok {
		if existing.TransferIntentDigest() != session.incrementalIntentDigest ||
			existing.FileRevision() != descriptor.FileRevision() ||
			existing.ExactSize() != descriptor.ExactSize() {
			return resumestate.FileCheckpointV1{}, outputfault.New(
				transfer.OutputFaultFile, transfer.OutputFaultStateCorrupt, resumestate.ErrFileCheckpointBinding,
			)
		}
		return existing, nil
	}
	if existing, ok := session.incrementalCheckpoints[key]; ok {
		if existing.TransferIntentDigest() != session.incrementalIntentDigest ||
			existing.FileRevision() != descriptor.FileRevision() || existing.ExactSize() != descriptor.ExactSize() {
			return resumestate.FileCheckpointV1{}, outputfault.New(
				transfer.OutputFaultFile, transfer.OutputFaultStateCorrupt, resumestate.ErrFileCheckpointBinding,
			)
		}
		return existing, nil
	}
	root := session.checkpointRuntime.RootIdentity
	if root.IsZero() {
		return resumestate.FileCheckpointV1{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, resumestate.ErrFileCheckpointBinding,
		)
	}
	checkpoint, err := resumestate.NewFileCheckpointV1(resumestate.FileCheckpointSpec{
		TransferIntentDigest:  session.incrementalIntentDigest,
		FileID:                descriptor.FileID(),
		FileRevision:          descriptor.FileRevision(),
		CanonicalPath:         record.CanonicalLocator(),
		ExactSize:             descriptor.ExactSize(),
		BackendID:             string(filesystemOutputBackendID),
		RootIdentity:          root.Bytes(),
		OwnedOutputObject:     record.OutputObject().Bytes(),
		StateGeneration:       record.StateGeneration(),
		CheckpointGeneration:  record.CheckpointGeneration(),
		VerifiedRanges:        checkpointRangesFromContent(record.DurableRanges()),
		Phase:                 checkpointPhaseForFile(record.Phase()),
		CommitState:           resumestate.FileCheckpointCommitCandidate,
		QuarantineReason:      record.QuarantineReason(),
		PhaseBeforeQuarantine: record.PhaseBeforeQuarantine(),
		RetirementReason:      record.RetirementReason(),
	})
	if err != nil {
		return resumestate.FileCheckpointV1{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.persistIncrementalCheckpointLocked(resumestate.FileCheckpointV1{}, checkpoint); err != nil {
		return resumestate.FileCheckpointV1{}, err
	}
	if session.incrementalCheckpoints == nil {
		session.incrementalCheckpoints = make(map[resumestate.LiveFileKey]resumestate.FileCheckpointV1)
	}
	session.incrementalCheckpoints[key] = checkpoint
	return checkpoint, nil
}

// verifyInitialIncrementalCheckpoint commits the empty durable-range baseline
// before BeginFile exposes a transaction. Without this cut, an interruption
// before the first non-empty checkpoint would leave only a write-ahead candidate
// that intentionally cannot authorize restart recovery.
func (session *Session) verifyInitialIncrementalCheckpoint(
	record resumestate.CheckpointRuntimeState,
) error {
	if session == nil || !session.incrementalAdmission {
		return nil
	}
	session.stateInstall.Lock()
	defer session.stateInstall.Unlock()
	previous, found := session.incrementalCheckpointByPath[record.CanonicalLocator()]
	if !found {
		return fmt.Errorf("%w: missing initial checkpoint", resumestate.ErrFileCheckpointRecovery)
	}
	if previous.CommitState() == resumestate.FileCheckpointCommitVerified ||
		previous.CommitState() == resumestate.FileCheckpointCommitPublished {
		return nil
	}
	verified, err := resumestate.PromoteCheckpoint(
		previous, checkpointPhaseForFile(record.Phase()), resumestate.FileCheckpointCommitVerified,
	)
	if err != nil {
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.persistIncrementalCheckpointLocked(previous, verified); err != nil {
		return err
	}
	for key := range session.incrementalCheckpoints {
		if key.IntentDigest == verified.TransferIntentDigest() && key.FileID == verified.FileID() &&
			key.Revision == verified.FileRevision() && key.CanonicalPath == verified.CanonicalPath() &&
			key.ExactSize == verified.ExactSize() {
			session.incrementalCheckpoints[key] = verified
		}
	}
	return nil
}

// advanceIncrementalCheckpoint commits the volatile execution transition into
// the V1 file-local authority. The candidate is promoted only after the caller has
// completed its data/witness verification, so an in-memory crash cut cannot grant
// ranges that were never returned as VerifiedDurableRanges.
func (session *Session) advanceIncrementalCheckpoint(
	record resumestate.CheckpointRuntimeState,
	descriptor content.FileRevisionDescriptor,
	ranges content.RangeSet,
) (resumestate.FileCheckpointV1, error) {
	if session == nil || !session.incrementalAdmission {
		return resumestate.FileCheckpointV1{}, nil
	}
	if _, err := session.ensureIncrementalCheckpoint(descriptor, record); err != nil {
		return resumestate.FileCheckpointV1{}, err
	}
	session.stateInstall.Lock()
	defer session.stateInstall.Unlock()
	live, found := session.incrementalFiles[record.CanonicalLocator()]
	if !found {
		return resumestate.FileCheckpointV1{}, transfer.ErrInvalidOutputSelection
	}
	key, err := live.Key()
	if err != nil {
		return resumestate.FileCheckpointV1{}, err
	}
	previous, found := session.incrementalCheckpointByPath[record.CanonicalLocator()]
	if !found {
		previous, found = session.incrementalCheckpoints[key]
	}
	if !found {
		return resumestate.FileCheckpointV1{}, fmt.Errorf("%w: missing incremental checkpoint", resumestate.ErrFileCheckpointRecovery)
	}
	next, err := resumestate.NewFileCheckpointV1(resumestate.FileCheckpointSpec{
		OwnershipMarker:       previous.OwnershipMarker(),
		Namespace:             previous.Namespace(),
		TransferIntentDigest:  previous.TransferIntentDigest(),
		FileID:                previous.FileID(),
		FileRevision:          previous.FileRevision(),
		CanonicalPath:         previous.CanonicalPath(),
		ExactSize:             previous.ExactSize(),
		BackendID:             string(previous.BackendID()),
		RootIdentity:          previous.RootIdentity().Bytes(),
		OwnedOutputObject:     previous.OwnedOutputObject().Bytes(),
		StateGeneration:       record.StateGeneration(),
		CheckpointGeneration:  previous.CheckpointGeneration() + 1,
		VerifiedRanges:        checkpointRangesFromContent(ranges),
		Phase:                 checkpointPhaseForFile(record.Phase()),
		CommitState:           resumestate.FileCheckpointCommitCandidate,
		QuarantineReason:      record.QuarantineReason(),
		PhaseBeforeQuarantine: record.PhaseBeforeQuarantine(),
		RetirementReason:      record.RetirementReason(),
	})
	if err != nil {
		return resumestate.FileCheckpointV1{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := resumestate.ValidateCheckpointTransition(previous, next); err != nil {
		return resumestate.FileCheckpointV1{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	verified, err := resumestate.PromoteCheckpoint(
		next, checkpointPhaseForFile(record.Phase()), resumestate.FileCheckpointCommitVerified,
	)
	if err != nil {
		return resumestate.FileCheckpointV1{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.persistIncrementalCheckpointLocked(previous, verified); err != nil {
		return resumestate.FileCheckpointV1{}, err
	}
	for existingKey := range session.incrementalCheckpoints {
		if existingKey.IntentDigest == key.IntentDigest && existingKey.FileID == key.FileID &&
			existingKey.Revision == key.Revision && existingKey.CanonicalPath == key.CanonicalPath &&
			existingKey.ExactSize == key.ExactSize && existingKey != key {
			delete(session.incrementalCheckpoints, existingKey)
		}
	}
	session.incrementalCheckpoints[key] = verified
	return verified, nil
}

func (session *Session) syncIncrementalCheckpointState(record resumestate.CheckpointRuntimeState) error {
	if session == nil || !session.incrementalAdmission {
		return nil
	}
	session.stateInstall.Lock()
	defer session.stateInstall.Unlock()
	previous, found := session.incrementalCheckpointByPath[record.CanonicalLocator()]
	if !found || previous.CommitState() == resumestate.FileCheckpointCommitCandidate ||
		previous.CommitState() == resumestate.FileCheckpointCommitPublished ||
		previous.CommitState() == resumestate.FileCheckpointCommitQuarantined ||
		record.CheckpointGeneration() != previous.CheckpointGeneration() ||
		record.StateGeneration() <= previous.StateGeneration() {
		// A data-generation transition is committed by advanceIncrementalCheckpoint
		// after data and witness verification. A lifecycle commit must never race
		// ahead and accidentally grant those bytes.
		return nil
	}
	commitState := resumestate.FileCheckpointCommitVerified
	switch record.Phase() {
	case resumestate.CheckpointRuntimePublished:
		commitState = resumestate.FileCheckpointCommitPublished
	case resumestate.CheckpointRuntimeQuarantined:
		commitState = resumestate.FileCheckpointCommitQuarantined
	}
	next, err := resumestate.AdvanceCheckpointState(
		previous, record.StateGeneration(), checkpointPhaseForFile(record.Phase()), commitState,
		record.QuarantineReason(), record.PhaseBeforeQuarantine(), record.RetirementReason(),
	)
	if err != nil {
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.persistIncrementalCheckpointLocked(previous, next); err != nil {
		return err
	}
	for key := range session.incrementalCheckpoints {
		if key.IntentDigest == next.TransferIntentDigest() && key.FileID == next.FileID() &&
			key.Revision == next.FileRevision() && key.CanonicalPath == next.CanonicalPath() &&
			key.ExactSize == next.ExactSize() {
			session.incrementalCheckpoints[key] = next
		}
	}
	return nil
}

func checkpointRangesFromContent(ranges content.RangeSet) []resumestate.FileCheckpointRange {
	owned := ranges.Ranges()
	converted := make([]resumestate.FileCheckpointRange, len(owned))
	for index, current := range owned {
		converted[index] = resumestate.FileCheckpointRange{Offset: current.Offset, End: current.End}
	}
	return converted
}

func checkpointPhaseForFile(phase resumestate.CheckpointRuntimePhase) resumestate.FileCheckpointPhase {
	switch phase {
	case resumestate.CheckpointRuntimeReserved:
		return resumestate.FileCheckpointPhaseReserved
	case resumestate.CheckpointRuntimeWitnessed:
		return resumestate.FileCheckpointPhaseActive
	case resumestate.CheckpointRuntimePublishing:
		return resumestate.FileCheckpointPhasePublishing
	case resumestate.CheckpointRuntimePublishBlocked:
		return resumestate.FileCheckpointPhasePaused
	case resumestate.CheckpointRuntimePublished:
		return resumestate.FileCheckpointPhasePublished
	case resumestate.CheckpointRuntimeRetiring:
		return resumestate.FileCheckpointPhaseRetired
	case resumestate.CheckpointRuntimeQuarantined:
		return resumestate.FileCheckpointPhaseQuarantined
	default:
		return resumestate.FileCheckpointPhaseQuarantined
	}
}

func checkpointOutputFault(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultStateIO,
		fmt.Errorf("%s: %w", operation, cause))
}

func (session *Session) persistIncrementalCheckpointLocked(
	previous resumestate.FileCheckpointV1,
	next resumestate.FileCheckpointV1,
) error {
	if session == nil || session.checkpointsDir == nil || next.RecordID().IsZero() {
		return checkpointOutputFault("persist FileCheckpointV1", transfer.ErrInvalidOutputBinding)
	}
	encoded, err := resumestate.EncodeFileCheckpointV1(next)
	if err != nil {
		return checkpointOutputFault("encode FileCheckpointV1", err)
	}
	shard, name, err := checkpointstore.FileFor(session.checkpointsDir, next.RecordID(), true)
	if err != nil {
		return checkpointOutputFault("open FileCheckpointV1 shard", err)
	}
	defer shard.Close()
	if previous.RecordID().IsZero() {
		err = checkpointstore.InstallCreate(shard, name, encoded)
	} else {
		if previous.RecordID() != next.RecordID() {
			return checkpointOutputFault("replace FileCheckpointV1", resumestate.ErrFileCheckpointBinding)
		}
		previousEncoded, encodeErr := resumestate.EncodeFileCheckpointV1(previous)
		if encodeErr != nil {
			return checkpointOutputFault("encode previous FileCheckpointV1", encodeErr)
		}
		err = checkpointstore.InstallReplace(shard, name, previousEncoded, encoded)
	}
	if err != nil {
		return checkpointOutputFault("install FileCheckpointV1", err)
	}
	if session.incrementalCheckpointByPath == nil {
		session.incrementalCheckpointByPath = make(map[string]resumestate.FileCheckpointV1)
	}
	session.incrementalCheckpointByPath[next.CanonicalPath()] = next
	return nil
}

func (session *Session) removeIncrementalCheckpoint(record resumestate.CheckpointRuntimeState) (error, error) {
	if session == nil || session.checkpointsDir == nil || !session.incrementalAdmission {
		return transfer.ErrInvalidOutputBinding, nil
	}
	session.stateInstall.Lock()
	defer session.stateInstall.Unlock()
	checkpoint, found := session.incrementalCheckpointByPath[record.CanonicalLocator()]
	if !found || checkpoint.RecordID().IsZero() ||
		checkpoint.CommitState() != resumestate.FileCheckpointCommitVerified ||
		checkpoint.Phase() != resumestate.FileCheckpointPhaseRetired ||
		!bytes.Equal(checkpoint.OwnedOutputObject().Bytes(), record.OutputObject().Bytes()) {
		return resumestate.ErrFileCheckpointBinding, nil
	}
	expected, err := resumestate.EncodeFileCheckpointV1(checkpoint)
	if err != nil {
		return err, nil
	}
	shard, name, err := checkpointstore.FileFor(session.checkpointsDir, checkpoint.RecordID(), false)
	if err != nil {
		return err, closeOutputDirectory(shard)
	}
	operationErr, cleanupErr := checkpointstore.RemoveExact(shard, name, expected)
	cleanupErr = errors.Join(cleanupErr, shard.Close())
	if operationErr != nil {
		return operationErr, cleanupErr
	}
	delete(session.incrementalCheckpointByPath, checkpoint.CanonicalPath())
	for key, current := range session.incrementalCheckpoints {
		if current.RecordID() == checkpoint.RecordID() {
			delete(session.incrementalCheckpoints, key)
		}
	}
	return nil, cleanupErr
}

func (session *Session) incrementalCheckpointFor(live resumestate.LiveFileSelection) (resumestate.FileCheckpointV1, bool) {
	if session == nil {
		return resumestate.FileCheckpointV1{}, false
	}
	session.stateInstall.RLock()
	defer session.stateInstall.RUnlock()
	checkpoint, found := session.incrementalCheckpointByPath[live.Selection.Path]
	if !found {
		key, err := live.Key()
		if err == nil {
			checkpoint, found = session.incrementalCheckpoints[key]
		}
	}
	return checkpoint, found && checkpoint.TransferIntentDigest() == session.incrementalIntentDigest &&
		checkpoint.FileID() == live.Selection.FileID && checkpoint.FileRevision() == live.Revision &&
		checkpoint.CanonicalPath() == live.Selection.Path && checkpoint.ExactSize() == live.Selection.ExpectedSize &&
		checkpoint.BackendID() == filesystemOutputBackendID &&
		checkpoint.CommitState() != resumestate.FileCheckpointCommitCandidate
}
