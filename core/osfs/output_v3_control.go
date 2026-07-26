package osfs

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

const outputRootInspectionLimit = 1024

type outputControlNamespace struct {
	directory outputV3Directory
	sessions  outputV3Directory
	control   resumestate.Control
}

func (namespace *outputControlNamespace) Close() error {
	if namespace == nil {
		return nil
	}
	return errors.Join(namespace.sessions.Close(), namespace.directory.Close())
}

func (authority *FilesystemOutputAuthority) openOrBootstrapControl(
	platform outputV3Platform,
) (*outputControlNamespace, bool, error) {
	root := platform.Root()
	controlKind, err := observeExactOutputEntry(root, resumestate.ControlDirectoryName)
	if err != nil {
		return nil, false, rootOutputFault("inspect output root", err)
	}
	controlPresent := controlKind != outputV3EntryAbsent
	candidates, err := root.NamesWithPrefix(resumestate.BootstrapCandidatePrefix, outputRootInspectionLimit)
	if err != nil {
		return nil, false, rootOutputFault("inspect bootstrap candidates", err)
	}
	for _, name := range candidates {
		if _, err := resumestate.ParseBootstrapCandidateName(name); err != nil {
			return nil, false, rootOutputFault("classify bootstrap candidate", err)
		}
	}

	if controlPresent {
		namespace, err := openInstalledControl(root, platform)
		if err != nil {
			return nil, false, err
		}
		for _, name := range candidates {
			if err := removeRecoverableBootstrapCandidate(authority, root, name, platform, &namespace.control); err != nil {
				_ = namespace.Close()
				return nil, false, rootOutputFault("remove superseded bootstrap candidate", err)
			}
		}
		return namespace, false, nil
	}

	if len(candidates) > 0 {
		var complete []string
		for _, name := range candidates {
			candidate, control, state, err := inspectBootstrapCandidate(authority, root, name, platform)
			if err != nil {
				return nil, false, rootOutputFault("inspect bootstrap candidate", err)
			}
			switch state {
			case resumestate.BootstrapCandidateEmpty:
				if err := removeBootstrapCandidate(root, name, candidate); err != nil {
					return nil, false, rootOutputFault("remove empty bootstrap candidate", err)
				}
			case resumestate.BootstrapCandidateValidPartial:
				if err := completeBootstrapCandidate(authority, candidate, control); err != nil {
					_ = candidate.Close()
					return nil, false, rootOutputFault("complete bootstrap candidate", err)
				}
				_ = candidate.Close()
				complete = append(complete, name)
			case resumestate.BootstrapCandidateComplete:
				_ = candidate.Close()
				complete = append(complete, name)
			default:
				_ = candidate.Close()
				return nil, false, rootOutputFault("classify bootstrap candidate", errOutputRootUnsafe)
			}
		}
		if len(complete) > 0 {
			slices.Sort(complete)
			installErr := installBootstrapCandidate(root, complete[0])
			collision := errors.Is(installErr, errOutputV3Collision)
			if installErr != nil && !collision {
				return nil, false, rootOutputFault("install recovered bootstrap candidate", installErr)
			}
			namespace, err := openInstalledControl(root, platform)
			if err != nil {
				return nil, false, err
			}
			losers := complete[1:]
			if collision {
				losers = complete
			}
			for _, name := range losers {
				if err := removeRecoverableBootstrapCandidate(authority, root, name, platform, &namespace.control); err != nil {
					_ = namespace.Close()
					return nil, false, rootOutputFault("remove losing bootstrap candidate", err)
				}
			}
			return namespace, true, nil
		}
	}

	control, err := authority.newControl(platform)
	if err != nil {
		return nil, false, err
	}
	nonce, err := resumestate.GenerateBootstrapNonce(authority.random)
	if err != nil {
		return nil, false, rootOutputFault("allocate bootstrap nonce", err)
	}
	name := resumestate.BootstrapCandidateName(nonce)
	candidate, err := root.CreateDirectory(name, true)
	if err != nil {
		return nil, false, rootOutputFault("create bootstrap candidate", err)
	}
	if err := completeBootstrapCandidate(authority, candidate, control); err != nil {
		_ = candidate.Close()
		return nil, false, rootOutputFault("build bootstrap candidate", err)
	}
	installErr := installBootstrapCandidate(root, name)
	collision := errors.Is(installErr, errOutputV3Collision)
	if installErr != nil {
		if !collision {
			_ = candidate.Close()
			return nil, false, rootOutputFault("install control namespace", installErr)
		}
	}
	_ = candidate.Close()
	namespace, err := openInstalledControl(root, platform)
	if err != nil {
		return nil, false, err
	}
	if namespace.control != control {
		_ = namespace.Close()
		return nil, false, rootOutputFault("verify installed control binding", errOutputRootUnsafe)
	}
	if collision {
		if err := removeRecoverableBootstrapCandidate(authority, root, name, platform, &namespace.control); err != nil {
			_ = namespace.Close()
			return nil, false, rootOutputFault("remove colliding bootstrap candidate", err)
		}
	}
	return namespace, true, nil
}

func (authority *FilesystemOutputAuthority) newControl(platform outputV3Platform) (resumestate.Control, error) {
	root, err := platform.RootBinding()
	if err != nil {
		return resumestate.Control{}, rootOutputFault("bind output root identity", err)
	}
	control, err := resumestate.NewControl(resumestate.ControlSpec{
		Backend: filesystemOutputBackendID, OutputRoot: root, Certification: platform.Certification(),
		Durability: platform.Durability(), Generation: 1,
	})
	if err != nil {
		return resumestate.Control{}, rootOutputFault("construct control state", err)
	}
	return control, nil
}

func completeBootstrapCandidate(
	authority *FilesystemOutputAuthority,
	candidate outputV3Directory,
	control resumestate.Control,
) error {
	store := authority.stateStore(transfer.ResumeIntent{}, transfer.OutputSessionID{})
	names, err := candidate.Names(4)
	if err != nil {
		return err
	}
	prefixLength, err := bootstrapCandidatePrefixLength(names)
	if err != nil {
		return err
	}
	if prefixLength == 0 {
		encoded, err := resumestate.EncodeControl(control)
		if err != nil {
			return err
		}
		if _, err := store.ensureInitialRecord(candidate, resumestate.ControlRecordName, encoded, resumestate.MaxControlStateBytes); err != nil {
			return err
		}
		names = append(names, resumestate.ControlRecordName)
	}
	if prefixLength < 2 {
		lock, err := candidate.CreateFile(resumestate.CoordinatorLockName, true, 0)
		if err != nil {
			return err
		}
		if err := errors.Join(lock.Sync(), candidate.Sync(), lock.Close()); err != nil {
			return err
		}
	}
	if prefixLength < 3 {
		sessions, err := candidate.CreateDirectory(resumestate.SessionsDirectoryName, true)
		if err != nil {
			return err
		}
		if err := errors.Join(sessions.Sync(), candidate.Sync(), sessions.Close()); err != nil {
			return err
		}
	}
	return candidate.Sync()
}

func bootstrapCandidatePrefixLength(names []string) (int, error) {
	expected := []string{
		resumestate.ControlRecordName,
		resumestate.CoordinatorLockName,
		resumestate.SessionsDirectoryName,
	}
	actual := slices.Clone(names)
	slices.Sort(actual)
	for length := 0; length <= len(expected); length++ {
		prefix := slices.Clone(expected[:length])
		slices.Sort(prefix)
		if slices.Equal(actual, prefix) {
			return length, nil
		}
	}
	return 0, errOutputRootUnsafe
}

func inspectBootstrapCandidate(
	authority *FilesystemOutputAuthority,
	root outputV3Directory,
	name string,
	platform outputV3Platform,
) (outputV3Directory, resumestate.Control, resumestate.BootstrapCandidateObservation, error) {
	candidate, err := root.OpenDirectory(name, true)
	if err != nil {
		return nil, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
	}
	controlKind, err := observeExactOutputEntry(candidate, resumestate.ControlRecordName)
	if err != nil {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
	}
	temporaries, err := candidate.NamesWithPrefix(
		resumestate.ControlUpdateTemporaryPrefix, outputStateAllocationAttempts+1,
	)
	if err != nil {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
	}
	rawNames, err := candidate.Names(len(outputSessionCandidateChildren) + outputStateAllocationAttempts + 1)
	if err != nil {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
	}
	structuralNames := make([]string, 0, len(rawNames))
	for _, child := range rawNames {
		if strings.HasPrefix(child, resumestate.ControlUpdateTemporaryPrefix) {
			continue
		}
		structuralNames = append(structuralNames, child)
	}
	if _, err := bootstrapCandidatePrefixLength(structuralNames); err != nil {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, nil
	}
	if len(rawNames) == 0 {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateEmpty, nil
	}
	if controlKind != outputV3EntryAbsent || len(temporaries) != 0 {
		expected, err := authority.newControl(platform)
		if err != nil {
			return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
		}
		encoded, err := resumestate.EncodeControl(expected)
		if err != nil {
			return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
		}
		store := authority.stateStore(transfer.ResumeIntent{}, transfer.OutputSessionID{})
		if _, err := store.ensureInitialRecord(
			candidate, resumestate.ControlRecordName, encoded, resumestate.MaxControlStateBytes,
		); err != nil {
			return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
		}
	}
	names, err := candidate.Names(4)
	if err != nil {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
	}
	prefixLength, err := bootstrapCandidatePrefixLength(names)
	if err != nil || prefixLength == 0 {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, nil
	}
	control, err := readAndValidateControl(candidate, platform)
	if err != nil {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
	}
	if prefixLength >= 2 {
		lock, err := candidate.OpenFile(resumestate.CoordinatorLockName, true, false)
		if err != nil {
			return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
		}
		size, sizeErr := lock.Size()
		closeErr := lock.Close()
		if sizeErr != nil || closeErr != nil || size != 0 {
			return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe,
				errors.Join(errOutputRootUnsafe, sizeErr, closeErr)
		}
	}
	if prefixLength == 3 {
		sessions, err := validateControlChildren(candidate)
		if err != nil {
			return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
		}
		if err := sessions.Close(); err != nil {
			return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
		}
		return candidate, control, resumestate.BootstrapCandidateComplete, nil
	}
	return candidate, control, resumestate.BootstrapCandidateValidPartial, nil
}

func installBootstrapCandidate(root outputV3Directory, name string) error {
	candidate, err := root.OpenDirectory(name, true)
	if err != nil {
		return err
	}
	defer candidate.Close()
	installed, err := root.InstallDirectoryNoReplace(candidate, resumestate.ControlDirectoryName)
	if installed != nil {
		_ = installed.Close()
	}
	if err == nil {
		err = root.Sync()
	}
	return err
}

func openInstalledControl(root outputV3Directory, platform outputV3Platform) (*outputControlNamespace, error) {
	directory, err := root.OpenDirectory(resumestate.ControlDirectoryName, true)
	if err != nil {
		return nil, rootOutputFault("open control namespace", err)
	}
	control, err := readAndValidateControl(directory, platform)
	if err != nil {
		_ = directory.Close()
		return nil, err
	}
	sessions, err := validateControlChildren(directory)
	if err != nil {
		_ = directory.Close()
		return nil, rootOutputFault("validate control namespace", err)
	}
	return &outputControlNamespace{directory: directory, sessions: sessions, control: control}, nil
}

func readAndValidateControl(directory outputV3Directory, platform outputV3Platform) (resumestate.Control, error) {
	encoded, err := readStateRecord(directory, resumestate.ControlRecordName, resumestate.MaxControlStateBytes)
	if err != nil {
		return resumestate.Control{}, rootOutputFault("read global control state", err)
	}
	control, err := resumestate.DecodeControl(encoded)
	rootBinding, bindingErr := platform.RootBinding()
	if err != nil || control.Backend() != filesystemOutputBackendID || control.Certification() != platform.Certification() ||
		control.Durability() != platform.Durability() || bindingErr != nil || control.OutputRoot() != rootBinding {
		return resumestate.Control{}, rootOutputFault(
			"validate global control state", errors.Join(err, bindingErr, errOutputRootUnsafe),
		)
	}
	return control, nil
}

func validateControlChildren(directory outputV3Directory) (outputV3Directory, error) {
	names, err := directory.Names(4)
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	expected := []string{resumestate.ControlRecordName, resumestate.CoordinatorLockName, resumestate.SessionsDirectoryName}
	slices.Sort(expected)
	if !slices.Equal(names, expected) {
		return nil, errOutputRootUnsafe
	}
	lock, err := directory.OpenFile(resumestate.CoordinatorLockName, true, false)
	if err != nil {
		return nil, err
	}
	size, sizeErr := lock.Size()
	closeErr := lock.Close()
	if sizeErr != nil || closeErr != nil || size != 0 {
		return nil, errors.Join(errOutputRootUnsafe, sizeErr, closeErr)
	}
	return directory.OpenDirectory(resumestate.SessionsDirectoryName, true)
}

func removeRecoverableBootstrapCandidate(
	authority *FilesystemOutputAuthority,
	root outputV3Directory,
	name string,
	platform outputV3Platform,
	installed *resumestate.Control,
) error {
	candidate, control, observation, err := inspectBootstrapCandidate(authority, root, name, platform)
	if err != nil {
		return err
	}
	if observation == resumestate.BootstrapCandidateUnsafe ||
		observation != resumestate.BootstrapCandidateEmpty && installed != nil && control != *installed {
		_ = candidate.Close()
		return errOutputRootUnsafe
	}
	return removeBootstrapCandidate(root, name, candidate)
}

func removeBootstrapCandidate(root outputV3Directory, name string, candidate outputV3Directory) error {
	if candidate == nil {
		return errOutputRootUnsafe
	}
	names, err := candidate.Names(4)
	if err != nil {
		return errors.Join(err, candidate.Close())
	}
	for _, child := range names {
		if child != resumestate.ControlRecordName && child != resumestate.CoordinatorLockName &&
			child != resumestate.SessionsDirectoryName {
			return errors.Join(errOutputRootUnsafe, candidate.Close())
		}
	}

	// control.state is the candidate's envelope. Removing it first would turn a
	// crash during cleanup into an unclassifiable non-empty directory. Retiring
	// data before the stable lock and the envelope last ensures every persisted
	// cut is either a recognized partial candidate or an empty removable shell.
	for _, child := range []string{
		resumestate.SessionsDirectoryName,
		resumestate.CoordinatorLockName,
		resumestate.ControlRecordName,
	} {
		if !slices.Contains(names, child) {
			continue
		}
		if err := verifyPinnedDirectoryEntry(root, name, candidate); err != nil {
			return errors.Join(err, candidate.Close())
		}
		if err := removeBootstrapChild(candidate, child); err != nil {
			return errors.Join(err, candidate.Close())
		}
		if err := candidate.Sync(); err != nil {
			return errors.Join(err, candidate.Close())
		}
	}

	if err := verifyPinnedDirectoryEntry(root, name, candidate); err != nil {
		return errors.Join(err, candidate.Close())
	}
	remaining, err := candidate.Names(1)
	if err != nil || len(remaining) != 0 {
		return errors.Join(errOutputRootUnsafe, err, candidate.Close())
	}
	if err := root.RemoveDirectory(name, candidate); err != nil {
		return errors.Join(err, candidate.Close())
	}
	return errors.Join(root.Sync(), candidate.Close())
}

func removeBootstrapChild(candidate outputV3Directory, name string) error {
	switch name {
	case resumestate.SessionsDirectoryName:
		directory, err := candidate.OpenDirectory(name, true)
		if err != nil {
			return err
		}
		children, listErr := directory.Names(1)
		if listErr != nil || len(children) != 0 {
			return errors.Join(errOutputRootUnsafe, listErr, directory.Close())
		}
		return errors.Join(candidate.RemoveDirectory(name, directory), directory.Close())
	case resumestate.CoordinatorLockName, resumestate.ControlRecordName:
		file, err := candidate.OpenFile(name, true, false)
		if err != nil {
			return err
		}
		return errors.Join(candidate.RemoveFile(name, file), file.Close())
	default:
		return errOutputRootUnsafe
	}
}

func verifyPinnedDirectoryEntry(parent outputV3Directory, name string, expected outputV3Directory) error {
	current, err := parent.OpenDirectory(name, true)
	if err != nil {
		return err
	}
	same, compareErr := current.SameDirectory(expected)
	closeErr := current.Close()
	if compareErr != nil || closeErr != nil || !same {
		return errors.Join(errOutputRootUnsafe, compareErr, closeErr)
	}
	return nil
}

func rootOutputFault(operation string, cause error) error {
	if errors.Is(cause, errOutputV3Unsupported) {
		return outputFault(transfer.OutputFaultRoot, transfer.OutputFaultUnsupportedFilesystem,
			errors.Join(errUnsupportedOutputVolume, fmt.Errorf("%s: %w", operation, cause)))
	}
	return outputFault(transfer.OutputFaultRoot, transfer.OutputFaultNamespaceUnsafe,
		errors.Join(errOutputRootUnsafe, fmt.Errorf("%s: %w", operation, cause)))
}

func isMissing(err error) bool { return errors.Is(err, fs.ErrNotExist) }
