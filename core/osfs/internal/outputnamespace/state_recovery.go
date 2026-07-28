package outputnamespace

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

func removeExactStateTemporary(
	directory outputcap.Directory,
	name string,
	temporary outputcap.File,
) error {
	kind, err := ObserveExactEntry(directory, name)
	if err != nil {
		return err
	}
	if kind == outputcap.EntryAbsent {
		return nil
	}
	if kind != outputcap.EntryRegularFile {

		return outputcap.ErrUnsafeNamespace
	}
	return errors.Join(directory.RemoveFile(name, temporary), directory.Sync())
}

func verifyNamedStateFile(
	directory outputcap.Directory,
	name string,
	expectedFile outputcap.File,
	expectedBytes []byte,
	limit int,
) error {
	reopened, err := directory.OpenFile(name, true, false)
	if err != nil {
		return errors.Join(err, closeFile(reopened))
	}
	same, compareErr := reopened.SameFile(expectedFile)
	actual, readErr := ReadFile(reopened, limit)
	closeErr := reopened.Close()
	if compareErr != nil || !same || readErr != nil || !bytes.Equal(actual, expectedBytes) {
		return errors.Join(outputcap.ErrUnsafeNamespace, compareErr, readErr, closeErr)
	}
	return closeErr
}

func (store Store) temporaryName(target string) (string, error) {
	nonce, err := resumestate.GenerateUpdateNonce(store.random)
	if err != nil {
		return "", err
	}
	return resumestate.RecordUpdateTemporaryName(target, nonce)
}

func (store Store) EnsureInitialRecord(
	directory outputcap.Directory,
	name string,
	encoded []byte,
	limit int,
) (CreateOutcome, error) {
	if err := validateEncodedState(encoded, limit); err != nil {
		return CreateNotInstalled, err
	}
	kind, err := ObserveExactEntry(directory, name)
	if err != nil {
		return CreateNotInstalled, err
	}
	if kind != outputcap.EntryAbsent && kind != outputcap.EntryRegularFile {
		return CreateUncertain, outputcap.ErrUnsafeNamespace
	}
	if kind == outputcap.EntryRegularFile {
		readResult := ReadRecordWithCleanup(directory, name, limit)
		actual, readErr, closeErr := readResult.Encoded, readResult.ReadError, readResult.CloseError
		if readErr != nil || !bytes.Equal(actual, encoded) {
			return CreateUncertain, errors.Join(outputcap.ErrUnsafeNamespace, readErr, closeErr)
		}
		if closeErr != nil {
			return CreateAdopted, closeErr
		}
	}
	if err := RemoveInitialRecordTemporaries(directory, name, nil); err != nil {
		if kind == outputcap.EntryRegularFile {
			return CreateAdopted, err
		}
		return CreateNotInstalled, err
	}
	if kind == outputcap.EntryRegularFile {
		return CreateAdopted, nil
	}
	return store.CreateRecord(directory, name, encoded, limit)
}

func RemoveInitialRecordTemporaries(
	directory outputcap.Directory,
	target string,
	verifyAuthority func() error,
) error {
	prefix, err := resumestate.RecordUpdateTemporaryPrefix(target)
	if err != nil {
		return err
	}
	names, err := directory.NamesWithPrefix(prefix, AllocationAttempts+1)
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, err := resumestate.ParseRecordUpdateTemporaryName(target, name); err != nil {
			return outputcap.ErrUnsafeNamespace
		}
		kind, err := ObserveExactEntry(directory, name)
		if err != nil || kind != outputcap.EntryRegularFile {
			return errors.Join(outputcap.ErrUnsafeNamespace, err)
		}
		temporary, err := directory.OpenFile(name, true, false)
		if err != nil {
			return errors.Join(err, closeFile(temporary))
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

func ReconcileHeaderRecordTemporaries(
	directory outputcap.Directory,
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
		return VerifyRecord(
			directory, resumestate.HeaderRecordName, installed, resumestate.MaxSessionHeaderBytes,
		)
	}
	if err := verifyInstalled(); err != nil {
		return err
	}
	names, err := directory.NamesWithPrefix(
		resumestate.HeaderUpdateTemporaryPrefix, AllocationAttempts+1,
	)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := reconcileHeaderRecordTemporary(directory, namespace, name, verifyInstalled); err != nil {
			return err
		}
	}
	return nil
}

type headerTemporaryInspection struct {
	entry     resumestate.UpdateTemporaryEntryObservation
	temporary outputcap.File
	candidate *resumestate.Header
}

func reconcileHeaderRecordTemporary(
	directory outputcap.Directory,
	namespace resumestate.SessionNamespaceAuthority,
	name string,
	verifyInstalled func() error,
) error {
	inspection, err := inspectHeaderRecordTemporary(directory, name)
	if err != nil {
		return err
	}
	decision, err := resumestate.ReduceHeaderUpdateTemporary(
		namespace,
		resumestate.ClassifyHeaderUpdateTemporaryName(name),
		inspection.entry,
		inspection.candidate,
	)
	if err != nil {
		return errors.Join(err, closeFile(inspection.temporary))
	}
	return applyHeaderTemporaryDecision(
		directory, namespace, name, inspection.temporary, decision, verifyInstalled,
	)
}

func inspectHeaderRecordTemporary(
	directory outputcap.Directory,
	name string,
) (headerTemporaryInspection, error) {
	kind, err := ObserveExactEntry(directory, name)
	if err != nil {
		return headerTemporaryInspection{}, err
	}
	inspection := headerTemporaryInspection{entry: classifyHeaderTemporaryEntry(kind)}
	if inspection.entry != resumestate.UpdateTemporaryEntryRegular {
		return inspection, nil
	}
	inspection.temporary, err = directory.OpenFile(name, true, false)
	if err != nil {
		return headerTemporaryInspection{}, errors.Join(err, closeFile(inspection.temporary))
	}
	encoded, readErr := ReadFile(inspection.temporary, resumestate.MaxSessionHeaderBytes)
	if readErr != nil {
		return inspection, nil
	}
	decoded, decodeErr := resumestate.DecodeHeader(encoded)
	if decodeErr == nil {
		inspection.candidate = &decoded
	}
	return inspection, nil
}

func classifyHeaderTemporaryEntry(kind outputcap.EntryKind) resumestate.UpdateTemporaryEntryObservation {
	switch kind {
	case outputcap.EntryAbsent:
		return resumestate.UpdateTemporaryEntryMissing
	case outputcap.EntryRegularFile:
		return resumestate.UpdateTemporaryEntryRegular
	default:
		return resumestate.UpdateTemporaryEntryUnsafe
	}
}

func applyHeaderTemporaryDecision(
	directory outputcap.Directory,
	namespace resumestate.SessionNamespaceAuthority,
	name string,
	temporary outputcap.File,
	decision resumestate.HeaderUpdateTemporaryDecision,
	verifyInstalled func() error,
) error {
	switch decision.Action() {
	case resumestate.HeaderUpdateTemporaryAcceptInstalledHeader:
		return closeFile(temporary)
	case resumestate.HeaderUpdateTemporaryRemoveAndSyncSession:
		return removeAuthorizedHeaderTemporary(
			directory, namespace, name, temporary, decision, verifyInstalled,
		)
	case resumestate.HeaderUpdateTemporaryBlockResumeNamespace:
		return errors.Join(outputfault.ErrIntentUnsafe, closeFile(temporary))
	default:
		return errors.Join(
			fmt.Errorf("%w: header temporary recovery action", resumestate.ErrInvalidState),
			closeFile(temporary),
		)
	}
}

func removeAuthorizedHeaderTemporary(
	directory outputcap.Directory,
	namespace resumestate.SessionNamespaceAuthority,
	name string,
	temporary outputcap.File,
	decision resumestate.HeaderUpdateTemporaryDecision,
	verifyInstalled func() error,
) error {
	if err := verifyInstalled(); err != nil {
		return errors.Join(err, closeFile(temporary))
	}
	if err := decision.AuthorizeRemoval(
		namespace, name, resumestate.UpdateTemporaryEntryRegular,
	); err != nil {
		return errors.Join(err, closeFile(temporary))
	}
	if err := directory.RemoveFile(name, temporary); err != nil {
		return errors.Join(err, closeFile(temporary))
	}
	return errors.Join(directory.Sync(), temporary.Close())
}
