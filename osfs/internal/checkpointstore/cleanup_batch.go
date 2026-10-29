package checkpointstore

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"slices"

	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

// Bound both retained private handles and the amount of unfinished cleanup. No
// public placement guard survives this local operation, and no batch waits for
// another file to arrive from the network.
const maximumCleanupBatchRecords = 64

type cleanupNamespace struct {
	root   outputcap.Directory
	shards map[string]outputcap.Directory
}

func newCleanupNamespace(root outputcap.Directory) *cleanupNamespace {
	return &cleanupNamespace{root: root, shards: make(map[string]outputcap.Directory)}
}

func (namespace *cleanupNamespace) shard(name string) (outputcap.Directory, error) {
	if directory, found := namespace.shards[name]; found {
		return directory, nil
	}
	directory, err := OpenShard(namespace.root, name, false)
	if errors.Is(err, fs.ErrNotExist) {
		namespace.shards[name] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	namespace.shards[name] = directory
	return directory, nil
}

func (namespace *cleanupNamespace) sync() error {
	names := make([]string, 0, len(namespace.shards))
	for name := range namespace.shards {
		names = append(names, name)
	}
	slices.Sort(names)
	rootSynced := false
	for _, name := range names {
		directory := namespace.shards[name]
		if directory == nil {
			if rootSynced {
				continue
			}
			directory, rootSynced = namespace.root, true
		}
		if err := directory.Sync(); err != nil {
			return err
		}
	}
	return nil
}

func (namespace *cleanupNamespace) close() error {
	var result error
	for _, directory := range namespace.shards {
		result = errors.Join(result, closeDirectory(directory))
	}
	namespace.shards = nil
	return result
}

func (store *FileExecutionStore) removeOwned(namespace *cleanupNamespace, object checkpointmodel.ObjectID, suffix string) error {
	shardName, name := ownedObjectLocation(object, suffix)
	directory, err := namespace.shard(shardName)
	if err != nil || directory == nil {
		return err
	}
	kind, exact, err := directory.ClassifyExactEntry(name)
	if err != nil || !exact {
		return errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	if kind == outputcap.EntryAbsent {
		return nil
	}
	if kind != outputcap.EntryRegularFile {
		return outputcap.ErrUnsafeNamespace
	}
	file, err := directory.OpenObservedFile(name, store.stagePolicy == PrivateStage)
	if err != nil || file == nil {
		return errors.Join(err, outputcap.ErrUnsafeNamespace, closeFile(file))
	}
	if err := store.verifyOwnedRemoval(object, suffix, file); err != nil {
		return errors.Join(err, file.Close())
	}
	removeErr := directory.RemoveFile(name, file)
	closeErr := file.Close()
	if removeErr != nil || closeErr != nil {
		return errors.Join(removeErr, closeErr)
	}
	kind, exact, err = directory.ClassifyExactEntry(name)
	if err != nil || !exact || kind != outputcap.EntryAbsent {
		return errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	return nil
}

func (namespace *cleanupNamespace) removeImage(image checkpointImage) error {
	expected, err := checkpointmodel.EncodeRecord(image.record)
	if err != nil {
		return err
	}
	shardName, _ := recordLocation(image.record.RecordID())
	directory, err := namespace.shard(shardName)
	if err != nil {
		return err
	}
	if directory == nil {
		return fs.ErrNotExist
	}
	file, err := directory.OpenObservedFile(image.name, true)
	if err != nil {
		return errors.Join(err, closeFile(file))
	}
	actual, readErr := readBoundedFile(file)
	if readErr != nil || !bytes.Equal(actual, expected) {
		return errors.Join(checkpointmodel.ErrRecordBinding, readErr, closeFile(file))
	}
	return errors.Join(directory.RemoveFile(image.name, file), file.Close())
}

// cleanupBatch keeps the same recovery ordering as individual retirement:
// durable stage removal precedes anchor removal, and both precede record removal.
// Each directory is flushed once per phase. A failed barrier retains the later
// ownership evidence instead of acknowledging a partially retired batch.
func (store *FileExecutionStore) cleanupBatch(
	ctx context.Context,
	images []checkpointImage,
	retired map[checkpointmodel.ObjectID]struct{},
) (resultErr error) {
	stages := newCleanupNamespace(store.repository.stages)
	anchors := newCleanupNamespace(store.repository.anchors)
	recordNamespace := newCleanupNamespace(store.repository.records)
	recordsTouched := false
	defer func() {
		resultErr = errors.Join(resultErr, stages.close(), anchors.close(), recordNamespace.close())
		if resultErr != nil && recordsTouched {
			store.authority.markUnsettled()
		}
	}()
	objects := make([]checkpointmodel.ObjectID, 0, len(images))
	seen := make(map[checkpointmodel.ObjectID]struct{})
	for _, image := range images {
		object := image.record.OwnedObjectID()
		_, alreadyRetired := retired[object]
		_, duplicate := seen[object]
		if !alreadyRetired && !duplicate {
			objects = append(objects, object)
			seen[object] = struct{}{}
		}
	}
	for _, phase := range []struct {
		namespace *cleanupNamespace
		suffix    string
	}{{stages, ownedStageSuffix}, {anchors, ownedAnchorSuffix}} {
		for _, object := range objects {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := store.removeOwned(phase.namespace, object, phase.suffix); err != nil {
				return err
			}
		}
		if err := phase.namespace.sync(); err != nil {
			return err
		}
	}
	for _, object := range objects {
		retired[object] = struct{}{}
	}
	for _, image := range images {
		if err := ctx.Err(); err != nil {
			return err
		}
		recordsTouched = true
		if err := recordNamespace.removeImage(image); err != nil {
			return err
		}
	}
	if err := recordNamespace.sync(); err != nil {
		store.authority.markUnsettled()
		return err
	}
	for _, image := range images {
		if err := store.authority.removeImage(image); err != nil {
			store.authority.markUnsettled()
			return codedError(ErrorCorruptRecord, "remove file checkpoint authority", err)
		}
	}
	return nil
}
