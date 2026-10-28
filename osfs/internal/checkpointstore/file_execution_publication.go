package checkpointstore

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/fileexecution"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func (store *FileExecutionStore) ApplyRetirement(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	step fileexecution.RetirementStep,
) (observation fileexecution.OwnedObservation, resultErr error) {
	defer func() {
		resultErr = fileOutputBoundaryError(ctx, transferfault.ScopeFileLocal, resultErr)
	}()
	if store == nil || ctx == nil || object.IsZero() ||
		step < fileexecution.RetirementRemoveStage || step > fileexecution.RetirementSyncAnchorNamespace {
		return fileexecution.OwnedObservation{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return fileexecution.OwnedObservation{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.applyRetirementLocked(object, step)
}

func (store *FileExecutionStore) applyRetirementLocked(
	object checkpointmodel.ObjectID,
	step fileexecution.RetirementStep,
) (fileexecution.OwnedObservation, error) {
	var operationErr error
	switch step {
	case fileexecution.RetirementRemoveStage:
		operationErr = store.removeOwnedEntry(store.repository.stages, object, ownedStageSuffix)
	case fileexecution.RetirementSyncStageNamespace:
		operationErr = syncOwnedEntryNamespace(store.repository.stages, object)
	case fileexecution.RetirementRemoveAnchor:
		operationErr = store.removeOwnedEntry(store.repository.anchors, object, ownedAnchorSuffix)
	case fileexecution.RetirementSyncAnchorNamespace:
		operationErr = syncOwnedEntryNamespace(store.repository.anchors, object)
	default:
		return fileexecution.OwnedObservation{}, transfer.ErrInvalidOutputBinding
	}
	files, observation, observeErr := store.openObservedOwnedLocked(object, 0, false)
	return observation, errors.Join(operationErr, observeErr, files.close())
}

type candidateDurabilityFiles struct {
	stage           outputcap.RecoveryDurabilityFile
	anchor          outputcap.ObservedFile
	stageNamespace  outputcap.Directory
	anchorNamespace outputcap.Directory
}

func (files *candidateDurabilityFiles) close() error {
	if files == nil {
		return nil
	}
	return errors.Join(
		closeFile(files.stage), closeFile(files.anchor),
		closeDirectory(files.stageNamespace), closeDirectory(files.anchorNamespace),
	)
}

func (store *FileExecutionStore) openCandidateDurabilityLocked(
	record checkpointmodel.Record,
) (*candidateDurabilityFiles, fileexecution.OwnedObservation, error) {
	privateProfile := store.stagePolicy == PrivateStage
	stage := openRecoveryOwnedDataFile(
		store.repository.stages, record.OwnedObjectID(), ownedStageSuffix, privateProfile,
	)
	anchor := observeOwnedDataFile(
		store.repository.anchors, record.OwnedObjectID(), ownedAnchorSuffix, privateProfile,
	)
	condition, comparisonErr := classifyOwnedObservation(stage, anchor, record.ExactSize(), true)
	observation, observationErr := fileexecution.NewOwnedObservation(record.OwnedObjectID(), condition)
	recoveryStage, stageOK := stage.file.(outputcap.RecoveryDurabilityFile)
	observedAnchor, anchorOK := anchor.file.(outputcap.ObservedFile)
	files := &candidateDurabilityFiles{
		stage: recoveryStage, anchor: observedAnchor,
		stageNamespace: stage.directory, anchorNamespace: anchor.directory,
	}
	if condition != fileexecution.OwnedReady || !stageOK || !anchorOK {
		return nil, observation, errors.Join(
			stageObservationError(stage), anchorObservationError(anchor), comparisonErr, observationErr,
			closeFile(stage.file), closeFile(anchor.file),
			closeDirectory(stage.directory), closeDirectory(anchor.directory),
		)
	}
	return files, observation, errors.Join(comparisonErr, observationErr)
}

func (store *FileExecutionStore) candidateDurable(record checkpointmodel.Record) (bool, error) {
	if !record.Valid() || record.CommitState() != checkpointmodel.CommitCandidate {
		return false, reconciliationError(
			ReconciliationCandidateObservation, checkpointmodel.ErrRecordBinding,
		)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	files, observation, err := store.openCandidateDurabilityLocked(record)
	if checkpointmodel.InitialCandidate(record) && observation.Condition() == fileexecution.OwnedAbsent {
		// Candidate installation deliberately precedes owned creation. Exact
		// absence retains this recorded object as the sole lineage authority.
		return false, nil
	}
	if err != nil || observation.Condition() != fileexecution.OwnedReady || files == nil {
		if err == nil {
			err = checkpointmodel.ErrRecordBinding
		}
		return false, reconciliationError(ReconciliationCandidateObservation, err)
	}
	if err := files.stage.Sync(); err != nil {
		primary := reconciliationError(ReconciliationStageDurability, err)
		return false, errors.Join(primary, files.close())
	}
	if err := files.stageNamespace.Sync(); err != nil {
		primary := reconciliationError(ReconciliationNamespaceDurability, err)
		return false, errors.Join(primary, files.close())
	}
	if err := files.anchorNamespace.Sync(); err != nil {
		primary := reconciliationError(ReconciliationNamespaceDurability, err)
		return false, errors.Join(primary, files.close())
	}
	if closeErr := files.close(); closeErr != nil {
		return false, reconciliationError(ReconciliationCandidateObservation, closeErr)
	}
	return true, nil
}

// FinalMatchesOwned reconciles interrupted publication using the retained object
// witness. A durable delivery receipt no longer depends on this comparison.
func (store *FileExecutionStore) FinalMatchesOwned(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
	final outputcap.FileIdentity,
) (matches bool, resultErr error) {
	defer func() {
		resultErr = fileOutputBoundaryError(ctx, transferfault.ScopeFileLocal, resultErr)
	}()
	if store == nil || ctx == nil || object.IsZero() || final == nil {
		return false, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	anchor := observeOwnedDataFile(
		store.repository.anchors, object, ownedAnchorSuffix, store.stagePolicy == PrivateStage,
	)
	if anchor.state != privateEntryReady || anchor.file == nil {
		return false, errors.Join(
			anchorObservationError(anchor), closeFile(anchor.file), closeDirectory(anchor.directory),
		)
	}
	same, compareErr := sameExactOwnedFiles(final, anchor.file, exactSize)
	return same, errors.Join(compareErr, anchor.file.Close(), anchor.directory.Close())
}

// PublishOwnedNoReplace exposes the same data object through a move or a native
// hard link. The caller still reconciles placement before recording delivery.
func (store *FileExecutionStore) PublishOwnedNoReplace(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
	parent outputcap.Directory,
	name string,
) (linked outputcap.ObservedFile, resultErr error) {
	defer func() {
		resultErr = fileOutputBoundaryError(ctx, transferfault.ScopeFileLocal, resultErr)
	}()
	if store == nil || ctx == nil || object.IsZero() || parent == nil || name == "" {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	files, observation, err := store.openObservedOwnedLocked(object, exactSize, true)
	if err != nil || observation.Condition() != fileexecution.OwnedReady || files.anchor == nil {
		return nil, errors.Join(err, files.close(), outputcap.ErrUnsafeNamespace)
	}
	var linkedFile outputcap.ObservedFile
	var linkErr error
	if _, portable := files.stage.(recoveryMetadataSource); portable {
		linkedFile, linkErr = publishRecoveryStage(parent, files.stage, name)
	} else {
		linkedFile, linkErr = parent.LinkFileNoReplace(files.anchor, name)
	}
	return linkedFile, errors.Join(linkErr, files.close())
}

func (store *FileExecutionStore) removeOwnedEntry(root outputcap.Directory, object checkpointmodel.ObjectID, suffix string) error {
	namespace := newCleanupNamespace(root)
	return errors.Join(store.removeOwned(namespace, object, suffix), namespace.close())
}

func syncOwnedEntryNamespace(root outputcap.Directory, object checkpointmodel.ObjectID) error {
	shard, _ := ownedObjectLocation(object, ownedStageSuffix)
	directory, err := OpenShard(root, shard, false)
	if errors.Is(err, fs.ErrNotExist) {
		return root.Sync()
	}
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

func ownedObjectLocation(object checkpointmodel.ObjectID, suffix string) (string, string) {
	encoded := hex.EncodeToString(object.Bytes())
	return encoded[:recordShardLength], encoded + suffix
}
