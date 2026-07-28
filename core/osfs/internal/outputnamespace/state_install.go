package outputnamespace

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

type createInstallResult struct {
	outcome CreateOutcome
	cut     StateInstallCut
}

func createStateRecordWithTemporary(
	directory outputcap.Directory,
	targetName string,
	temporaryName string,
	temporary outputcap.File,
	encoded []byte,
	limit int,
) (createInstallResult, error) {
	written, err := temporary.WriteAt(encoded, 0)
	if err == nil && written != len(encoded) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return createInstallResult{outcome: CreateNotInstalled}, err
	}
	if err := temporary.Sync(); err != nil {
		return createInstallResult{outcome: CreateNotInstalled}, err
	}
	if err := verifyNamedStateFile(directory, temporaryName, temporary, encoded, limit); err != nil {
		return createInstallResult{outcome: CreateNotInstalled}, err
	}

	target, linkErr := directory.LinkFileNoReplace(temporary, targetName)
	cut := StateInstallCut{
		mutationReportedFailure: linkErr != nil,
	}
	var identityErr error
	if target == nil {
		if linkErr == nil {
			identityErr = fmt.Errorf("%w: state creation returned no fixed target", outputcap.ErrUnsafeNamespace)
		}
	} else {
		same, compareErr := target.SameFile(temporary)
		if compareErr != nil || !same {
			identityErr = errors.Join(outputcap.ErrUnsafeNamespace, compareErr)
		}
	}
	syncErr := directory.Sync()
	cut.parentSyncReportedFailure = syncErr != nil
	reopened := ReadRecordWithCleanup(directory, targetName, limit)
	actual, reopenErr, reopenCloseErr := reopened.Encoded, reopened.ReadError, reopened.CloseError
	targetCloseErr := closeFile(target)
	if identityErr == nil && reopenErr == nil && bytes.Equal(actual, encoded) {
		// Exact fixed-name bytes settle link and parent-sync reports at the
		// process-restart durability boundary. Handle cleanup is deliberately not
		// settled because the current owner still has to release that authority.
		return createInstallResult{outcome: CreateAdopted, cut: cut}, errors.Join(reopenCloseErr, targetCloseErr)
	}
	if target == nil && linkErr != nil &&
		!errors.Is(linkErr, outputcap.ErrNamespaceCollision) && !errors.Is(linkErr, outputcap.ErrUnsafeNamespace) &&
		errors.Is(reopenErr, fs.ErrNotExist) {
		return createInstallResult{outcome: CreateNotInstalled, cut: cut}, errors.Join(linkErr, syncErr, reopenCloseErr)
	}
	if reopenErr == nil && !bytes.Equal(actual, encoded) {
		reopenErr = fmt.Errorf("%w: installed state record differs from creation image", outputcap.ErrUnsafeNamespace)
	}
	return createInstallResult{outcome: CreateUncertain, cut: cut}, errors.Join(
		outputcap.ErrUnsafeNamespace,
		errors.New("osfs: state creation authority is uncertain"),
		identityErr,
		linkErr,
		syncErr,
		reopenErr,
		reopenCloseErr,
		targetCloseErr,
	)
}

func (store Store) ReplaceRecord(
	directory outputcap.Directory,
	name string,
	current RecordImage,
	next RecordImage,
	limit int,
) (ReplaceOutcome, error) {
	if err := validateStateReplacement(name, current, next, limit); err != nil {
		return ReplaceUnchanged, err
	}
	for range AllocationAttempts {
		temporaryName, err := store.temporaryName(name)
		if err != nil {
			return ReplaceUnchanged, err
		}
		temporary, err := directory.CreateFile(temporaryName, true, int64(len(next.encoded)))
		if errors.Is(err, outputcap.ErrNamespaceCollision) {
			if closeErr := closeFile(temporary); closeErr != nil {
				return ReplaceUnchanged, errors.Join(err, closeErr)
			}
			continue
		}
		if err != nil {
			return ReplaceUnchanged, errors.Join(err, closeFile(temporary))
		}
		attempt, replaceErr := replaceStateRecordWithTemporary(
			directory, name, temporaryName, temporary, current, next, limit,
		)
		outcome, cut := attempt.outcome, attempt.cut
		if outcome == ReplaceAdopted && store.observer != nil &&
			(cut.mutationReportedFailure || cut.parentSyncReportedFailure) {
			cut.stage = StateInstallReplace
			cut.targetName = name
			cut.encoded = next.encoded
			store.observer.ObserveStateInstall(cut)
		}
		var cleanupErr error
		if outcome == ReplaceUnchanged {
			cleanupErr = removeExactStateTemporary(directory, temporaryName, temporary)
		}
		closeErr := temporary.Close()
		if outcome == ReplaceAdopted {
			// At process-restart durability, an exact reopen of the installed image
			// is sufficient adoption proof even if a preceding sync reported failure.
			return outcome, errors.Join(replaceErr, cleanupErr, closeErr)
		}
		return outcome, errors.Join(replaceErr, cleanupErr, closeErr)
	}
	return ReplaceUnchanged, fmt.Errorf("%w: allocate state update temporary", outputcap.ErrUnsafeNamespace)
}

type replaceInstallResult struct {
	outcome ReplaceOutcome
	cut     StateInstallCut
}

func replaceStateRecordWithTemporary(
	directory outputcap.Directory,
	targetName string,
	temporaryName string,
	temporary outputcap.File,
	current RecordImage,
	next RecordImage,
	limit int,
) (replaceInstallResult, error) {
	written, err := temporary.WriteAt(next.encoded, 0)
	if err == nil && written != len(next.encoded) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return replaceInstallResult{outcome: ReplaceUnchanged}, err
	}
	if err := temporary.Sync(); err != nil {
		return replaceInstallResult{outcome: ReplaceUnchanged}, err
	}
	if err := verifyNamedStateFile(directory, temporaryName, temporary, next.encoded, limit); err != nil {
		return replaceInstallResult{outcome: ReplaceUnchanged}, err
	}
	installedRecord := ReadRecordWithCleanup(directory, targetName, limit)
	installed, readErr, closeErr := installedRecord.Encoded, installedRecord.ReadError, installedRecord.CloseError
	if readErr != nil || !bytes.Equal(installed, current.encoded) {
		return replaceInstallResult{outcome: ReplaceUncertain}, errors.Join(
			outputcap.ErrUnsafeNamespace, fmt.Errorf("pre-install state authority: %w", readErr), closeErr,
		)
	}
	if closeErr != nil {
		// The fixed target was byte-verified as the current generation before any
		// mutation. A cleanup failure cannot be mistaken for an installed next
		// generation, so the caller retains current authority and pauses.
		return replaceInstallResult{outcome: ReplaceUnchanged}, closeErr
	}

	replaceErr := directory.ReplacePrivateFile(temporary, targetName)
	syncErr := directory.Sync()
	cut := StateInstallCut{
		mutationReportedFailure:   replaceErr != nil,
		parentSyncReportedFailure: syncErr != nil,
	}
	reopened := ReadRecordWithCleanup(directory, targetName, limit)
	actual, reopenErr, reopenCloseErr := reopened.Encoded, reopened.ReadError, reopened.CloseError
	if reopenErr == nil && bytes.Equal(actual, next.encoded) {
		// Exact next-generation bytes settle replace and sync errors at the
		// process-restart durability boundary. Only handle cleanup remains an
		// operational failure for the current owner.
		return replaceInstallResult{outcome: ReplaceAdopted, cut: cut}, reopenCloseErr
	}
	if replaceErr != nil && reopenErr == nil && bytes.Equal(actual, current.encoded) {
		return replaceInstallResult{outcome: ReplaceUnchanged, cut: cut}, errors.Join(replaceErr, syncErr, reopenCloseErr)
	}
	if reopenErr == nil && !bytes.Equal(actual, current.encoded) && !bytes.Equal(actual, next.encoded) {
		reopenErr = fmt.Errorf("%w: installed state record is neither expected generation", outputcap.ErrUnsafeNamespace)
	}
	return replaceInstallResult{outcome: ReplaceUncertain, cut: cut}, errors.Join(
		outputcap.ErrUnsafeNamespace,
		errors.New("osfs: state replacement authority is uncertain"),
		replaceErr,
		syncErr,
		reopenErr,
		reopenCloseErr,
	)
}

func validateStateReplacement(
	targetName string,
	current RecordImage,
	next RecordImage,
	limit int,
) error {
	if err := validateEncodedState(current.encoded, limit); err != nil {
		return err
	}
	if err := validateEncodedState(next.encoded, limit); err != nil {
		return err
	}
	currentGeneration, err := DecodeRecordGeneration(targetName, current.encoded)
	if err != nil || currentGeneration != current.generation {
		return errors.Join(fmt.Errorf("%w: current state replacement image", resumestate.ErrInvalidState), err)
	}
	nextGeneration, err := DecodeRecordGeneration(targetName, next.encoded)
	if err != nil || nextGeneration != next.generation {
		return errors.Join(fmt.Errorf("%w: next state replacement image", resumestate.ErrInvalidState), err)
	}
	if current.generation == 0 || current.generation == math.MaxUint64 ||
		next.generation != current.generation+1 || bytes.Equal(current.encoded, next.encoded) {
		return fmt.Errorf("%w: state replacement generation", resumestate.ErrInvalidTransition)
	}
	return nil
}

func DecodeRecordGeneration(targetName string, encoded []byte) (uint64, error) {
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
