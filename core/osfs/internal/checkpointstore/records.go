package checkpointstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"runtime"
	"strconv"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

const (
	// Checkpoint records are small authenticated objects. Keeping the bound below
	// the session-header limit prevents a malformed record from turning a restart
	// scan into an unbounded allocation.
	maxFileCheckpointBytes = resumestate.MaxSessionHeaderBytes
	ShardLimit             = resumestate.MaxFileStateShardDirectories + 1
	EntryLimit             = resumestate.MaxFileStateEntriesPerSession + 1
	candidateReadAttempts  = outputnamespace.AllocationAttempts
)

func ValidShard(name string) bool {
	if len(name) != 2 {
		return false
	}
	for _, character := range name {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func OpenOrCreateRecordsDirectory(parent outputcap.Directory) (outputcap.Directory, error) {
	return openOrCreateDirectory(parent, resumestate.CheckpointsDirectoryName)
}

func OpenShard(parent outputcap.Directory, shard string, create bool) (outputcap.Directory, error) {
	if parent == nil || !ValidShard(shard) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if create {
		return openOrCreateDirectory(parent, shard)
	}
	opened, err := parent.OpenDirectory(shard, true)
	if err == nil {
		return opened, nil
	}
	if opened != nil {
		_ = opened.Close()
	}
	return nil, err
}

func TemporaryName(name string, encoded []byte, attempt int) string {
	hash := sha256.Sum256(append([]byte(name+"\x00"), encoded...))
	return ".candidate-" + hex.EncodeToString(hash[:8]) + "-" + strconv.Itoa(attempt)
}

func IsTemporaryName(name string) bool {
	return len(name) > len(".candidate-") && name[:len(".candidate-")] == ".candidate-"
}

// MatchesTemporaryName authenticates an installation candidate by both its
// deterministic name and exact image. A prefix match alone is never ownership
// evidence and must not authorize reconciliation.
func MatchesTemporaryName(candidate, target string, encoded []byte) bool {
	for attempt := range outputnamespace.AllocationAttempts {
		if candidate == TemporaryName(target, encoded, attempt) {
			return true
		}
	}
	return false
}

func WriteFile(file outputcap.File, encoded []byte) error {
	if file == nil || len(encoded) == 0 || len(encoded) > maxFileCheckpointBytes {
		return resumestate.ErrInvalidFileCheckpoint
	}
	written, err := file.WriteAt(encoded, 0)
	if err == nil && written != len(encoded) {
		err = fmt.Errorf("%w: checkpoint short write", outputcap.ErrUnsafeNamespace)
	}
	if err != nil {
		return err
	}
	return file.Sync()
}

func ReadFile(directory outputcap.Directory, name string) ([]byte, error) {
	encoded, err := outputnamespace.ReadRecord(directory, name, maxFileCheckpointBytes)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// RemoveExact makes the authenticated checkpoint image the deletion authority.
// A caller holding only a record name must not be able to retire a checkpoint
// that another recovery decision replaced after it was observed.
func RemoveExact(directory outputcap.Directory, name string, expected []byte) (error, error) {
	if directory == nil || name == "" || len(expected) == 0 || len(expected) > maxFileCheckpointBytes {
		return resumestate.ErrInvalidFileCheckpoint, nil
	}
	file, err := directory.OpenFile(name, true, false)
	if err != nil {
		return err, closeFile(file)
	}
	actual, err := outputnamespace.ReadFile(file, maxFileCheckpointBytes)
	if err != nil || !bytes.Equal(actual, expected) {
		return errors.Join(resumestate.ErrFileCheckpointBinding, err), file.Close()
	}
	operationErr := directory.RemoveFile(name, file)
	if operationErr == nil {
		operationErr = directory.Sync()
	}
	return operationErr, file.Close()
}

func RemoveTemporary(directory outputcap.Directory, name string, file outputcap.File) error {
	if directory == nil || file == nil {
		return nil
	}
	removeErr := directory.RemoveFile(name, file)
	closeErr := file.Close()
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(removeErr, closeErr)
}

func RemoveExactTemporary(directory outputcap.Directory, name string, expected []byte) error {
	file, err := openExactTemporary(directory, name, expected)
	if err != nil {
		return err
	}
	return RemoveTemporary(directory, name, file)
}

func InstallCreate(directory outputcap.Directory, name string, encoded []byte) error {
	if directory == nil || name == "" || len(encoded) == 0 || len(encoded) > maxFileCheckpointBytes {
		return resumestate.ErrInvalidFileCheckpoint
	}
	for attempt := range outputnamespace.AllocationAttempts {
		temporary, temporaryName, err := prepareInstallationCandidate(directory, name, encoded, attempt)
		if errors.Is(err, fs.ErrNotExist) {
			// A collision can disappear before it is opened. Advancing to the next
			// deterministic slot preserves idempotence without trusting the race.
			continue
		}
		if err != nil {
			return err
		}
		return installCreatedCandidate(directory, name, encoded, temporaryName, temporary)
	}
	return fmt.Errorf("%w: checkpoint temporary allocation exhausted", outputcap.ErrUnsafeNamespace)
}

func prepareInstallationCandidate(
	directory outputcap.Directory,
	targetName string,
	encoded []byte,
	attempt int,
) (outputcap.File, string, error) {
	temporaryName := TemporaryName(targetName, encoded, attempt)
	temporary, err := directory.CreateFile(temporaryName, true, int64(len(encoded)))
	if errors.Is(err, outputcap.ErrNamespaceCollision) {
		temporary, err = openExactTemporary(directory, temporaryName, encoded)
		return temporary, temporaryName, err
	}
	if candidateContention(err) {
		return nil, temporaryName, errors.Join(outputcap.ErrNamespaceLockBusy, err)
	}
	if err != nil {
		return nil, temporaryName, err
	}
	if writeErr := WriteFile(temporary, encoded); writeErr != nil {
		return nil, temporaryName, errors.Join(
			writeErr,
			RemoveTemporary(directory, temporaryName, temporary),
		)
	}
	return temporary, temporaryName, nil
}

func installCreatedCandidate(
	directory outputcap.Directory,
	targetName string,
	encoded []byte,
	temporaryName string,
	temporary outputcap.File,
) error {
	target, linkErr := directory.LinkFileNoReplace(temporary, targetName)
	if errors.Is(linkErr, outputcap.ErrNamespaceCollision) {
		return settleCreateCollision(directory, targetName, encoded, temporaryName, temporary)
	}
	if errors.Is(linkErr, fs.ErrNotExist) || candidateContention(linkErr) {
		return errors.Join(
			outputcap.ErrNamespaceLockBusy,
			linkErr,
			RemoveTemporary(directory, temporaryName, temporary),
		)
	}
	if linkErr != nil {
		return errors.Join(linkErr, RemoveTemporary(directory, temporaryName, temporary))
	}
	return verifyCreatedTarget(directory, targetName, encoded, temporaryName, temporary, target)
}

func settleCreateCollision(
	directory outputcap.Directory,
	targetName string,
	encoded []byte,
	temporaryName string,
	temporary outputcap.File,
) error {
	cleanupErr := RemoveTemporary(directory, temporaryName, temporary)
	existing, readErr := ReadFile(directory, targetName)
	if readErr == nil && bytes.Equal(existing, encoded) {
		return nil // idempotent retry of the same authenticated image
	}
	if candidateContention(readErr) {
		return errors.Join(outputcap.ErrNamespaceLockBusy, readErr, cleanupErr)
	}
	return errors.Join(resumestate.ErrFileCheckpointBinding, readErr, cleanupErr)
}

func verifyCreatedTarget(
	directory outputcap.Directory,
	targetName string,
	encoded []byte,
	temporaryName string,
	temporary outputcap.File,
	target outputcap.File,
) error {
	closeTargetErr := closeFile(target)
	syncErr := directory.Sync()
	actual, readErr := ReadFile(directory, targetName)
	cleanupErr := RemoveTemporary(directory, temporaryName, temporary)
	if candidateContention(cleanupErr) {
		// Once the target image is exact, another bootstrap reader may delay only
		// retirement of the private name. Restart reconciliation owns it.
		cleanupErr = nil
	}
	if readErr == nil && bytes.Equal(actual, encoded) {
		return errors.Join(closeTargetErr, syncErr, cleanupErr)
	}
	return errors.Join(
		outputcap.ErrUnsafeNamespace,
		resumestate.ErrFileCheckpointCrashBoundary,
		closeTargetErr,
		syncErr,
		readErr,
		cleanupErr,
	)
}

func InstallReplace(directory outputcap.Directory, name string, previous, next []byte) error {
	if directory == nil || name == "" || len(next) == 0 || len(next) > maxFileCheckpointBytes {
		return resumestate.ErrInvalidFileCheckpoint
	}
	current, err := ReadFile(directory, name)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, previous) {
		return resumestate.ErrFileCheckpointBinding
	}
	for attempt := range outputnamespace.AllocationAttempts {
		temporary, temporaryName, createErr := prepareInstallationCandidate(directory, name, next, attempt)
		if errors.Is(createErr, fs.ErrNotExist) {
			continue
		}
		if createErr != nil {
			return createErr
		}
		replaceErr := directory.ReplacePrivateFile(temporary, name)
		syncErr := directory.Sync()
		actual, readErr := ReadFile(directory, name)
		var closeErr, cleanupErr error
		if readErr == nil && bytes.Equal(actual, next) {
			// Exact bytes are the restart boundary. A reported replace/sync error is
			// still returned so the caller can pause, but the next opener may adopt
			// this authenticated image.
			closeErr = temporary.Close()
			return errors.Join(replaceErr, syncErr, closeErr)
		}
		if replaceErr == nil {
			closeErr = temporary.Close()
		}
		// ReplacePrivateFile consumes the private temporary name on success. On a
		// failed replacement the helper removes the still-owned temporary and
		// closes its capability exactly once.
		if replaceErr != nil {
			cleanupErr = RemoveTemporary(directory, temporaryName, temporary)
		}
		return errors.Join(
			outputcap.ErrUnsafeNamespace, resumestate.ErrFileCheckpointCrashBoundary,
			replaceErr, syncErr, readErr, closeErr, cleanupErr,
		)
	}
	return fmt.Errorf("%w: checkpoint replacement temporary allocation exhausted", outputcap.ErrUnsafeNamespace)
}

func openExactTemporary(
	directory outputcap.Directory,
	name string,
	expected []byte,
) (outputcap.File, error) {
	var mismatchErr error
	for range candidateReadAttempts {
		file, err := directory.OpenFile(name, true, false)
		if err != nil {
			if candidateContention(err) {
				return nil, errors.Join(outputcap.ErrNamespaceLockBusy, err)
			}
			return nil, err
		}
		actual, readErr := outputnamespace.ReadFile(file, maxFileCheckpointBytes)
		if readErr != nil {
			closeErr := file.Close()
			if candidateContention(readErr) {
				// Windows can admit the handle and still reject the first read while
				// the creating writer owns the candidate. That is contention, not
				// evidence that the deterministic image is corrupt.
				return nil, errors.Join(outputcap.ErrNamespaceLockBusy, readErr, closeErr)
			}
			return nil, errors.Join(resumestate.ErrFileCheckpointBinding, readErr, closeErr)
		}
		if bytes.Equal(actual, expected) {
			return file, nil
		}
		mismatchErr = resumestate.ErrFileCheckpointBinding
		if closeErr := file.Close(); closeErr != nil {
			return nil, errors.Join(mismatchErr, closeErr)
		}
		// CreatePrivateFile exposes the correctly named, pre-sized entry before
		// its first write. Yielding lets that owner finish without trusting an
		// image that remains inexact across the bounded observation window.
		runtime.Gosched()
	}
	return nil, mismatchErr
}

func candidateContention(err error) bool {
	return err != nil && (errors.Is(err, outputcap.ErrNamespaceLockBusy) ||
		errors.Is(err, outputcap.ErrFixedLinkSourceChanged) || platformCandidateContention(err))
}

func FileFor(directory outputcap.Directory, recordID resumestate.FileCheckpointRecordID, create bool) (outputcap.Directory, string, error) {
	name := resumestate.FileCheckpointName(recordID)
	shard, err := OpenShard(directory, name.Shard(), create)
	if err != nil {
		return nil, "", err
	}
	return shard, name.Name(), nil
}
