package checkpointstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

const recoveryWitnessDomain = "windshare/partial-metadata/v1\x00"
const maximumRecoveryMetadataBytes = 256

type recoveryMetadataSource interface{ RecoveryMetadata() ([]byte, error) }

func encodedRecoveryWitness(source outputcap.FileIdentity) ([]byte, error) {
	metadata, ok := source.(recoveryMetadataSource)
	if !ok {
		return nil, outputcap.ErrUnsafeNamespace
	}
	stamp, err := metadata.RecoveryMetadata()
	if err != nil {
		return nil, err
	}
	if len(stamp) == 0 || len(stamp) > maximumRecoveryMetadataBytes {
		return nil, outputcap.ErrUnsafeNamespace
	}
	size, err := source.Size()
	if err != nil {
		return nil, err
	}
	encoded := append([]byte(recoveryWitnessDomain), stamp...)
	encoded = binary.BigEndian.AppendUint64(encoded, size)
	checksum := sha256.Sum256(encoded)
	return append(encoded, checksum[:]...), nil
}

// Native anchors can supply stronger evidence, but are not a prerequisite for
// retaining a partial on filesystems that only support ordinary Go file I/O.
func createRecoveryWitness(directory outputcap.Directory, stage outputcap.FileIdentity, name string) (outputcap.ObservedFile, error) {
	if _, portable := stage.(recoveryMetadataSource); !portable {
		return directory.LinkFileNoReplace(stage, name)
	}
	encoded, err := encodedRecoveryWitness(stage)
	if err != nil {
		return nil, err
	}
	if err := InstallCreate(directory, name, encoded); err != nil {
		return nil, err
	}
	return directory.OpenObservedFile(name, true)
}

func matchesRecoveryWitness(stage, anchor outputcap.FileIdentity) (bool, error) {
	if _, portable := stage.(recoveryMetadataSource); !portable {
		return stage.SameFile(anchor)
	}
	reader, ok := anchor.(outputcap.ObservedFile)
	if !ok {
		return false, outputcap.ErrUnsafeNamespace
	}
	expected, err := encodedRecoveryWitness(stage)
	if err != nil {
		return false, err
	}
	size, err := anchor.Size()
	if err != nil {
		return false, err
	}
	if size != uint64(len(expected)) {
		return false, nil
	}
	actual := make([]byte, len(expected))
	_, err = reader.ReadAt(actual, 0)
	return err == nil && bytes.Equal(actual, expected), err
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

// Ordinary private paths do not establish deletion authority by themselves.
// Reopened data must still match its recorded metadata; auxiliary witnesses must
// retain their checked encoding even after the stage has been removed.
func (store *FileExecutionStore) verifyOwnedRemoval(object checkpointmodel.ObjectID, suffix string, file outputcap.ObservedFile) (err error) {
	if _, portable := file.(recoveryMetadataSource); !portable {
		return nil
	}
	if suffix == ownedAnchorSuffix {
		return validateRecoveryWitness(file)
	}
	anchor := observeOwnedDataFile(store.repository.anchors, object, ownedAnchorSuffix, true)
	defer func() { err = errors.Join(err, closeFile(anchor.file), closeDirectory(anchor.directory)) }()
	if anchor.state != privateEntryReady || anchor.file == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, anchorObservationError(anchor))
	}
	matched, err := matchesRecoveryWitness(file, anchor.file)
	if err != nil || !matched {
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	return nil
}

func validateRecoveryWitness(file outputcap.ObservedFile) error {
	size, err := file.Size()
	minimum := len(recoveryWitnessDomain) + 1 + 8 + sha256.Size
	maximum := len(recoveryWitnessDomain) + maximumRecoveryMetadataBytes + 8 + sha256.Size
	if err != nil || size < uint64(minimum) || size > uint64(maximum) {
		return errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	encoded := make([]byte, int(size))
	if _, err := file.ReadAt(encoded, 0); err != nil {
		return err
	}
	body := encoded[:len(encoded)-sha256.Size]
	checksum := sha256.Sum256(body)
	if !bytes.HasPrefix(body, []byte(recoveryWitnessDomain)) || !bytes.Equal(encoded[len(body):], checksum[:]) {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}
