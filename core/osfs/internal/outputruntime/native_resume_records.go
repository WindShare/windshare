package outputruntime

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

const nativeResumeDirectoryPrefix = "directory-"

func (lease *NativeResumeLease) reconciledRecordsLocked(
	ctx context.Context,
	lifecycle checkpointmodel.ReceiveLifecycleState,
) (*checkpointstore.FileExecutionStore, []checkpointmodel.Record, []checkpointmodel.AdmittedDirectory, error) {
	if err := lease.validateLocked(ctx); err != nil {
		return nil, nil, nil, err
	}
	store, err := checkpointstore.NewFileExecutionStore(lease.repository)
	if err != nil {
		return nil, nil, nil, err
	}
	snapshot, err := lease.repository.Reconcile(func(checkpointmodel.Record) (bool, error) {
		// NewFileExecutionStore already resolved every recoverable candidate under
		// this operation lease. A second candidate is therefore a changed cut.
		return false, nil
	})
	if err != nil || len(snapshot.Attention()) != 0 {
		return nil, nil, nil, errors.Join(err, checkpointmodel.ErrRecordRecovery)
	}
	records := snapshot.Records()
	indexed := make(map[checkpointmodel.RecordID]checkpointmodel.Record, len(records))
	objects := make(map[checkpointmodel.ObjectID]struct{}, len(records))
	for _, record := range records {
		if record.OperationID() != lease.operation.OperationID() ||
			record.ReceiveIntentDigest() != lease.operation.ReceiveIntentDigest() ||
			record.MaterializationBindingDigest() != lease.operation.BindingDigest() ||
			record.AuthorityRef() != lease.authorityRef ||
			record.MaterializerKind() != checkpointmodel.MaterializerNativeTree {
			return nil, nil, nil, checkpointmodel.ErrRecordBinding
		}
		if _, duplicate := objects[record.OwnedObjectID()]; duplicate {
			return nil, nil, nil, checkpointmodel.ErrRecordObjectConflict
		} else {
			objects[record.OwnedObjectID()] = struct{}{}
		}
		indexed[record.RecordID()] = record
	}
	for _, reference := range lifecycle.CheckpointReferences() {
		record, found := indexed[reference.RecordID()]
		if !found || record.CheckpointGeneration() != reference.CheckpointGeneration() ||
			(record.CommitState() != checkpointmodel.CommitVerified &&
				record.CommitState() != checkpointmodel.CommitPublished) {
			return nil, nil, nil, checkpointmodel.ErrRecordBinding
		}
	}
	artifacts, err := scanNativeResumeRecoveryArtifacts(lease.operationDirectory)
	if err != nil {
		return nil, nil, nil, err
	}
	for object := range artifacts {
		if _, owned := objects[object]; !owned {
			return nil, nil, nil, outputcap.ErrUnsafeNamespace
		}
	}
	directories, err := lease.admittedDirectoriesLocked()
	if err != nil {
		return nil, nil, nil, err
	}
	return store, records, directories, nil
}

func scanNativeResumeRecoveryArtifacts(
	operation outputcap.Directory,
) (map[checkpointmodel.ObjectID]struct{}, error) {
	checkpoints, err := openNativeResumeDirectory(operation, checkpointstore.CheckpointsDirectory, true)
	if err != nil {
		return nil, err
	}
	defer checkpoints.Close()
	result := make(map[checkpointmodel.ObjectID]struct{})
	for _, target := range []struct {
		name string
		kind checkpointstore.RecoveryArtifactKind
	}{
		{name: checkpointstore.StagesDirectory, kind: checkpointstore.RecoveryStage},
		{name: checkpointstore.AnchorsDirectory, kind: checkpointstore.RecoveryAnchor},
	} {
		directory, err := openNativeResumeDirectory(checkpoints, target.name, true)
		if err != nil {
			return nil, err
		}
		shards, err := directory.Names(checkpointstore.ShardLimit)
		if err != nil || len(shards) >= checkpointstore.ShardLimit {
			return nil, errors.Join(err, outputcap.ErrUnsafeNamespace, directory.Close())
		}
		for _, shardName := range shards {
			shard, err := checkpointstore.OpenShard(directory, shardName, false)
			if err != nil {
				return nil, errors.Join(err, directory.Close())
			}
			names, err := shard.Names(checkpointstore.EntryLimit)
			if err != nil || len(names) >= checkpointstore.EntryLimit {
				return nil, errors.Join(err, outputcap.ErrUnsafeNamespace, shard.Close(), directory.Close())
			}
			for _, name := range names {
				object, parseErr := checkpointstore.ParseRecoveryArtifactLocation(shardName, name, target.kind)
				kind, exact, classifyErr := shard.ClassifyExactEntry(name)
				if parseErr != nil || classifyErr != nil || !exact || kind != outputcap.EntryRegularFile {
					return nil, errors.Join(
						parseErr, classifyErr, outputcap.ErrUnsafeNamespace, shard.Close(), directory.Close(),
					)
				}
				result[object] = struct{}{}
			}
			if err := shard.Close(); err != nil {
				return nil, errors.Join(err, directory.Close())
			}
		}
		if err := directory.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (lease *NativeResumeLease) admittedDirectoriesLocked() ([]checkpointmodel.AdmittedDirectory, error) {
	manifests, err := openNativeResumeDirectory(
		lease.operationDirectory, checkpointstore.ManifestsDirectory, true,
	)
	if err != nil {
		return nil, err
	}
	defer manifests.Close()
	names, err := manifests.Names(checkpointstore.EntryLimit)
	if err != nil || len(names) >= checkpointstore.EntryLimit {
		return nil, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	slices.Sort(names)
	result := make([]checkpointmodel.AdmittedDirectory, 0, len(names))
	admissions := make(map[checkpointmodel.AggregateDigest]struct{}, len(names))
	paths := make(map[string]struct{}, len(names))
	objects := make(map[transfer.OwnedObjectID]struct{}, len(names))
	for _, name := range names {
		if !strings.HasPrefix(name, nativeResumeDirectoryPrefix) {
			return nil, outputcap.ErrUnsafeNamespace
		}
		encodedID := strings.TrimPrefix(name, nativeResumeDirectoryPrefix)
		if len(encodedID) != catalog.IdentityBytes*2 || encodedID != strings.ToLower(encodedID) {
			return nil, outputcap.ErrUnsafeNamespace
		}
		rawID, decodeErr := hex.DecodeString(encodedID)
		directoryID, identityErr := catalog.DirectoryIDFromBytes(rawID)
		record, readErr := lease.repository.ReadAdmittedDirectory(directoryID)
		if decodeErr != nil || identityErr != nil || readErr != nil ||
			record.OperationID() != lease.operation.OperationID() ||
			record.ReceiveIntentDigest() != lease.operation.ReceiveIntentDigest() {
			return nil, errors.Join(
				decodeErr, identityErr, readErr, checkpointmodel.ErrInvalidAdmittedDirectory,
			)
		}
		if _, duplicate := admissions[record.AdmissionDigest()]; duplicate {
			return nil, checkpointmodel.ErrInvalidAdmittedDirectory
		}
		if _, duplicate := paths[record.CanonicalPath()]; duplicate {
			return nil, checkpointmodel.ErrInvalidAdmittedDirectory
		}
		if _, duplicate := objects[record.OwnedObjectID()]; duplicate {
			return nil, checkpointmodel.ErrInvalidAdmittedDirectory
		}
		admissions[record.AdmissionDigest()] = struct{}{}
		paths[record.CanonicalPath()] = struct{}{}
		objects[record.OwnedObjectID()] = struct{}{}
		result = append(result, record)
	}
	for _, record := range result {
		if record.CanonicalPath() == "" {
			if !record.ParentAdmissionDigest().IsZero() {
				return nil, checkpointmodel.ErrInvalidAdmittedDirectory
			}
			continue
		}
		if _, found := admissions[record.ParentAdmissionDigest()]; !found {
			return nil, checkpointmodel.ErrInvalidAdmittedDirectory
		}
	}
	slices.SortFunc(result, func(left, right checkpointmodel.AdmittedDirectory) int {
		leftDepth := nativeResumePathDepth(left.CanonicalPath())
		rightDepth := nativeResumePathDepth(right.CanonicalPath())
		if leftDepth != rightDepth {
			return leftDepth - rightDepth
		}
		return strings.Compare(left.CanonicalPath(), right.CanonicalPath())
	})
	return result, nil
}

func nativeResumePathDepth(path string) int {
	if path == "" {
		return 0
	}
	return strings.Count(path, "/") + 1
}

func (lease *NativeResumeLease) observeRecordsLocked(
	ctx context.Context,
	store *checkpointstore.FileExecutionStore,
	records []checkpointmodel.Record,
) (bool, error) {
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if record.Phase() == checkpointmodel.PhaseQuarantined ||
			record.CommitState() == checkpointmodel.CommitQuarantined {
			return false, nil
		}
		if record.Phase() == checkpointmodel.PhasePublished ||
			record.Phase() == checkpointmodel.PhaseRetired &&
				record.RetirementReason() == checkpointmodel.RetirementPublished {
			proven, err := observeNativeResumePublication(ctx, lease.platform, store, record)
			if err != nil || !proven {
				return proven, err
			}
			continue
		}
		file, observation, err := store.OpenOwnedFile(
			ctx, record.OwnedObjectID(), record.ExactSize(), false,
		)
		closeErr := closeNativeResumeOwnedFile(file)
		if err != nil || closeErr != nil || observation.ObjectID() != record.OwnedObjectID() {
			return false, errors.Join(err, closeErr, outputcap.ErrUnsafeNamespace)
		}
		switch record.Phase() {
		case checkpointmodel.PhaseRetired:
			switch observation.Condition() {
			case fileexecution.OwnedAbsent, fileexecution.OwnedReady,
				fileexecution.OwnedAnchorMissing, fileexecution.OwnedStageMissing:
				continue
			default:
				return false, nil
			}
		case checkpointmodel.PhaseReserved, checkpointmodel.PhaseActive,
			checkpointmodel.PhasePaused, checkpointmodel.PhasePublishing:
			if observation.Condition() != fileexecution.OwnedReady {
				return false, nil
			}
		default:
			return false, nil
		}
	}
	return true, nil
}

func closeNativeResumeOwnedFile(file fileexecution.OwnedFile) error {
	if file == nil {
		return nil
	}
	return file.Close()
}
