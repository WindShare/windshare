package outputruntime

import (
	"bytes"
	"errors"
	"io/fs"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type persistedCheckpointLoader struct {
	session        *Session
	rootIdentity   resumestate.FileCheckpointRootID
	byPath         map[string]resumestate.FileCheckpointV1
	byKey          map[resumestate.LiveFileKey]resumestate.FileCheckpointV1
	byObject       map[resumestate.FileCheckpointObjectID]persistedCheckpointRecord
	blockedPaths   map[string]struct{}
	blockedObjects map[resumestate.FileCheckpointObjectID]struct{}
	attention      []ResumeAttention
}

type persistedCheckpointRecord struct {
	checkpoint resumestate.FileCheckpointV1
	stateName  string
}

func (session *Session) loadPersistedIncrementalCheckpoints() ([]ResumeAttention, error) {
	if session == nil || !session.incrementalAdmission {
		return nil, nil
	}
	if session.checkpointsDir == nil || session.incrementalIntentDigest.IsZero() {
		return nil, checkpointOutputFault("open FileCheckpointV1 namespace", transfer.ErrInvalidOutputBinding)
	}
	root := session.checkpointRuntime.RootIdentity
	if root.IsZero() {
		return nil, checkpointOutputFault("bind FileCheckpointV1 root", resumestate.ErrFileCheckpointBinding)
	}
	shards, err := session.checkpointsDir.Names(checkpointstore.ShardLimit)
	if err != nil {
		return nil, checkpointOutputFault("list FileCheckpointV1 shards", err)
	}
	if len(shards) >= checkpointstore.ShardLimit {
		return nil, checkpointOutputFault("bound FileCheckpointV1 shards", resumestate.ErrFileCheckpointRecovery)
	}
	slices.Sort(shards)
	loader := persistedCheckpointLoader{
		session:        session,
		rootIdentity:   root,
		byPath:         make(map[string]resumestate.FileCheckpointV1),
		byKey:          make(map[resumestate.LiveFileKey]resumestate.FileCheckpointV1),
		byObject:       make(map[resumestate.FileCheckpointObjectID]persistedCheckpointRecord),
		blockedPaths:   make(map[string]struct{}),
		blockedObjects: make(map[resumestate.FileCheckpointObjectID]struct{}),
	}
	if err := loader.loadShards(shards); err != nil {
		return nil, err
	}
	session.stateInstall.Lock()
	session.incrementalCheckpointByPath = loader.byPath
	session.incrementalCheckpoints = loader.byKey
	session.stateInstall.Unlock()
	if err := session.installCheckpointObjectClaims(loader.byPath); err != nil {
		return nil, err
	}
	return loader.attention, nil
}

func (session *Session) installCheckpointObjectClaims(
	checkpoints map[string]resumestate.FileCheckpointV1,
) error {
	claims := make(map[resumestate.OutputObjectID]resumestate.LocatorDigest, len(checkpoints))
	for path, checkpoint := range checkpoints {
		objectID, err := resumestate.OutputObjectIDFromBytes(checkpoint.OwnedOutputObject().Bytes())
		if err != nil {
			return checkpointOutputFault("bind checkpoint output object", err)
		}
		claims[objectID] = resumestate.DigestCanonicalLocator(path)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.objectClaims == nil {
		session.objectClaims = make(map[resumestate.OutputObjectID]resumestate.LocatorDigest)
	}
	for objectID, locator := range claims {
		if existing, claimed := session.objectClaims[objectID]; claimed && existing != locator {
			return checkpointOutputFault("claim checkpoint output object", resumestate.ErrFileCheckpointObjectConflict)
		}
		session.objectClaims[objectID] = locator
	}
	return nil
}

func (loader *persistedCheckpointLoader) loadShards(shards []string) error {
	for _, shardName := range shards {
		if !checkpointstore.ValidShard(shardName) {
			loader.addAttention("unclassified-checkpoint-shard", shardName)
			continue
		}
		if err := loader.loadShard(shardName); err != nil {
			return err
		}
	}
	return nil
}

func (loader *persistedCheckpointLoader) loadShard(shardName string) (resultErr error) {
	shard, err := loader.session.checkpointsDir.OpenDirectory(shardName, true)
	if err != nil {
		return checkpointOutputFault("open FileCheckpointV1 shard", err)
	}
	defer func() {
		if closeErr := shard.Close(); closeErr != nil {
			resultErr = errors.Join(
				resultErr,
				checkpointOutputFault("close FileCheckpointV1 shard", closeErr),
			)
		}
	}()
	names, err := shard.Names(checkpointstore.EntryLimit)
	if err != nil {
		return checkpointOutputFault("list FileCheckpointV1 records", err)
	}
	if len(names) >= checkpointstore.EntryLimit {
		return checkpointOutputFault("bound FileCheckpointV1 records", resumestate.ErrFileCheckpointRecovery)
	}
	slices.Sort(names)
	for _, name := range names {
		if checkpointstore.IsTemporaryName(name) {
			continue
		}
		if err := loader.loadRecord(shard, shardName, name); err != nil {
			return err
		}
	}
	for _, name := range names {
		if !checkpointstore.IsTemporaryName(name) {
			continue
		}
		if err := loader.loadTemporaryRecord(shard, shardName, name); err != nil {
			return err
		}
	}
	return nil
}

func (loader *persistedCheckpointLoader) loadRecord(
	shard outputcap.Directory,
	shardName string,
	name string,
) error {
	stateName := shardName + "/" + name
	recordID, err := resumestate.ParseFileCheckpointName(shardName, name)
	if err != nil {
		loader.addAttention("unclassified-checkpoint-record", stateName)
		return nil
	}
	encoded, err := checkpointstore.ReadFile(shard, name)
	if err != nil {
		return checkpointOutputFault("read FileCheckpointV1 record", err)
	}
	checkpoint, err := resumestate.DecodeFileCheckpointV1(encoded)
	if err != nil || !loader.validBinding(checkpoint, recordID) {
		loader.addAttention("invalid-checkpoint-binding", stateName)
		return nil
	}
	if checkpoint.CommitState() == resumestate.FileCheckpointCommitCandidate {
		checkpoint, err = loader.reconcileInitialCandidate(shard, name, checkpoint, encoded)
		if err != nil {
			return err
		}
	}
	if !committedCheckpointState(checkpoint.CommitState()) {
		loader.addAttention("uncommitted-checkpoint", stateName)
		return nil
	}
	loader.admitCheckpoint(checkpoint, stateName)
	return nil
}

func (loader *persistedCheckpointLoader) loadTemporaryRecord(
	shard outputcap.Directory,
	shardName string,
	name string,
) error {
	stateName := shardName + "/" + name
	encoded, err := checkpointstore.ReadFile(shard, name)
	if err != nil {
		return checkpointOutputFault("read FileCheckpointV1 candidate", err)
	}
	checkpoint, err := resumestate.DecodeFileCheckpointV1(encoded)
	if err != nil {
		loader.addAttention("invalid-checkpoint-candidate", stateName)
		return nil
	}
	target := resumestate.FileCheckpointName(checkpoint.RecordID())
	if target.Shard() != shardName || !checkpointstore.MatchesTemporaryName(name, target.Name(), encoded) ||
		!loader.validBinding(checkpoint, checkpoint.RecordID()) {
		loader.addAttention("invalid-checkpoint-candidate", stateName)
		return nil
	}
	currentEncoded, readErr := checkpointstore.ReadFile(shard, target.Name())
	switch {
	case errors.Is(readErr, fs.ErrNotExist):
		if !initialCheckpointCandidate(checkpoint) {
			loader.addAttention("orphaned-checkpoint-candidate", stateName)
			return nil
		}
		err = checkpointstore.InstallCreate(shard, target.Name(), encoded)
	case readErr != nil:
		return checkpointOutputFault("read FileCheckpointV1 candidate target", readErr)
	case bytes.Equal(currentEncoded, encoded):
		err = checkpointstore.InstallCreate(shard, target.Name(), encoded)
	default:
		current, decodeErr := resumestate.DecodeFileCheckpointV1(currentEncoded)
		if decodeErr == nil && checkpoint.CommitState() == resumestate.FileCheckpointCommitCandidate &&
			committedCheckpointState(current.CommitState()) &&
			resumestate.ValidateCheckpointTransition(checkpoint, current) == nil {
			if err := checkpointstore.RemoveExactTemporary(shard, name, encoded); err != nil {
				return checkpointOutputFault("remove superseded FileCheckpointV1 candidate", err)
			}
			return nil
		}
		if decodeErr != nil || !committedCheckpointState(checkpoint.CommitState()) ||
			resumestate.ValidateCheckpointTransition(current, checkpoint) != nil {
			loader.addAttention("conflicting-checkpoint-candidate", stateName)
			return nil
		}
		err = checkpointstore.InstallReplace(shard, target.Name(), currentEncoded, encoded)
	}
	if err != nil {
		return checkpointOutputFault("reconcile FileCheckpointV1 candidate", err)
	}
	return loader.loadRecord(shard, shardName, target.Name())
}

func (loader *persistedCheckpointLoader) reconcileInitialCandidate(
	shard outputcap.Directory,
	name string,
	checkpoint resumestate.FileCheckpointV1,
	encoded []byte,
) (resumestate.FileCheckpointV1, error) {
	if !initialCheckpointCandidate(checkpoint) {
		return checkpoint, nil
	}
	recoverable, err := loader.session.verifyInitialCandidateWitness(checkpoint)
	if err != nil {
		return resumestate.FileCheckpointV1{},
			checkpointOutputFault("verify initial FileCheckpointV1 candidate", err)
	}
	if !recoverable {
		return checkpoint, nil
	}
	verified, err := resumestate.PromoteCheckpoint(
		checkpoint, resumestate.FileCheckpointPhaseActive, resumestate.FileCheckpointCommitVerified,
	)
	if err != nil {
		return resumestate.FileCheckpointV1{},
			checkpointOutputFault("promote initial FileCheckpointV1 candidate", err)
	}
	verifiedEncoded, err := resumestate.EncodeFileCheckpointV1(verified)
	if err != nil {
		return resumestate.FileCheckpointV1{},
			checkpointOutputFault("encode promoted FileCheckpointV1 candidate", err)
	}
	if err := checkpointstore.InstallReplace(shard, name, encoded, verifiedEncoded); err != nil {
		return resumestate.FileCheckpointV1{},
			checkpointOutputFault("install promoted FileCheckpointV1 candidate", err)
	}
	return verified, nil
}

func initialCheckpointCandidate(checkpoint resumestate.FileCheckpointV1) bool {
	return checkpoint.CommitState() == resumestate.FileCheckpointCommitCandidate &&
		checkpoint.Phase() == resumestate.FileCheckpointPhaseActive &&
		checkpoint.CheckpointGeneration() == 0 && len(checkpoint.VerifiedRanges()) == 0
}

func (session *Session) verifyInitialCandidateWitness(
	checkpoint resumestate.FileCheckpointV1,
) (recoverable bool, resultErr error) {
	objectID, err := resumestate.OutputObjectIDFromBytes(checkpoint.OwnedOutputObject().Bytes())
	if err != nil {
		return false, err
	}
	stageName := resumestate.StageName(objectID)
	stageDir, present, err := openOutputShard(session.stagesDir, stageName.Shard(), false)
	if err != nil || !present {
		return false, errors.Join(err, closeOutputDirectory(stageDir))
	}
	defer func() { resultErr = errors.Join(resultErr, stageDir.Close()) }()
	anchorName := resumestate.AnchorName(objectID)
	anchorDir, present, err := openOutputShard(session.anchorsDir, anchorName.Shard(), false)
	if err != nil || !present {
		return false, errors.Join(err, closeOutputDirectory(anchorDir))
	}
	defer func() { resultErr = errors.Join(resultErr, anchorDir.Close()) }()
	stage, err := stageDir.OpenFile(stageName.Name(), true, true)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, stage.Close()) }()
	anchor, err := anchorDir.OpenFile(anchorName.Name(), true, false)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, anchor.Close()) }()
	stageSize, stageErr := stage.Size()
	anchorSize, anchorErr := anchor.Size()
	same, sameErr := stage.SameFile(anchor)
	if stageErr != nil || anchorErr != nil || sameErr != nil {
		return false, errors.Join(stageErr, anchorErr, sameErr)
	}
	if stageSize != checkpoint.ExactSize() || anchorSize != checkpoint.ExactSize() || !same {
		return false, nil
	}
	if err := errors.Join(stage.Sync(), stageDir.Sync(), anchorDir.Sync()); err != nil {
		return false, err
	}
	return true, nil
}

func (loader *persistedCheckpointLoader) validBinding(
	checkpoint resumestate.FileCheckpointV1,
	recordID resumestate.FileCheckpointRecordID,
) bool {
	return checkpoint.RecordID() == recordID &&
		checkpoint.TransferIntentDigest() == loader.session.incrementalIntentDigest &&
		checkpoint.BackendID() == filesystemOutputBackendID &&
		bytes.Equal(checkpoint.RootIdentity().Bytes(), loader.rootIdentity.Bytes())
}

func committedCheckpointState(state resumestate.FileCheckpointCommitState) bool {
	return state == resumestate.FileCheckpointCommitVerified ||
		state == resumestate.FileCheckpointCommitPublished ||
		state == resumestate.FileCheckpointCommitQuarantined
}

func (loader *persistedCheckpointLoader) admitCheckpoint(
	checkpoint resumestate.FileCheckpointV1,
	stateName string,
) {
	path := checkpoint.CanonicalPath()
	objectID := checkpoint.OwnedOutputObject()
	if _, blocked := loader.blockedPaths[path]; blocked {
		return
	}
	if _, blocked := loader.blockedObjects[objectID]; blocked {
		return
	}
	if previous, duplicate := loader.byPath[path]; duplicate && previous.RecordID() != checkpoint.RecordID() {
		loader.addAttention("duplicate-checkpoint-path", stateName)
		loader.rejectCheckpoint(previous)
		loader.blockedPaths[path] = struct{}{}
		return
	}
	if previous, duplicate := loader.byObject[objectID]; duplicate &&
		previous.checkpoint.RecordID() != checkpoint.RecordID() {
		loader.addAttention("duplicate-checkpoint-output-object", previous.stateName)
		loader.addAttention("duplicate-checkpoint-output-object", stateName)
		loader.rejectCheckpoint(previous.checkpoint)
		loader.blockedPaths[previous.checkpoint.CanonicalPath()] = struct{}{}
		loader.blockedPaths[path] = struct{}{}
		loader.blockedObjects[objectID] = struct{}{}
		return
	}
	loader.byPath[path] = checkpoint
	loader.byKey[liveFileKeyForCheckpoint(checkpoint)] = checkpoint
	loader.byObject[objectID] = persistedCheckpointRecord{checkpoint: checkpoint, stateName: stateName}
}

func (loader *persistedCheckpointLoader) rejectCheckpoint(checkpoint resumestate.FileCheckpointV1) {
	delete(loader.byPath, checkpoint.CanonicalPath())
	delete(loader.byKey, liveFileKeyForCheckpoint(checkpoint))
	objectID := checkpoint.OwnedOutputObject()
	if existing, found := loader.byObject[objectID]; found &&
		existing.checkpoint.RecordID() == checkpoint.RecordID() {
		delete(loader.byObject, objectID)
	}
}

func liveFileKeyForCheckpoint(
	checkpoint resumestate.FileCheckpointV1,
) resumestate.LiveFileKey {
	return resumestate.LiveFileKey{
		IntentDigest:  checkpoint.TransferIntentDigest(),
		FileID:        checkpoint.FileID(),
		Revision:      checkpoint.FileRevision(),
		CanonicalPath: checkpoint.CanonicalPath(),
		ExactSize:     checkpoint.ExactSize(),
	}
}

func (loader *persistedCheckpointLoader) addAttention(code, state string) {
	loader.attention = append(loader.attention, ResumeAttention{
		Scope: ResumeAttentionFile,
		Code:  code,
		State: state,
	})
}
