package checkpointstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"runtime"
	"strconv"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

const (
	// A V1 record can contain 16,384 verified ranges. One MiB bounds malformed
	// input without rejecting the protocol's maximum canonical record.
	maxFileCheckpointBytes = 1 << 20
	ShardLimit             = checkpointmodel.MaxCheckpointShardDirectories + 1
	EntryLimit             = checkpointmodel.MaxCheckpointRecordsPerIntent + 1
	installationAttempts   = 16
	candidateReadAttempts  = installationAttempts
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

func OpenShard(parent outputcap.Directory, shard string, create bool) (outputcap.Directory, error) {
	if parent == nil || !ValidShard(shard) {
		return nil, transfer.ErrInvalidOutputBinding
	}
	if create {
		return openOrCreateDirectory(parent, shard)
	}
	return openExistingDirectory(parent, shard)
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
	for attempt := range installationAttempts {
		if candidate == TemporaryName(target, encoded, attempt) {
			return true
		}
	}
	return false
}

func WriteFile(file outputcap.File, encoded []byte) error {
	if file == nil || len(encoded) == 0 || len(encoded) > maxFileCheckpointBytes {
		return checkpointmodel.ErrInvalidRecord
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
	if directory == nil || name == "" {
		return nil, checkpointmodel.ErrInvalidRecord
	}
	kind, exact, err := directory.ClassifyExactEntry(name)
	if err != nil {
		return nil, err
	}
	if kind == outputcap.EntryAbsent {
		return nil, fs.ErrNotExist
	}
	if !exact || kind != outputcap.EntryRegularFile {
		return nil, outputcap.ErrUnsafeNamespace
	}
	file, err := directory.OpenFile(name, true, false)
	if err != nil {
		return nil, errors.Join(err, closeFile(file))
	}
	encoded, readErr := readBoundedFile(file)
	return encoded, errors.Join(readErr, closeFile(file))
}

func readBoundedFile(file outputcap.File) ([]byte, error) {
	if file == nil {
		return nil, checkpointmodel.ErrInvalidRecord
	}
	size, err := file.Size()
	if err != nil {
		return nil, err
	}
	if size == 0 || size > maxFileCheckpointBytes {
		return nil, outputcap.ErrUnsafeNamespace
	}
	encoded := make([]byte, int(size))
	read, err := file.ReadAt(encoded, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if read != len(encoded) {
		return nil, io.ErrUnexpectedEOF
	}
	return encoded, nil
}

// RemoveExact makes the authenticated checkpoint image the deletion authority.
// A caller holding only a record name must not be able to retire a checkpoint
// that another recovery decision replaced after it was observed.
func RemoveExact(directory outputcap.Directory, name string, expected []byte) (error, error) {
	if directory == nil || name == "" || len(expected) == 0 || len(expected) > maxFileCheckpointBytes {
		return checkpointmodel.ErrInvalidRecord, nil
	}
	file, err := directory.OpenFile(name, true, false)
	if err != nil {
		return err, closeFile(file)
	}
	actual, err := readBoundedFile(file)
	if err != nil || !bytes.Equal(actual, expected) {
		return errors.Join(checkpointmodel.ErrRecordBinding, err), closeFile(file)
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
	if removeErr == nil {
		removeErr = directory.Sync()
	}
	closeErr := file.Close()
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(removeErr, closeErr)
}

func RemoveExactTemporary(directory outputcap.Directory, name string, expected []byte) error {
	file, err := openExactTemporary(directory, name, expected)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return RemoveTemporary(directory, name, file)
}

func InstallCreate(directory outputcap.Directory, name string, encoded []byte) error {
	if directory == nil || name == "" || len(encoded) == 0 || len(encoded) > maxFileCheckpointBytes {
		return checkpointmodel.ErrInvalidRecord
	}
	installed, err := ReadFile(directory, name)
	switch {
	case err == nil && bytes.Equal(installed, encoded):
		return reconcileExactCandidates(directory, name, encoded)
	case err == nil:
		return checkpointmodel.ErrRecordBinding
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	for attempt := range installationAttempts {
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
		closeErr := closeFile(temporary)
		reopened, reopenErr := openExactTemporary(directory, temporaryName, encoded)
		if closeErr != nil {
			return nil, temporaryName, errors.Join(closeErr, reopenErr, closeFile(reopened))
		}
		return reopened, temporaryName, reopenErr
	}
	if candidateContention(err) {
		return nil, temporaryName, errors.Join(outputcap.ErrNamespaceLockBusy, err, closeFile(temporary))
	}
	if err != nil {
		return nil, temporaryName, errors.Join(err, closeFile(temporary))
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
		return errors.Join(closeFile(target),
			settleCreateCollision(directory, targetName, encoded, temporaryName, temporary))
	}
	if errors.Is(linkErr, fs.ErrNotExist) || candidateContention(linkErr) {
		return errors.Join(
			outputcap.ErrNamespaceLockBusy,
			linkErr,
			closeFile(target),
			RemoveTemporary(directory, temporaryName, temporary),
		)
	}
	if linkErr != nil {
		return errors.Join(linkErr, closeFile(target), RemoveTemporary(directory, temporaryName, temporary))
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
		return errors.Join(cleanupErr, reconcileExactCandidates(directory, targetName, encoded))
	}
	if candidateContention(readErr) {
		return errors.Join(outputcap.ErrNamespaceLockBusy, readErr, cleanupErr)
	}
	return errors.Join(checkpointmodel.ErrRecordBinding, readErr, cleanupErr)
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
		return errors.Join(closeTargetErr, syncErr, cleanupErr,
			reconcileExactCandidates(directory, targetName, encoded))
	}
	return errors.Join(
		outputcap.ErrUnsafeNamespace,
		checkpointmodel.ErrRecordCrashBoundary,
		closeTargetErr,
		syncErr,
		readErr,
		cleanupErr,
	)
}

func InstallReplace(directory outputcap.Directory, name string, previous, next []byte) error {
	if directory == nil || name == "" || len(previous) == 0 || len(previous) > maxFileCheckpointBytes ||
		len(next) == 0 || len(next) > maxFileCheckpointBytes {
		return checkpointmodel.ErrInvalidRecord
	}
	current, err := ReadFile(directory, name)
	if err != nil {
		return err
	}
	if bytes.Equal(current, next) {
		return reconcileExactCandidates(directory, name, next)
	}
	if !bytes.Equal(current, previous) {
		return checkpointmodel.ErrRecordBinding
	}
	for attempt := range installationAttempts {
		temporary, temporaryName, createErr := prepareInstallationCandidate(directory, name, next, attempt)
		if errors.Is(createErr, fs.ErrNotExist) {
			continue
		}
		if createErr != nil {
			return createErr
		}
		// Preparing a candidate may yield to another owner or an external
		// namespace mutation. Reopen the predecessor at the last safe cut so a
		// stale observation can never authorize replacement.
		current, readErr := ReadFile(directory, name)
		if readErr == nil && bytes.Equal(current, next) {
			return errors.Join(RemoveTemporary(directory, temporaryName, temporary),
				reconcileExactCandidates(directory, name, next))
		}
		if readErr != nil || !bytes.Equal(current, previous) {
			return errors.Join(checkpointmodel.ErrRecordBinding, readErr,
				RemoveTemporary(directory, temporaryName, temporary))
		}
		replaceErr := directory.ReplacePrivateFile(temporary, name)
		syncErr := directory.Sync()
		actual, readErr := ReadFile(directory, name)
		var closeErr, cleanupErr error
		if readErr == nil && bytes.Equal(actual, next) {
			// Exact bytes are the restart boundary. A reported replace/sync error is
			// still returned so the caller can pause, but the next opener may adopt
			// this authenticated image.
			closeErr = closeFile(temporary)
			return errors.Join(replaceErr, syncErr, closeErr,
				reconcileExactCandidates(directory, name, next))
		}
		if replaceErr == nil {
			closeErr = closeFile(temporary)
		}
		// ReplacePrivateFile consumes the private temporary name on success. On a
		// failed replacement the helper removes the still-owned temporary and
		// closes its capability exactly once.
		if replaceErr != nil {
			cleanupErr = RemoveTemporary(directory, temporaryName, temporary)
		}
		return errors.Join(
			outputcap.ErrUnsafeNamespace, checkpointmodel.ErrRecordCrashBoundary,
			replaceErr, syncErr, readErr, closeErr, cleanupErr,
		)
	}
	return fmt.Errorf("%w: checkpoint replacement temporary allocation exhausted", outputcap.ErrUnsafeNamespace)
}

// reconcileExactCandidates removes only deterministic names whose exact image
// is already installed at the fixed target. Foreign names and foreign bytes are
// preserved and fail closed for explicit attention.
func reconcileExactCandidates(directory outputcap.Directory, target string, encoded []byte) error {
	if directory == nil || target == "" || len(encoded) == 0 || len(encoded) > maxFileCheckpointBytes {
		return checkpointmodel.ErrInvalidRecord
	}
	for attempt := range installationAttempts {
		name := TemporaryName(target, encoded, attempt)
		kind, exact, err := directory.ClassifyExactEntry(name)
		if err != nil {
			return err
		}
		if kind == outputcap.EntryAbsent {
			continue
		}
		if !exact || kind != outputcap.EntryRegularFile {
			return outputcap.ErrUnsafeNamespace
		}
		if err := RemoveExactTemporary(directory, name, encoded); err != nil {
			return err
		}
	}
	return nil
}

func openExactTemporary(
	directory outputcap.Directory,
	name string,
	expected []byte,
) (outputcap.File, error) {
	var mismatchErr error
	var lastObserved []byte
	for range candidateReadAttempts {
		file, err := directory.OpenFile(name, true, false)
		if err != nil {
			closeErr := closeFile(file)
			if candidateContention(err) {
				return nil, errors.Join(outputcap.ErrNamespaceLockBusy, err, closeErr)
			}
			return nil, errors.Join(err, closeErr)
		}
		actual, readErr := readBoundedFile(file)
		if readErr != nil {
			closeErr := closeFile(file)
			if candidateContention(readErr) {
				// Windows can admit the handle and still reject the first read while
				// the creating writer owns the candidate. That is contention, not
				// evidence that the deterministic image is corrupt.
				return nil, errors.Join(outputcap.ErrNamespaceLockBusy, readErr, closeErr)
			}
			return nil, errors.Join(checkpointmodel.ErrRecordBinding, readErr, closeErr)
		}
		if bytes.Equal(actual, expected) {
			return file, nil
		}
		lastObserved = actual
		mismatchErr = checkpointmodel.ErrRecordBinding
		if closeErr := closeFile(file); closeErr != nil {
			return nil, errors.Join(mismatchErr, closeErr)
		}
		// CreatePrivateFile exposes the correctly named, pre-sized entry before
		// its first write. Yielding lets that owner finish without trusting an
		// image that remains inexact across the bounded observation window.
		runtime.Gosched()
	}
	if candidateWriteInFlight(lastObserved, expected) {
		return nil, errors.Join(outputcap.ErrNamespaceLockBusy, mismatchErr)
	}
	return nil, mismatchErr
}

func candidateWriteInFlight(actual, expected []byte) bool {
	if len(actual) != len(expected) || bytes.Equal(actual, expected) {
		return false
	}
	mismatch := false
	for index := range actual {
		if !mismatch && actual[index] == expected[index] {
			continue
		}
		mismatch = true
		if actual[index] != 0 {
			return false
		}
	}
	return mismatch
}

func candidateContention(err error) bool {
	return err != nil && (errors.Is(err, outputcap.ErrNamespaceLockBusy) ||
		errors.Is(err, outputcap.ErrFixedLinkSourceChanged) || platformCandidateContention(err))
}

func FileFor(directory outputcap.Directory, recordID checkpointmodel.RecordID, create bool) (outputcap.Directory, string, error) {
	shardName, recordName := recordLocation(recordID)
	shard, err := OpenShard(directory, shardName, create)
	if err != nil {
		return nil, "", err
	}
	return shard, recordName, nil
}
