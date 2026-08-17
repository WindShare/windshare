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
	stage  outputcap.MutableFile
	anchor outputcap.ObservedFile
	closed bool
}

// observedOwnedFile satisfies the file-execution transaction port for terminal
// checkpoints without widening its native handles. Mutation methods fail closed;
// only active/paused/publishing records request nativeOwnedFile instead.
type observedOwnedFile struct {
	object checkpointmodel.ObjectID
	files  observedOwnedFiles
	closed bool
}

func (file *observedOwnedFile) ObjectID() checkpointmodel.ObjectID { return file.object }
func (*observedOwnedFile) WriteAt([]byte, int64) (int, error) {
	return 0, fileOutputBoundaryErrorWithoutContext(transferfault.ScopeFileLocal, outputcap.ErrUnsafeNamespace)
}
func (*observedOwnedFile) Sync() error {
	return fileOutputBoundaryErrorWithoutContext(transferfault.ScopeFileLocal, outputcap.ErrUnsafeNamespace)
}
func (*observedOwnedFile) SetModifiedTime(catalog.ModifiedTime) error {
	return fileOutputBoundaryErrorWithoutContext(transferfault.ScopeFileLocal, outputcap.ErrUnsafeNamespace)
}
func (file *observedOwnedFile) MetadataMatches(
	exactSize uint64,
	modifiedTime catalog.ModifiedTime,
) (bool, error) {
	if file == nil || file.closed || file.files.stage == nil {
		return false, fileOutputBoundaryErrorWithoutContext(
			transferfault.ScopeFileLocal, outputcap.ErrUnsafeNamespace,
		)
	}
	matches, err := file.files.stage.MetadataMatches(exactSize, modifiedTime)
	return matches, fileOutputBoundaryErrorWithoutContext(transferfault.ScopeFileLocal, err)
}
func (file *observedOwnedFile) Close() error {
	if file == nil || file.closed {
		return nil
	}
	file.closed = true
	err := file.files.close()
	file.files = observedOwnedFiles{}
	return fileOutputBoundaryErrorWithoutContext(transferfault.ScopeFileLocal, err)
}

type exactParentStageAuthority interface {
	WithExactParent(context.Context, func(outputcap.Directory) error) error
}

type ordinaryStageCreator interface {
	CreateOrdinaryOutputStage(outputcap.Directory, string, uint64) error
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

func (store *FileExecutionStore) ObserveOwnedObject(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	exactSize uint64,
) (observation fileexecution.OwnedObservation, resultErr error) {
	defer func() {
		resultErr = fileOutputBoundaryError(ctx, transferfault.ScopeFileLocal, resultErr)
	}()
	if store == nil || ctx == nil || object.IsZero() || exactSize > catalog.MaxFileSize {
		return fileexecution.OwnedObservation{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return fileexecution.OwnedObservation{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	files, observation, err := store.openObservedOwnedLocked(object, exactSize, true)
	return observation, errors.Join(err, files.close())
}

func (store *FileExecutionStore) CreateOwnedFile(
	ctx context.Context,
	destination fileexecution.FileDestination,
	object checkpointmodel.ObjectID,
	exactSize uint64,
) (file fileexecution.OwnedFile, observation fileexecution.OwnedObservation, resultErr error) {
	defer func() {
		resultErr = fileOutputBoundaryError(ctx, transferfault.ScopeFileLocal, resultErr)
	}()
	if store == nil || ctx == nil || store.profile.Valid() && destination == nil || object.IsZero() || exactSize > catalog.MaxFileSize {
		return nil, fileexecution.OwnedObservation{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return nil, fileexecution.OwnedObservation{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()

	existing, observation, observeErr := store.openObservedOwnedLocked(object, exactSize, true)
	observeErr = errors.Join(observeErr, existing.close())
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

	stage, created, createErr := store.createOwnedStage(
		ctx, destination, stageDirectory, stageName, exactSize,
	)
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

func (store *FileExecutionStore) createOwnedStage(
	ctx context.Context,
	destination fileexecution.FileDestination,
	stageDirectory outputcap.Directory,
	stageName string,
	exactSize uint64,
) (outputcap.MutableFile, bool, error) {
	if !store.profile.Valid() {
		// Synthetic stores retain the private constructor for isolated unit tests;
		// production composition always supplies a certified ordinary profile.
		stage, err := stageDirectory.CreateFile(stageName, true, int64(exactSize))
		return stage, err == nil && stage != nil, err
	}
	parent, ok := destination.(exactParentStageAuthority)
	if !ok {
		return nil, false, outputcap.ErrUnsafeNamespace
	}
	err := parent.WithExactParent(ctx, func(finalParent outputcap.Directory) error {
		creator, ok := finalParent.(ordinaryStageCreator)
		if !ok {
			return outputcap.ErrUnsafeNamespace
		}
		return creator.CreateOrdinaryOutputStage(stageDirectory, stageName, exactSize)
	})
	if err != nil {
		return nil, false, err
	}
	stage, err := stageDirectory.OpenMutableFile(stageName, false)
	return stage, stage != nil, err
}

func (store *FileExecutionStore) reconcileOwnedCreateLocked(
	object checkpointmodel.ObjectID,
	exactSize uint64,
	created bool,
	operationErr error,
) (fileexecution.OwnedFile, fileexecution.OwnedObservation, error) {
	file, observation, observeErr := store.openMutableOwnedLocked(object, exactSize, true)
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
	if writable {
		return store.openMutableOwnedLocked(object, exactSize, true)
	}
	files, observation, err := store.openObservedOwnedLocked(object, exactSize, true)
	if err != nil || observation.Condition() != fileexecution.OwnedReady {
		return nil, observation, errors.Join(err, files.close())
	}
	return &observedOwnedFile{object: object, files: files}, observation, nil
}

type privateEntryState uint8

const (
	privateEntryAbsent privateEntryState = iota + 1
	privateEntryReady
	privateEntryUnsafe
)

type privateFileObservation struct {
	directory outputcap.Directory
	file      outputcap.FileIdentity
	state     privateEntryState
	err       error
}

type observedOwnedFiles struct {
	stage  outputcap.ObservedFile
	anchor outputcap.ObservedFile
}

func (files observedOwnedFiles) close() error {
	return errors.Join(closeFile(files.stage), closeFile(files.anchor))
}

func (store *FileExecutionStore) openObservedOwnedLocked(
	object checkpointmodel.ObjectID,
	exactSize uint64,
	validateSize bool,
) (observedOwnedFiles, fileexecution.OwnedObservation, error) {
	privateProfile := !store.profile.Valid()
	stage := observeOwnedDataFile(store.repository.stages, object, ownedStageSuffix, privateProfile)
	anchor := observeOwnedDataFile(store.repository.anchors, object, ownedAnchorSuffix, privateProfile)
	condition, comparisonErr := classifyOwnedObservation(stage, anchor, exactSize, validateSize)
	observation, observationErr := fileexecution.NewOwnedObservation(object, condition)
	closeDirectoriesErr := errors.Join(closeDirectory(stage.directory), closeDirectory(anchor.directory))
	files := observedOwnedFiles{}
	if stage.file != nil {
		files.stage, _ = stage.file.(outputcap.ObservedFile)
	}
	if anchor.file != nil {
		files.anchor, _ = anchor.file.(outputcap.ObservedFile)
	}
	if condition != fileexecution.OwnedReady || files.stage == nil || files.anchor == nil {
		return observedOwnedFiles{}, observation, errors.Join(
			stageObservationError(stage), anchorObservationError(anchor), comparisonErr, observationErr,
			closeFile(stage.file), closeFile(anchor.file), closeDirectoriesErr,
		)
	}
	return files, observation, errors.Join(comparisonErr, observationErr, closeDirectoriesErr)
}

func (store *FileExecutionStore) openMutableOwnedLocked(
	object checkpointmodel.ObjectID,
	exactSize uint64,
	validateSize bool,
) (fileexecution.OwnedFile, fileexecution.OwnedObservation, error) {
	privateProfile := !store.profile.Valid()
	stage := openMutableOwnedDataFile(store.repository.stages, object, ownedStageSuffix, privateProfile)
	anchor := observeOwnedDataFile(store.repository.anchors, object, ownedAnchorSuffix, privateProfile)
	condition, comparisonErr := classifyOwnedObservation(stage, anchor, exactSize, validateSize)
	observation, observationErr := fileexecution.NewOwnedObservation(object, condition)
	closeDirectoriesErr := errors.Join(closeDirectory(stage.directory), closeDirectory(anchor.directory))
	mutable, mutableOK := stage.file.(outputcap.MutableFile)
	observedAnchor, anchorOK := anchor.file.(outputcap.ObservedFile)
	if condition != fileexecution.OwnedReady || !mutableOK || !anchorOK {
		return nil, observation, errors.Join(
			stageObservationError(stage), anchorObservationError(anchor), comparisonErr, observationErr,
			closeFile(stage.file), closeFile(anchor.file), closeDirectoriesErr,
		)
	}
	return &nativeOwnedFile{object: object, stage: mutable, anchor: observedAnchor}, observation,
		errors.Join(comparisonErr, observationErr, closeDirectoriesErr)
}

type ownedFileOpener func(outputcap.Directory, string, bool) (outputcap.FileIdentity, error)

func observeOwnedDataFile(
	root outputcap.Directory,
	object checkpointmodel.ObjectID,
	suffix string,
	privateProfile bool,
) privateFileObservation {
	return openOwnedDataFile(root, object, suffix, privateProfile, func(
		directory outputcap.Directory, name string, private bool,
	) (outputcap.FileIdentity, error) {
		return directory.OpenObservedFile(name, private)
	})
}

func openMutableOwnedDataFile(
	root outputcap.Directory,
	object checkpointmodel.ObjectID,
	suffix string,
	privateProfile bool,
) privateFileObservation {
	return openOwnedDataFile(root, object, suffix, privateProfile, func(
		directory outputcap.Directory, name string, private bool,
	) (outputcap.FileIdentity, error) {
		return directory.OpenMutableFile(name, private)
	})
}

func openRecoveryOwnedDataFile(
	root outputcap.Directory,
	object checkpointmodel.ObjectID,
	suffix string,
	privateProfile bool,
) privateFileObservation {
	return openOwnedDataFile(root, object, suffix, privateProfile, func(
		directory outputcap.Directory, name string, private bool,
	) (outputcap.FileIdentity, error) {
		return directory.OpenRecoveryDurabilityFile(name, private)
	})
}

func openOwnedDataFile(
	root outputcap.Directory,
	object checkpointmodel.ObjectID,
	suffix string,
	privateProfile bool,
	opener ownedFileOpener,
) privateFileObservation {
	shard, name := ownedObjectLocation(object, suffix)
	directory, err := OpenShard(root, shard, false)
	if errors.Is(err, fs.ErrNotExist) {
		return privateFileObservation{state: privateEntryAbsent}
	}
	if err != nil || directory == nil {
		return privateFileObservation{directory: directory, state: privateEntryUnsafe, err: err}
	}
	kind, exact, err := directory.ClassifyExactEntry(name)
	if err != nil {
		return privateFileObservation{directory: directory, state: privateEntryUnsafe, err: err}
	}
	if kind == outputcap.EntryAbsent {
		return privateFileObservation{directory: directory, state: privateEntryAbsent}
	}
	if !exact || kind != outputcap.EntryRegularFile {
		return privateFileObservation{directory: directory, state: privateEntryUnsafe}
	}
	file, err := opener(directory, name, privateProfile)
	if err != nil || file == nil {
		return privateFileObservation{directory: directory, file: file, state: privateEntryUnsafe, err: err}
	}
	return privateFileObservation{directory: directory, file: file, state: privateEntryReady}
}

func stageObservationError(observation privateFileObservation) error {
	if observation.state == privateEntryUnsafe {
		return errors.Join(outputcap.ErrUnsafeNamespace, observation.err)
	}
	return nil
}

func anchorObservationError(observation privateFileObservation) error {
	if observation.state == privateEntryUnsafe {
		return errors.Join(outputcap.ErrUnsafeNamespace, observation.err)
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

func sameExactOwnedFiles(stage, anchor outputcap.FileIdentity, exactSize uint64) (bool, error) {
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
