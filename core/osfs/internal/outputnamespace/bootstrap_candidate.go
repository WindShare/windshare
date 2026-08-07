package outputnamespace

import (
	"errors"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func completeBootstrapCandidate(
	controller Controller,
	candidate outputcap.Directory,
	control resumestate.Control,
) error {
	store := controller.Store(transfer.TransferIntentDigest{}, transfer.OutputSessionID{})
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
		if _, err := store.EnsureInitialRecord(
			candidate, resumestate.ControlRecordName, encoded, resumestate.MaxControlStateBytes,
		); err != nil {
			return err
		}
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
	return 0, outputfault.ErrRootUnsafe
}

func inspectBootstrapCandidate(
	controller Controller,
	root outputcap.Directory,
	name string,
	platform outputcap.Platform,
) (outputcap.Directory, resumestate.Control, resumestate.BootstrapCandidateObservation, error) {
	candidate, err := root.OpenDirectory(name, true)
	if err != nil {
		return nil, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
	}
	structure, err := inspectBootstrapCandidateStructure(candidate)
	if err != nil {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
	}
	if structure.disposition == bootstrapStructureUnsafe {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, nil
	}
	if structure.disposition == bootstrapStructureEmpty {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateEmpty, nil
	}
	if err := controller.ensureBootstrapControlRecord(candidate, platform, structure.hasControlArtifacts); err != nil {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
	}
	names, err := candidate.Names(4)
	if err != nil {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
	}
	prefixLength, err := bootstrapCandidatePrefixLength(names)
	if err != nil || prefixLength == 0 {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, nil
	}
	control, err := controller.readAndValidateControl(candidate, platform)
	if err != nil {
		return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
	}
	if prefixLength >= 2 {
		if err := verifyBootstrapCandidateLock(candidate); err != nil {
			return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
		}
	}
	if prefixLength == 3 {
		if err := verifyCompleteBootstrapCandidate(candidate); err != nil {
			return candidate, resumestate.Control{}, resumestate.BootstrapCandidateUnsafe, err
		}
		return candidate, control, resumestate.BootstrapCandidateComplete, nil
	}
	return candidate, control, resumestate.BootstrapCandidateValidPartial, nil
}

type bootstrapStructureDisposition uint8

const (
	bootstrapStructureEmpty bootstrapStructureDisposition = iota + 1
	bootstrapStructurePlausible
	bootstrapStructureUnsafe
)

type bootstrapStructureInspection struct {
	disposition         bootstrapStructureDisposition
	hasControlArtifacts bool
}

func inspectBootstrapCandidateStructure(candidate outputcap.Directory) (bootstrapStructureInspection, error) {
	controlKind, err := ObserveExactEntry(candidate, resumestate.ControlRecordName)
	if err != nil {
		return bootstrapStructureInspection{}, err
	}
	temporaries, err := candidate.NamesWithPrefix(
		resumestate.ControlUpdateTemporaryPrefix, AllocationAttempts+1,
	)
	if err != nil {
		return bootstrapStructureInspection{}, err
	}
	rawNames, err := candidate.Names(len(sessionCandidateChildren) + AllocationAttempts + 1)
	if err != nil {
		return bootstrapStructureInspection{}, err
	}
	structuralNames := bootstrapStructuralNames(rawNames)
	if _, err := bootstrapCandidatePrefixLength(structuralNames); err != nil {
		return bootstrapStructureInspection{disposition: bootstrapStructureUnsafe}, nil
	}
	if len(rawNames) == 0 {
		return bootstrapStructureInspection{disposition: bootstrapStructureEmpty}, nil
	}
	return bootstrapStructureInspection{
		disposition:         bootstrapStructurePlausible,
		hasControlArtifacts: controlKind != outputcap.EntryAbsent || len(temporaries) != 0,
	}, nil
}

func bootstrapStructuralNames(rawNames []string) []string {
	structuralNames := make([]string, 0, len(rawNames))
	for _, child := range rawNames {
		if !strings.HasPrefix(child, resumestate.ControlUpdateTemporaryPrefix) {
			structuralNames = append(structuralNames, child)
		}
	}
	return structuralNames
}

func (controller Controller) ensureBootstrapControlRecord(
	candidate outputcap.Directory,
	platform outputcap.Platform,
	hasControlArtifacts bool,
) error {
	if !hasControlArtifacts {
		return nil
	}
	expected, err := controller.newControl(platform)
	if err != nil {
		return err
	}
	encoded, err := resumestate.EncodeControl(expected)
	if err != nil {
		return err
	}
	store := controller.Store(transfer.TransferIntentDigest{}, transfer.OutputSessionID{})
	_, err = store.EnsureInitialRecord(
		candidate, resumestate.ControlRecordName, encoded, resumestate.MaxControlStateBytes,
	)
	return err
}

func verifyBootstrapCandidateLock(candidate outputcap.Directory) error {
	lock, err := candidate.OpenFile(resumestate.CoordinatorLockName, true, false)
	if err != nil {
		return err
	}
	size, sizeErr := lock.Size()
	closeErr := lock.Close()
	if sizeErr != nil || closeErr != nil || size != 0 {
		return errors.Join(outputfault.ErrRootUnsafe, sizeErr, closeErr)
	}
	return nil
}

func verifyCompleteBootstrapCandidate(candidate outputcap.Directory) error {
	sessions, err := validateControlChildren(candidate)
	if err != nil {
		return err
	}
	return sessions.Close()
}

func installBootstrapCandidate(root outputcap.Directory, name string) error {
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
