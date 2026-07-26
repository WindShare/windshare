package osfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

const (
	outputStateAllocationAttempts  = 16
	outputFileShardInspectionLimit = resumestate.MaxFileStateEntriesPerSession + 1
)

type outputStateStore struct {
	random              io.Reader
	traceAdoptedInstall func(outputStateInstallCut)
}

type outputStateInstallCut struct {
	stage                     FilesystemOutputStateInstallStage
	targetName                string
	encoded                   []byte
	mutationReportedFailure   bool
	parentSyncReportedFailure bool
}

type outputStateRecordImage struct {
	encoded    []byte
	generation uint64
}

type outputStateCreateOutcome uint8

const (
	// NotInstalled means the fixed target did not adopt the requested image.
	// The caller retains no authority derived from this creation attempt.
	outputStateCreateNotInstalled outputStateCreateOutcome = iota + 1
	// Adopted means the fixed target was reopened and byte-verified as the
	// requested image. Cleanup errors do not change that durable authority.
	outputStateCreateAdopted
	// Uncertain means mutation was attempted but the fixed target could not be
	// classified as either absent or the requested image.
	outputStateCreateUncertain
)

type outputStateReplaceOutcome uint8

const (
	// Unchanged is returned only when the fixed target has been reopened and
	// byte-verified as the caller's current generation after any attempted replace.
	outputStateReplaceUnchanged outputStateReplaceOutcome = iota + 1
	// Adopted means the fixed target has been reopened and byte-verified as the
	// next generation. The caller must advance its in-memory authority.
	outputStateReplaceAdopted
	// Uncertain permanently invalidates the current owner. Only a fresh namespace
	// reopen may decide which generation is authoritative.
	outputStateReplaceUncertain
)

func (store outputStateStore) createRecord(
	directory outputV3Directory,
	name string,
	encoded []byte,
	limit int,
) (outputStateCreateOutcome, error) {
	if err := validateEncodedState(encoded, limit); err != nil {
		return outputStateCreateNotInstalled, err
	}
	for range outputStateAllocationAttempts {
		temporaryName, err := store.temporaryName(name)
		if err != nil {
			return outputStateCreateNotInstalled, err
		}
		temporary, err := directory.CreateFile(temporaryName, true, int64(len(encoded)))
		if errors.Is(err, errOutputV3Collision) {
			if closeErr := closeOutputV3File(temporary); closeErr != nil {
				return outputStateCreateNotInstalled, errors.Join(err, closeErr)
			}
			continue
		}
		if err != nil {
			return outputStateCreateNotInstalled, errors.Join(err, closeOutputV3File(temporary))
		}
		outcome, installErr, cut := createStateRecordWithTemporary(
			directory, name, temporaryName, temporary, encoded, limit,
		)
		if outcome == outputStateCreateAdopted && store.traceAdoptedInstall != nil &&
			(cut.mutationReportedFailure || cut.parentSyncReportedFailure) {
			cut.stage = FilesystemOutputStateCreate
			cut.targetName = name
			cut.encoded = encoded
			store.traceAdoptedInstall(cut)
		}
		var cleanupErr error
		if outcome != outputStateCreateUncertain {
			cleanupErr = removeExactStateTemporary(directory, temporaryName, temporary)
		}
		return outcome, errors.Join(installErr, cleanupErr, temporary.Close())
	}
	return outputStateCreateNotInstalled, fmt.Errorf("%w: allocate state creation temporary", errOutputV3Unsafe)
}

func createStateRecordWithTemporary(
	directory outputV3Directory,
	targetName string,
	temporaryName string,
	temporary outputV3File,
	encoded []byte,
	limit int,
) (outputStateCreateOutcome, error, outputStateInstallCut) {
	written, err := temporary.WriteAt(encoded, 0)
	if err == nil && written != len(encoded) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return outputStateCreateNotInstalled, err, outputStateInstallCut{}
	}
	if err := temporary.Sync(); err != nil {
		return outputStateCreateNotInstalled, err, outputStateInstallCut{}
	}
	if err := verifyNamedStateFile(directory, temporaryName, temporary, encoded, limit); err != nil {
		return outputStateCreateNotInstalled, err, outputStateInstallCut{}
	}

	target, linkErr := directory.LinkFileNoReplace(temporary, targetName)
	cut := outputStateInstallCut{
		mutationReportedFailure: linkErr != nil,
	}
	var identityErr error
	if target == nil {
		if linkErr == nil {
			identityErr = fmt.Errorf("%w: state creation returned no fixed target", errOutputV3Unsafe)
		}
	} else {
		same, compareErr := target.SameFile(temporary)
		if compareErr != nil || !same {
			identityErr = errors.Join(errOutputV3Unsafe, compareErr)
		}
	}
	syncErr := directory.Sync()
	cut.parentSyncReportedFailure = syncErr != nil
	actual, reopenErr, reopenCloseErr := readStateRecordWithCleanup(directory, targetName, limit)
	targetCloseErr := closeOutputV3File(target)
	if identityErr == nil && reopenErr == nil && bytes.Equal(actual, encoded) {
		// Exact fixed-name bytes settle link and parent-sync reports at the
		// process-restart durability boundary. Handle cleanup is deliberately not
		// settled because the current owner still has to release that authority.
		return outputStateCreateAdopted, errors.Join(reopenCloseErr, targetCloseErr), cut
	}
	if target == nil && linkErr != nil &&
		!errors.Is(linkErr, errOutputV3Collision) && !errors.Is(linkErr, errOutputV3Unsafe) &&
		errors.Is(reopenErr, fs.ErrNotExist) {
		return outputStateCreateNotInstalled, errors.Join(linkErr, syncErr, reopenCloseErr), cut
	}
	if reopenErr == nil && !bytes.Equal(actual, encoded) {
		reopenErr = fmt.Errorf("%w: installed state record differs from creation image", errOutputV3Unsafe)
	}
	return outputStateCreateUncertain, errors.Join(
		errOutputV3Unsafe,
		errors.New("osfs: state creation authority is uncertain"),
		identityErr,
		linkErr,
		syncErr,
		reopenErr,
		reopenCloseErr,
		targetCloseErr,
	), cut
}

func (store outputStateStore) replaceRecord(
	directory outputV3Directory,
	name string,
	current outputStateRecordImage,
	next outputStateRecordImage,
	limit int,
) (outputStateReplaceOutcome, error) {
	if err := validateStateReplacement(name, current, next, limit); err != nil {
		return outputStateReplaceUnchanged, err
	}
	for range outputStateAllocationAttempts {
		temporaryName, err := store.temporaryName(name)
		if err != nil {
			return outputStateReplaceUnchanged, err
		}
		temporary, err := directory.CreateFile(temporaryName, true, int64(len(next.encoded)))
		if errors.Is(err, errOutputV3Collision) {
			if closeErr := closeOutputV3File(temporary); closeErr != nil {
				return outputStateReplaceUnchanged, errors.Join(err, closeErr)
			}
			continue
		}
		if err != nil {
			return outputStateReplaceUnchanged, errors.Join(err, closeOutputV3File(temporary))
		}
		outcome, replaceErr, cut := replaceStateRecordWithTemporary(
			directory, name, temporaryName, temporary, current, next, limit,
		)
		if outcome == outputStateReplaceAdopted && store.traceAdoptedInstall != nil &&
			(cut.mutationReportedFailure || cut.parentSyncReportedFailure) {
			cut.stage = FilesystemOutputStateReplace
			cut.targetName = name
			cut.encoded = next.encoded
			store.traceAdoptedInstall(cut)
		}
		var cleanupErr error
		if outcome == outputStateReplaceUnchanged {
			cleanupErr = removeExactStateTemporary(directory, temporaryName, temporary)
		}
		closeErr := temporary.Close()
		if outcome == outputStateReplaceAdopted {
			// At process-restart durability, an exact reopen of the installed image
			// is sufficient adoption proof even if a preceding sync reported failure.
			return outcome, errors.Join(replaceErr, cleanupErr, closeErr)
		}
		return outcome, errors.Join(replaceErr, cleanupErr, closeErr)
	}
	return outputStateReplaceUnchanged, fmt.Errorf("%w: allocate state update temporary", errOutputV3Unsafe)
}

func replaceStateRecordWithTemporary(
	directory outputV3Directory,
	targetName string,
	temporaryName string,
	temporary outputV3File,
	current outputStateRecordImage,
	next outputStateRecordImage,
	limit int,
) (outputStateReplaceOutcome, error, outputStateInstallCut) {
	written, err := temporary.WriteAt(next.encoded, 0)
	if err == nil && written != len(next.encoded) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return outputStateReplaceUnchanged, err, outputStateInstallCut{}
	}
	if err := temporary.Sync(); err != nil {
		return outputStateReplaceUnchanged, err, outputStateInstallCut{}
	}
	if err := verifyNamedStateFile(directory, temporaryName, temporary, next.encoded, limit); err != nil {
		return outputStateReplaceUnchanged, err, outputStateInstallCut{}
	}
	installed, readErr, closeErr := readStateRecordWithCleanup(directory, targetName, limit)
	if readErr != nil || !bytes.Equal(installed, current.encoded) {
		return outputStateReplaceUncertain, errors.Join(
			errOutputV3Unsafe, fmt.Errorf("pre-install state authority: %w", readErr), closeErr,
		), outputStateInstallCut{}
	}
	if closeErr != nil {
		// The fixed target was byte-verified as the current generation before any
		// mutation. A cleanup failure cannot be mistaken for an installed next
		// generation, so the caller retains current authority and pauses.
		return outputStateReplaceUnchanged, closeErr, outputStateInstallCut{}
	}

	replaceErr := directory.ReplacePrivateFile(temporary, targetName)
	syncErr := directory.Sync()
	cut := outputStateInstallCut{
		mutationReportedFailure:   replaceErr != nil,
		parentSyncReportedFailure: syncErr != nil,
	}
	actual, reopenErr, reopenCloseErr := readStateRecordWithCleanup(directory, targetName, limit)
	if reopenErr == nil && bytes.Equal(actual, next.encoded) {
		// Exact next-generation bytes settle replace and sync errors at the
		// process-restart durability boundary. Only handle cleanup remains an
		// operational failure for the current owner.
		return outputStateReplaceAdopted, reopenCloseErr, cut
	}
	if replaceErr != nil && reopenErr == nil && bytes.Equal(actual, current.encoded) {
		return outputStateReplaceUnchanged, errors.Join(replaceErr, syncErr, reopenCloseErr), cut
	}
	if reopenErr == nil && !bytes.Equal(actual, current.encoded) && !bytes.Equal(actual, next.encoded) {
		reopenErr = fmt.Errorf("%w: installed state record is neither expected generation", errOutputV3Unsafe)
	}
	return outputStateReplaceUncertain, errors.Join(
		errOutputV3Unsafe,
		errors.New("osfs: state replacement authority is uncertain"),
		replaceErr,
		syncErr,
		reopenErr,
		reopenCloseErr,
	), cut
}

func validateStateReplacement(
	targetName string,
	current outputStateRecordImage,
	next outputStateRecordImage,
	limit int,
) error {
	if err := validateEncodedState(current.encoded, limit); err != nil {
		return err
	}
	if err := validateEncodedState(next.encoded, limit); err != nil {
		return err
	}
	currentGeneration, err := decodeStateRecordGeneration(targetName, current.encoded)
	if err != nil || currentGeneration != current.generation {
		return errors.Join(fmt.Errorf("%w: current state replacement image", resumestate.ErrInvalidState), err)
	}
	nextGeneration, err := decodeStateRecordGeneration(targetName, next.encoded)
	if err != nil || nextGeneration != next.generation {
		return errors.Join(fmt.Errorf("%w: next state replacement image", resumestate.ErrInvalidState), err)
	}
	if current.generation == 0 || current.generation == math.MaxUint64 ||
		next.generation != current.generation+1 || bytes.Equal(current.encoded, next.encoded) {
		return fmt.Errorf("%w: state replacement generation", resumestate.ErrInvalidTransition)
	}
	return nil
}

func decodeStateRecordGeneration(targetName string, encoded []byte) (uint64, error) {
	if targetName == resumestate.ControlRecordName {
		control, err := resumestate.DecodeControl(encoded)
		if err != nil {
			return 0, err
		}
		return control.Generation(), nil
	}
	if targetName == resumestate.HeaderRecordName {
		header, err := resumestate.DecodeHeader(encoded)
		if err != nil {
			return 0, err
		}
		return header.StateGeneration(), nil
	}
	if len(targetName) < resumestate.ShardHexCharacters {
		return 0, fmt.Errorf("%w: state replacement target", resumestate.ErrInvalidState)
	}
	digest, err := resumestate.ParseFileRecordName(targetName[:resumestate.ShardHexCharacters], targetName)
	if err != nil {
		return 0, err
	}
	record, err := resumestate.DecodeFileRecord(encoded)
	if err != nil {
		return 0, err
	}
	if record.LocatorDigest() != digest {
		return 0, fmt.Errorf("%w: file state replacement target", resumestate.ErrInvalidState)
	}
	return record.StateGeneration(), nil
}

func removeExactStateTemporary(
	directory outputV3Directory,
	name string,
	temporary outputV3File,
) error {
	kind, err := observeExactOutputEntry(directory, name)
	if err != nil {
		return err
	}
	if kind == outputV3EntryAbsent {
		return nil
	}
	if kind != outputV3EntryRegularFile {
		return errOutputV3Unsafe
	}
	return errors.Join(directory.RemoveFile(name, temporary), directory.Sync())
}

func verifyNamedStateFile(
	directory outputV3Directory,
	name string,
	expectedFile outputV3File,
	expectedBytes []byte,
	limit int,
) error {
	reopened, err := directory.OpenFile(name, true, false)
	if err != nil {
		return errors.Join(err, closeOutputV3File(reopened))
	}
	same, compareErr := reopened.SameFile(expectedFile)
	actual, readErr := readStateFile(reopened, limit)
	closeErr := reopened.Close()
	if compareErr != nil || !same || readErr != nil || !bytes.Equal(actual, expectedBytes) {
		return errors.Join(errOutputV3Unsafe, compareErr, readErr, closeErr)
	}
	return closeErr
}

func (store outputStateStore) temporaryName(target string) (string, error) {
	nonce, err := resumestate.GenerateUpdateNonce(store.random)
	if err != nil {
		return "", err
	}
	return resumestate.RecordUpdateTemporaryName(target, nonce)
}

func (store outputStateStore) ensureInitialRecord(
	directory outputV3Directory,
	name string,
	encoded []byte,
	limit int,
) (outputStateCreateOutcome, error) {
	if err := validateEncodedState(encoded, limit); err != nil {
		return outputStateCreateNotInstalled, err
	}
	kind, err := observeExactOutputEntry(directory, name)
	if err != nil {
		return outputStateCreateNotInstalled, err
	}
	if kind != outputV3EntryAbsent && kind != outputV3EntryRegularFile {
		return outputStateCreateUncertain, errOutputV3Unsafe
	}
	if kind == outputV3EntryRegularFile {
		actual, readErr, closeErr := readStateRecordWithCleanup(directory, name, limit)
		if readErr != nil || !bytes.Equal(actual, encoded) {
			return outputStateCreateUncertain, errors.Join(errOutputV3Unsafe, readErr, closeErr)
		}
		if closeErr != nil {
			return outputStateCreateAdopted, closeErr
		}
	}
	if err := removeInitialRecordTemporaries(directory, name, nil); err != nil {
		if kind == outputV3EntryRegularFile {
			return outputStateCreateAdopted, err
		}
		return outputStateCreateNotInstalled, err
	}
	if kind == outputV3EntryRegularFile {
		return outputStateCreateAdopted, nil
	}
	return store.createRecord(directory, name, encoded, limit)
}

func removeInitialRecordTemporaries(
	directory outputV3Directory,
	target string,
	verifyAuthority func() error,
) error {
	prefix, err := resumestate.RecordUpdateTemporaryPrefix(target)
	if err != nil {
		return err
	}
	names, err := directory.NamesWithPrefix(prefix, outputStateAllocationAttempts+1)
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, err := resumestate.ParseRecordUpdateTemporaryName(target, name); err != nil {
			return errOutputV3Unsafe
		}
		kind, err := observeExactOutputEntry(directory, name)
		if err != nil || kind != outputV3EntryRegularFile {
			return errors.Join(errOutputV3Unsafe, err)
		}
		temporary, err := directory.OpenFile(name, true, false)
		if err != nil {
			return errors.Join(err, closeOutputV3File(temporary))
		}
		if verifyAuthority != nil {
			if err := verifyAuthority(); err != nil {
				return errors.Join(err, temporary.Close())
			}
		}
		if err := directory.RemoveFile(name, temporary); err != nil {
			return errors.Join(err, temporary.Close())
		}
		if err := errors.Join(directory.Sync(), temporary.Close()); err != nil {
			return err
		}
	}
	return nil
}

func reconcileHeaderRecordTemporaries(
	directory outputV3Directory,
	namespace resumestate.SessionNamespaceAuthority,
	verifyAuthority func() error,
) error {
	if verifyAuthority == nil {
		return fmt.Errorf("%w: header temporary recovery authority", resumestate.ErrInvalidState)
	}
	installed, err := resumestate.EncodeHeader(namespace.Header())
	if err != nil {
		return err
	}
	verifyInstalled := func() error {
		if err := verifyAuthority(); err != nil {
			return err
		}
		return verifyStateRecord(
			directory, resumestate.HeaderRecordName, installed, resumestate.MaxSessionHeaderBytes,
		)
	}
	if err := verifyInstalled(); err != nil {
		return err
	}
	names, err := directory.NamesWithPrefix(
		resumestate.HeaderUpdateTemporaryPrefix, outputStateAllocationAttempts+1,
	)
	if err != nil {
		return err
	}
	for _, name := range names {
		classified := resumestate.ClassifyHeaderUpdateTemporaryName(name)
		kind, observeErr := observeExactOutputEntry(directory, name)
		if observeErr != nil {
			return observeErr
		}
		entry := resumestate.UpdateTemporaryEntryUnsafe
		if kind == outputV3EntryAbsent {
			entry = resumestate.UpdateTemporaryEntryMissing
		} else if kind == outputV3EntryRegularFile {
			entry = resumestate.UpdateTemporaryEntryRegular
		}

		var temporary outputV3File
		var candidate *resumestate.Header
		if entry == resumestate.UpdateTemporaryEntryRegular {
			temporary, err = directory.OpenFile(name, true, false)
			if err != nil {
				return errors.Join(err, closeOutputV3File(temporary))
			}
			encoded, readErr := readStateFile(temporary, resumestate.MaxSessionHeaderBytes)
			if readErr == nil {
				decoded, decodeErr := resumestate.DecodeHeader(encoded)
				if decodeErr == nil {
					candidate = &decoded
				}
			}
		}

		decision, reduceErr := resumestate.ReduceHeaderUpdateTemporary(
			namespace, classified, entry, candidate,
		)
		if reduceErr != nil {
			return errors.Join(reduceErr, closeOutputV3File(temporary))
		}
		switch decision.Action() {
		case resumestate.HeaderUpdateTemporaryAcceptInstalledHeader:
			if err := closeOutputV3File(temporary); err != nil {
				return err
			}
			continue
		case resumestate.HeaderUpdateTemporaryRemoveAndSyncSession:
			if err := verifyInstalled(); err != nil {
				return errors.Join(err, closeOutputV3File(temporary))
			}
			if err := decision.AuthorizeRemoval(
				namespace, name, resumestate.UpdateTemporaryEntryRegular,
			); err != nil {
				return errors.Join(err, closeOutputV3File(temporary))
			}
			if err := directory.RemoveFile(name, temporary); err != nil {
				return errors.Join(err, closeOutputV3File(temporary))
			}
			if err := errors.Join(directory.Sync(), temporary.Close()); err != nil {
				return err
			}
		case resumestate.HeaderUpdateTemporaryBlockResumeNamespace:
			return errors.Join(errOutputIntentUnsafe, closeOutputV3File(temporary))
		default:
			return errors.Join(
				fmt.Errorf("%w: header temporary recovery action", resumestate.ErrInvalidState),
				closeOutputV3File(temporary),
			)
		}
	}
	return nil
}

func readStateRecord(directory outputV3Directory, name string, limit int) ([]byte, error) {
	encoded, readErr, closeErr := readStateRecordWithCleanup(directory, name, limit)
	return encoded, errors.Join(readErr, closeErr)
}

// readStateRecordWithCleanup keeps namespace observation separate from handle
// cleanup. Install classifiers need the exact bytes even when Close fails so
// they can retain or adopt the generation that was actually reopened.
func readStateRecordWithCleanup(
	directory outputV3Directory,
	name string,
	limit int,
) ([]byte, error, error) {
	kind, err := observeExactOutputEntry(directory, name)
	if err != nil {
		return nil, err, nil
	}
	if kind == outputV3EntryAbsent {
		return nil, fs.ErrNotExist, nil
	}
	if kind != outputV3EntryRegularFile {
		return nil, errOutputV3Unsafe, nil
	}
	file, err := directory.OpenFile(name, true, false)
	if err != nil {
		return nil, err, closeOutputV3File(file)
	}
	encoded, readErr := readStateFile(file, limit)
	return encoded, readErr, file.Close()
}

func readStateFile(file outputV3File, limit int) ([]byte, error) {
	if file == nil || limit <= 0 {
		return nil, fmt.Errorf("%w: state record handle", errOutputV3Unsafe)
	}
	size, err := file.Size()
	if err != nil {
		return nil, err
	}
	if size == 0 || size > uint64(limit) {
		return nil, fmt.Errorf("%w: state record size", errOutputV3Unsafe)
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

func verifyStateRecord(directory outputV3Directory, name string, expected []byte, limit int) error {
	actual, err := readStateRecord(directory, name, limit)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) {
		return fmt.Errorf("%w: installed state record differs after reopen", errOutputV3Unsafe)
	}
	return nil
}

func validateEncodedState(encoded []byte, limit int) error {
	if len(encoded) == 0 || limit <= 0 || len(encoded) > limit {
		return fmt.Errorf("%w: encoded state exceeds its bound", errOutputV3Unsafe)
	}
	return nil
}

func ensureOutputDirectory(parent outputV3Directory, name string, private bool) (outputV3Directory, bool, error) {
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil {
		return nil, false, err
	}
	if kind != outputV3EntryAbsent {
		if !exact || kind != outputV3EntryDirectory {
			return nil, false, errOutputV3Unsafe
		}
		directory, err := parent.OpenDirectory(name, private)
		if err != nil {
			return nil, false, errors.Join(
				errOutputV3PositiveEntryEvidence, err, closeOutputV3Directory(directory),
			)
		}
		return directory, false, err
	}
	directory, err := parent.CreateDirectory(name, private)
	if errors.Is(err, errOutputV3Collision) {
		collisionErr := err
		directory, err = parent.OpenDirectory(name, private)
		if err != nil {
			return nil, false, errors.Join(
				errOutputV3PositiveEntryEvidence, collisionErr, err, closeOutputV3Directory(directory),
			)
		}
		return directory, false, err
	}
	if err != nil {
		return nil, false, errors.Join(err, closeOutputV3Directory(directory))
	}
	// A returned directory is immediately usable as authority. Persisting its
	// parent entry here prevents callers from accidentally relying on an
	// unsynchronised namespace cut.
	if err := errors.Join(directory.Sync(), parent.Sync()); err != nil {
		return nil, false, errors.Join(err, directory.Close())
	}
	return directory, true, nil
}

func openOptionalOutputDirectory(
	directory outputV3Directory,
	name string,
	private bool,
) (outputV3Directory, bool, error) {
	kind, exact, err := directory.ClassifyExactEntry(name)
	if err != nil {
		return nil, false, err
	}
	if kind == outputV3EntryAbsent {
		return nil, false, nil
	}
	if !exact || kind != outputV3EntryDirectory {
		return nil, false, errOutputV3Unsafe
	}
	opened, err := directory.OpenDirectory(name, private)
	if err != nil {
		return nil, false, errors.Join(errOutputV3PositiveEntryEvidence, err, closeOutputV3Directory(opened))
	}
	return opened, true, nil
}

func observeExactOutputEntry(directory outputV3Directory, name string) (outputV3EntryKind, error) {
	kind, exact, err := directory.ClassifyExactEntry(name)
	if err != nil {
		return outputV3EntryAbsent, err
	}
	if kind != outputV3EntryAbsent && !exact {
		return kind, errOutputV3Unsafe
	}
	return kind, nil
}
