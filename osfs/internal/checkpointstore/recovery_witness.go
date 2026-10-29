package checkpointstore

import (
	"errors"

	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/recoverywitness"
)

type portableWitnessFile interface{ BindRecoveryWitness([]byte) (bool, error) }
type witnessCreator interface{ CreateRecoveryWitness() ([]byte, error) }
type witnessRemoval interface{ RecoveryRemovalAllowed([]byte) (bool, error) }
type witnessPublisher interface {
	PrepareRecoveryPublication([]byte) ([]byte, error)
	StripRecoveryWitness([]byte) error
}

func readRecoveryWitness(file outputcap.FileIdentity) ([]byte, error) {
	reader, ok := file.(outputcap.ObservedFile)
	if !ok {
		return nil, outputcap.ErrUnsafeNamespace
	}
	size, err := file.Size()
	if err != nil || size != uint64(recoverywitness.EncodedSize) {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	encoded := make([]byte, recoverywitness.EncodedSize)
	if _, err := reader.ReadAt(encoded, 0); err != nil {
		return nil, err
	}
	if _, err := recoverywitness.Decode(encoded); err != nil {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	return encoded, nil
}

// The portable witness is carried by the object itself. A sidecar records the
// expected token but cannot confer ownership on a replacement at the same path.
func createRecoveryWitness(directory outputcap.Directory, stage outputcap.FileIdentity, name string) (outputcap.ObservedFile, error) {
	creator, portable := stage.(witnessCreator)
	if !portable {
		return directory.LinkFileNoReplace(stage, name)
	}
	encoded, err := creator.CreateRecoveryWitness()
	if err != nil {
		return nil, err
	}
	if err := InstallCreate(directory, name, encoded); err != nil {
		return nil, err
	}
	return directory.OpenObservedFile(name, true)
}

func matchesRecoveryWitness(stage, anchor outputcap.FileIdentity) (bool, error) {
	portable, ok := stage.(portableWitnessFile)
	if !ok {
		return stage.SameFile(anchor)
	}
	encoded, err := readRecoveryWitness(anchor)
	if err != nil {
		return false, err
	}
	return portable.BindRecoveryWitness(encoded)
}

func (store *FileExecutionStore) preparePortablePublication(object checkpointmodel.ObjectID, stage outputcap.FileIdentity) error {
	publisher, ok := stage.(witnessPublisher)
	if !ok {
		return outputcap.ErrUnsafeNamespace
	}
	anchor := observeOwnedDataFile(store.repository.anchors, object, ownedAnchorSuffix, true)
	if anchor.state != privateEntryReady || anchor.file == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, anchorObservationError(anchor), closeFile(anchor.file), closeDirectory(anchor.directory))
	}
	previous, err := readRecoveryWitness(anchor.file)
	err = errors.Join(err, anchor.file.Close(), anchor.directory.Close())
	if err != nil {
		return err
	}
	next, err := publisher.PrepareRecoveryPublication(previous)
	if err != nil {
		return err
	}
	shard, name := ownedObjectLocation(object, ownedAnchorSuffix)
	directory, err := OpenShard(store.repository.anchors, shard, false)
	if err != nil {
		return err
	}
	err = errors.Join(InstallReplace(directory, name, previous, next), directory.Close())
	if err != nil {
		return err
	}
	// Persist reconciliation evidence before removing the private trailer. A
	// restart can then finish a staged or published file without native IDs.
	return publisher.StripRecoveryWitness(next)
}

type noReplacePublisher interface {
	PublishFileNoReplace(outputcap.FileIdentity, string) (outputcap.PublishNoReplaceOutcome, error)
}

func installFileNoReplace(directory outputcap.Directory, source outputcap.FileIdentity, name string) (outputcap.ObservedFile, error) {
	if publisher, ok := directory.(noReplacePublisher); ok {
		outcome, err := publisher.PublishFileNoReplace(source, name)
		if err != nil {
			return nil, err
		}
		if outcome == outputcap.PublishNoReplaceCollision {
			return nil, outputcap.ErrNamespaceCollision
		}
		if outcome != outputcap.PublishNoReplaceCommitted {
			return nil, outputcap.ErrUnsafeNamespace
		}
		return directory.OpenObservedFile(name, true)
	}
	return directory.LinkFileNoReplace(source, name)
}

func publishRecoveryStage(parent outputcap.Directory, stage outputcap.FileIdentity, name string) (outputcap.ObservedFile, error) {
	publisher, ok := parent.(noReplacePublisher)
	if !ok {
		return nil, outputcap.ErrUnsafeNamespace
	}
	outcome, err := publisher.PublishFileNoReplace(stage, name)
	if err != nil {
		return nil, err
	}
	if outcome == outputcap.PublishNoReplaceCollision {
		return nil, outputcap.ErrNamespaceCollision
	}
	if outcome != outputcap.PublishNoReplaceCommitted {
		return nil, outputcap.ErrUnsafeNamespace
	}
	final, err := parent.OpenObservedFile(name, false)
	if err != nil {
		return nil, errors.Join(err, closeFile(final))
	}
	return final, nil
}

// Deletion requires the embedded object token. Matching publication samples
// only permits delivery reconciliation, never deletion of a trailerless file.
func (store *FileExecutionStore) verifyOwnedRemoval(object checkpointmodel.ObjectID, suffix string, file outputcap.ObservedFile) (err error) {
	removal, portable := file.(witnessRemoval)
	if !portable {
		return nil
	}
	if suffix == ownedAnchorSuffix {
		_, err := readRecoveryWitness(file)
		return err
	}
	anchor := observeOwnedDataFile(store.repository.anchors, object, ownedAnchorSuffix, true)
	defer func() { err = errors.Join(err, closeFile(anchor.file), closeDirectory(anchor.directory)) }()
	if anchor.state != privateEntryReady || anchor.file == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, anchorObservationError(anchor))
	}
	encoded, err := readRecoveryWitness(anchor.file)
	if err != nil {
		return err
	}
	matched, err := removal.RecoveryRemovalAllowed(encoded)
	if err != nil || !matched {
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	return nil
}
