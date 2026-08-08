package checkpointstore

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"strings"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

type nativeOwnedFile struct {
	object checkpointmodel.ObjectID
	stage  outputcap.File
	anchor outputcap.File
	closed bool
}

type RecoveryArtifactKind uint8

const (
	RecoveryStage RecoveryArtifactKind = iota + 1
	RecoveryAnchor
)

func (kind RecoveryArtifactKind) suffix() (string, bool) {
	switch kind {
	case RecoveryStage:
		return ownedStageSuffix, true
	case RecoveryAnchor:
		return ownedAnchorSuffix, true
	default:
		return "", false
	}
}

func RecoveryArtifactLocation(
	object checkpointmodel.ObjectID,
	kind RecoveryArtifactKind,
) (string, string, error) {
	suffix, valid := kind.suffix()
	if object.IsZero() || !valid {
		return "", "", checkpointmodel.ErrRecordBinding
	}
	shard, name := ownedObjectLocation(object, suffix)
	return shard, name, nil
}

func ParseRecoveryArtifactLocation(
	shard string,
	name string,
	kind RecoveryArtifactKind,
) (checkpointmodel.ObjectID, error) {
	suffix, valid := kind.suffix()
	if !valid || !ValidShard(shard) || len(name) != recordIDHexLength+len(suffix) ||
		!strings.HasSuffix(name, suffix) || name[:recordShardLength] != shard {
		return checkpointmodel.ObjectID{}, checkpointmodel.ErrRecordBinding
	}
	encoded := strings.TrimSuffix(name, suffix)
	if encoded != strings.ToLower(encoded) {
		return checkpointmodel.ObjectID{}, checkpointmodel.ErrRecordBinding
	}
	raw, err := hex.DecodeString(encoded)
	if err != nil {
		return checkpointmodel.ObjectID{}, checkpointmodel.ErrRecordBinding
	}
	return checkpointmodel.ObjectIDFromBytes(raw)
}

func (file *nativeOwnedFile) ObjectID() checkpointmodel.ObjectID { return file.object }
func (file *nativeOwnedFile) WriteAt(data []byte, offset int64) (int, error) {
	if file == nil || file.stage == nil || file.closed {
		return 0, fileOutputBoundaryErrorWithoutContext(transferfault.ScopeFileLocal, outputcap.ErrUnsafeNamespace)
	}
	written, err := file.stage.WriteAt(data, offset)
	return written, fileOutputBoundaryErrorWithoutContext(transferfault.ScopeFileLocal, err)
}
func (file *nativeOwnedFile) Sync() error {
	if file == nil || file.stage == nil || file.closed {
		return fileOutputBoundaryErrorWithoutContext(transferfault.ScopeFileLocal, outputcap.ErrUnsafeNamespace)
	}
	return fileOutputBoundaryErrorWithoutContext(transferfault.ScopeFileLocal, file.stage.Sync())
}
func (file *nativeOwnedFile) SetModifiedTime(modifiedTime catalog.ModifiedTime) error {
	if file == nil || file.stage == nil || file.closed {
		return fileOutputBoundaryErrorWithoutContext(transferfault.ScopeFileLocal, outputcap.ErrUnsafeNamespace)
	}
	return fileOutputBoundaryErrorWithoutContext(transferfault.ScopeFileLocal, file.stage.SetModifiedTime(modifiedTime))
}
func (file *nativeOwnedFile) MetadataMatches(exactSize uint64, modifiedTime catalog.ModifiedTime) (bool, error) {
	if file == nil || file.stage == nil || file.closed {
		return false, fileOutputBoundaryErrorWithoutContext(transferfault.ScopeFileLocal, outputcap.ErrUnsafeNamespace)
	}
	matches, err := file.stage.MetadataMatches(exactSize, modifiedTime)
	return matches, fileOutputBoundaryErrorWithoutContext(transferfault.ScopeFileLocal, err)
}
func (file *nativeOwnedFile) Close() error {
	if file == nil || file.closed {
		return nil
	}
	file.closed = true
	err := errors.Join(closeFile(file.stage), closeFile(file.anchor))
	file.stage, file.anchor = nil, nil
	return fileOutputBoundaryErrorWithoutContext(transferfault.ScopeFileLocal, err)
}

func (store *FileExecutionStore) CreateOwnedFile(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
) (file fileexecution.OwnedFile, observation fileexecution.OwnedObservation, resultErr error) {
	defer func() {
		resultErr = fileOutputBoundaryError(ctx, transferfault.ScopeFileLocal, resultErr)
	}()
	if store == nil || ctx == nil || object.IsZero() || exactSize > catalog.MaxFileSize {
		return nil, fileexecution.OwnedObservation{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, fileexecution.OwnedObservation{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	existing, observation, observeErr := store.openOwnedLocked(object, exactSize, true, false)
	if existing != nil {
		observeErr = errors.Join(observeErr, existing.Close())
	}
	if observeErr != nil {
		return nil, observation, observeErr
	}
	if observation.Condition() != fileexecution.OwnedAbsent {
		collision, _ := fileexecution.NewOwnedObservation(object, fileexecution.OwnedObjectCollision)
		return nil, collision, nil
	}

	stageShard, stageName := ownedObjectLocation(object, ownedStageSuffix)
	anchorShard, anchorName := ownedObjectLocation(object, ownedAnchorSuffix)
	stageDirectory, err := OpenShard(store.repository.stages, stageShard, true)
	if err != nil {
		return store.reconcileOwnedCreateLocked(object, exactSize, false, err)
	}
	anchorDirectory, err := OpenShard(store.repository.anchors, anchorShard, true)
	if err != nil {
		return store.reconcileOwnedCreateLocked(
			object, exactSize, false, errors.Join(err, stageDirectory.Close()),
		)
	}

	stage, createErr := stageDirectory.CreateFile(stageName, true, int64(exactSize))
	created := createErr == nil && stage != nil
	if createErr == nil && stage == nil {
		createErr = outputcap.ErrUnsafeNamespace
	}
	if createErr != nil {
		return store.reconcileOwnedCreateLocked(
			object,
			exactSize,
			created,
			errors.Join(createErr, closeFile(stage), stageDirectory.Close(), anchorDirectory.Close()),
		)
	}
	if err := errors.Join(stage.Sync(), stageDirectory.Sync()); err != nil {
		return store.reconcileOwnedCreateLocked(
			object,
			exactSize,
			true,
			errors.Join(err, stage.Close(), stageDirectory.Close(), anchorDirectory.Close()),
		)
	}
	anchor, linkErr := anchorDirectory.LinkFileNoReplace(stage, anchorName)
	if linkErr == nil && anchor == nil {
		linkErr = outputcap.ErrUnsafeNamespace
	}
	if linkErr != nil {
		return store.reconcileOwnedCreateLocked(
			object,
			exactSize,
			true,
			errors.Join(linkErr, closeFile(anchor), stage.Close(), stageDirectory.Close(), anchorDirectory.Close()),
		)
	}
	if err := anchorDirectory.Sync(); err != nil {
		return store.reconcileOwnedCreateLocked(
			object,
			exactSize,
			true,
			errors.Join(err, anchor.Close(), stage.Close(), stageDirectory.Close(), anchorDirectory.Close()),
		)
	}
	ready, verifyErr := sameExactOwnedFiles(stage, anchor, exactSize)
	closeErr := errors.Join(stageDirectory.Close(), anchorDirectory.Close())
	if verifyErr != nil || !ready {
		return store.reconcileOwnedCreateLocked(
			object,
			exactSize,
			true,
			errors.Join(verifyErr, closeErr, anchor.Close(), stage.Close()),
		)
	}
	observation, _ = fileexecution.NewOwnedObservation(object, fileexecution.OwnedReady)
	return &nativeOwnedFile{object: object, stage: stage, anchor: anchor}, observation, closeErr
}

func (store *FileExecutionStore) reconcileOwnedCreateLocked(
	object checkpointmodel.ObjectID,
	exactSize uint64,
	created bool,
	operationErr error,
) (fileexecution.OwnedFile, fileexecution.OwnedObservation, error) {
	file, observation, observeErr := store.openOwnedLocked(object, exactSize, true, true)
	if !created && observation.Condition() != fileexecution.OwnedAbsent {
		if file != nil {
			observeErr = errors.Join(observeErr, file.Close())
			file = nil
		}
		observation, _ = fileexecution.NewOwnedObservation(object, fileexecution.OwnedObjectCollision)
	}
	return file, observation, errors.Join(operationErr, observeErr)
}

func (store *FileExecutionStore) OpenOwnedFile(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
	writable bool,
) (file fileexecution.OwnedFile, observation fileexecution.OwnedObservation, resultErr error) {
	defer func() {
		resultErr = fileOutputBoundaryError(ctx, transferfault.ScopeFileLocal, resultErr)
	}()
	if store == nil || ctx == nil || object.IsZero() || exactSize > catalog.MaxFileSize {
		return nil, fileexecution.OwnedObservation{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, fileexecution.OwnedObservation{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.openOwnedLocked(object, exactSize, true, writable)
}

type privateEntryState uint8

const (
	privateEntryAbsent privateEntryState = iota + 1
	privateEntryReady
	privateEntryUnsafe
)

type privateFileObservation struct {
	directory outputcap.Directory
	file      outputcap.File
	state     privateEntryState
}

func (store *FileExecutionStore) openOwnedLocked(
	object checkpointmodel.ObjectID,
	exactSize uint64,
	validateSize bool,
	writable bool,
) (fileexecution.OwnedFile, fileexecution.OwnedObservation, error) {
	stage := observePrivateFile(store.repository.stages, object, ownedStageSuffix, writable)
	anchor := observePrivateFile(store.repository.anchors, object, ownedAnchorSuffix, false)
	condition, comparisonErr := classifyOwnedObservation(stage, anchor, exactSize, validateSize)
	observation, observationErr := fileexecution.NewOwnedObservation(object, condition)
	closeDirectoriesErr := errors.Join(closeDirectory(stage.directory), closeDirectory(anchor.directory))
	if condition != fileexecution.OwnedReady {
		return nil, observation, errors.Join(
			stageObservationError(stage), anchorObservationError(anchor), comparisonErr, observationErr,
			closeFile(stage.file), closeFile(anchor.file), closeDirectoriesErr,
		)
	}
	return &nativeOwnedFile{object: object, stage: stage.file, anchor: anchor.file}, observation,
		errors.Join(comparisonErr, observationErr, closeDirectoriesErr)
}

func observePrivateFile(
	root outputcap.Directory,
	object checkpointmodel.ObjectID,
	suffix string,
	writable bool,
) privateFileObservation {
	shard, name := ownedObjectLocation(object, suffix)
	directory, err := OpenShard(root, shard, false)
	if errors.Is(err, fs.ErrNotExist) {
		return privateFileObservation{state: privateEntryAbsent}
	}
	if err != nil || directory == nil {
		return privateFileObservation{directory: directory, state: privateEntryUnsafe}
	}
	kind, exact, err := directory.ClassifyExactEntry(name)
	if err != nil {
		return privateFileObservation{directory: directory, state: privateEntryUnsafe}
	}
	if kind == outputcap.EntryAbsent {
		return privateFileObservation{directory: directory, state: privateEntryAbsent}
	}
	if !exact || kind != outputcap.EntryRegularFile {
		return privateFileObservation{directory: directory, state: privateEntryUnsafe}
	}
	file, err := directory.OpenFile(name, true, writable)
	if err != nil || file == nil {
		return privateFileObservation{directory: directory, file: file, state: privateEntryUnsafe}
	}
	return privateFileObservation{directory: directory, file: file, state: privateEntryReady}
}

func stageObservationError(observation privateFileObservation) error {
	if observation.state == privateEntryUnsafe {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}

func anchorObservationError(observation privateFileObservation) error {
	if observation.state == privateEntryUnsafe {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}

func classifyOwnedObservation(
	stage privateFileObservation,
	anchor privateFileObservation,
	exactSize uint64,
	validateSize bool,
) (fileexecution.OwnedCondition, error) {
	switch {
	case stage.state == privateEntryAbsent && anchor.state == privateEntryAbsent:
		return fileexecution.OwnedAbsent, nil
	case anchor.state == privateEntryUnsafe:
		return fileexecution.OwnedAnchorUnsafe, nil
	case anchor.state == privateEntryAbsent:
		return fileexecution.OwnedAnchorMissing, nil
	case stage.state == privateEntryUnsafe:
		return fileexecution.OwnedStageUnsafe, nil
	case stage.state == privateEntryAbsent:
		return fileexecution.OwnedStageMissing, nil
	}
	if stage.file == nil || anchor.file == nil {
		return fileexecution.OwnedStageUnsafe, outputcap.ErrUnsafeNamespace
	}
	same, err := stage.file.SameFile(anchor.file)
	if err != nil {
		return fileexecution.OwnedStageUnsafe, err
	}
	if !same {
		return fileexecution.OwnedStageMismatch, nil
	}
	if validateSize {
		ready, err := sameExactOwnedFiles(stage.file, anchor.file, exactSize)
		if err != nil {
			return fileexecution.OwnedStageUnsafe, err
		}
		if !ready {
			return fileexecution.OwnedStageMismatch, nil
		}
	}
	return fileexecution.OwnedReady, nil
}

func sameExactOwnedFiles(stage, anchor outputcap.File, exactSize uint64) (bool, error) {
	if stage == nil || anchor == nil {
		return false, outputcap.ErrUnsafeNamespace
	}
	same, sameErr := stage.SameFile(anchor)
	stageSize, stageErr := stage.Size()
	anchorSize, anchorErr := anchor.Size()
	if sameErr != nil || stageErr != nil || anchorErr != nil {
		return false, errors.Join(sameErr, stageErr, anchorErr)
	}
	return same && stageSize == exactSize && anchorSize == exactSize, nil
}

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
	var operationErr error
	switch step {
	case fileexecution.RetirementRemoveStage:
		operationErr = removeOwnedEntry(store.repository.stages, object, ownedStageSuffix)
	case fileexecution.RetirementSyncStageNamespace:
		operationErr = syncOwnedEntryNamespace(store.repository.stages, object)
	case fileexecution.RetirementRemoveAnchor:
		operationErr = removeOwnedEntry(store.repository.anchors, object, ownedAnchorSuffix)
	case fileexecution.RetirementSyncAnchorNamespace:
		operationErr = syncOwnedEntryNamespace(store.repository.anchors, object)
	}
	file, observation, observeErr := store.openOwnedLocked(object, 0, false, false)
	return observation, errors.Join(operationErr, observeErr, closeOwnedFile(file))
}

func (store *FileExecutionStore) initialCandidateReady(record checkpointmodel.Record) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	file, observation, err := store.openOwnedLocked(
		record.OwnedOutputObject(), record.ExactSize(), true, false,
	)
	if observation.Condition() != fileexecution.OwnedReady {
		return false, errors.Join(err, closeOwnedFile(file))
	}
	if err != nil {
		return false, errors.Join(err, closeOwnedFile(file))
	}
	syncErr := file.Sync()
	if syncErr == nil {
		syncErr = errors.Join(
			syncOwnedEntryNamespace(store.repository.stages, record.OwnedOutputObject()),
			syncOwnedEntryNamespace(store.repository.anchors, record.OwnedOutputObject()),
		)
	}
	return syncErr == nil, errors.Join(syncErr, closeOwnedFile(file))
}

// FinalMatchesOwned compares a public file against the exact private anchor.
// Published checkpoints intentionally retain that anchor after retiring the
// writable stage, so identity remains provable across process restarts.
func (store *FileExecutionStore) FinalMatchesOwned(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
	final outputcap.File,
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
	anchor := observePrivateFile(store.repository.anchors, object, ownedAnchorSuffix, false)
	if anchor.state != privateEntryReady || anchor.file == nil {
		return false, errors.Join(
			anchorObservationError(anchor), closeFile(anchor.file), closeDirectory(anchor.directory),
		)
	}
	same, compareErr := sameExactOwnedFiles(final, anchor.file, exactSize)
	return same, errors.Join(compareErr, anchor.file.Close(), anchor.directory.Close())
}

// PublishOwnedNoReplace links the private anchor into one already-guarded public
// parent. The caller still performs post-link path and metadata reconciliation.
func (store *FileExecutionStore) PublishOwnedNoReplace(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
	parent outputcap.Directory,
	name string,
) (linked outputcap.File, resultErr error) {
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
	file, observation, err := store.openOwnedLocked(object, exactSize, true, false)
	owned, ok := file.(*nativeOwnedFile)
	if err != nil || observation.Condition() != fileexecution.OwnedReady || !ok || owned.anchor == nil {
		return nil, errors.Join(err, closeOwnedFile(file), outputcap.ErrUnsafeNamespace)
	}
	linked, linkErr := parent.LinkFileNoReplace(owned.anchor, name)
	return linked, errors.Join(linkErr, owned.Close())
}

func removeOwnedEntry(root outputcap.Directory, object checkpointmodel.ObjectID, suffix string) error {
	shard, name := ownedObjectLocation(object, suffix)
	directory, err := OpenShard(root, shard, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	kind, exact, classifyErr := directory.ClassifyExactEntry(name)
	if classifyErr != nil || !exact {
		return errors.Join(classifyErr, outputcap.ErrUnsafeNamespace, directory.Close())
	}
	if kind == outputcap.EntryAbsent {
		return directory.Close()
	}
	if kind != outputcap.EntryRegularFile {
		return errors.Join(outputcap.ErrUnsafeNamespace, directory.Close())
	}
	file, openErr := directory.OpenFile(name, true, false)
	if openErr != nil || file == nil {
		return errors.Join(openErr, outputcap.ErrUnsafeNamespace, closeFile(file), directory.Close())
	}
	removeErr := directory.RemoveFile(name, file)
	return errors.Join(removeErr, file.Close(), directory.Close())
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

func closeOwnedFile(file fileexecution.OwnedFile) error {
	if file == nil {
		return nil
	}
	return file.Close()
}
