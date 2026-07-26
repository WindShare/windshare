package osfs

import (
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type outputSessionCandidateState uint8

const (
	outputSessionCandidateEmpty outputSessionCandidateState = iota + 1
	outputSessionCandidatePartial
	outputSessionCandidateComplete
)

var outputSessionCandidateChildren = []string{
	resumestate.HeaderRecordName,
	resumestate.SessionLockName,
	resumestate.FilesDirectoryName,
	resumestate.AnchorsDirectoryName,
	resumestate.StagesDirectoryName,
}

func openCanonicalIntentDirectory(
	sessions outputV3Directory,
	intent transfer.ResumeIntent,
) (outputV3Directory, error) {
	names, err := sessions.Names(outputRootInspectionLimit)
	if err != nil {
		return nil, err
	}
	canonicalName := resumestate.ResumeNamespaceName(intent)
	canonicalPresent := false
	for _, name := range names {
		classified := resumestate.ClassifyResumeNamespaceName(name)
		if classified.Classification() == resumestate.ResumeNamespaceOpaque || classified.Intent() != intent {
			continue
		}
		if classified.Classification() != resumestate.ResumeNamespaceCanonical || name != canonicalName {
			return nil, errOutputIntentUnsafe
		}
		canonicalPresent = true
	}
	if canonicalPresent {
		return sessions.OpenDirectory(canonicalName, true)
	}
	created, err := sessions.CreateDirectory(canonicalName, true)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(created.Sync(), sessions.Sync()); err != nil {
		return nil, errors.Join(err, created.Close())
	}
	return created, nil
}

func (authority *FilesystemOutputAuthority) openOrCreateSessionDirectory(
	intent outputV3Directory,
	control resumestate.Control,
	selection transfer.OutputSelection,
	ancestry resumestate.OutputAncestryBinding,
) (string, outputV3Directory, bool, error) {
	names, err := intent.Names(resumestate.MaxSessionsPerIntent + 1)
	if err != nil {
		return "", nil, false, err
	}
	var installedNames []string
	var candidateNames []string
	for _, name := range names {
		if _, parseErr := resumestate.ParseSessionDirectoryName(name); parseErr == nil {
			installedNames = append(installedNames, name)
			continue
		}
		if _, parseErr := resumestate.ParseSessionCandidateName(name); parseErr == nil {
			candidateNames = append(candidateNames, name)
			continue
		}
		return "", nil, false, errOutputIntentUnsafe
	}
	if len(installedNames) > 1 || len(candidateNames) > 1 || len(installedNames)+len(candidateNames) > 1 {
		return "", nil, false, errOutputIntentUnsafe
	}
	if len(installedNames) == 1 {
		opened, err := intent.OpenDirectory(installedNames[0], true)
		return installedNames[0], opened, false, err
	}
	if len(candidateNames) == 1 {
		name := candidateNames[0]
		sessionID, _ := resumestate.ParseSessionCandidateName(name)
		candidate, err := intent.OpenDirectory(name, true)
		if err != nil {
			return "", nil, false, err
		}
		expected, err := newOutputSessionHeader(control, selection, ancestry, sessionID)
		if err != nil {
			return "", nil, false, errors.Join(err, candidate.Close())
		}
		candidateNames, err := candidate.Names(len(outputSessionCandidateChildren) + outputStateAllocationAttempts + 1)
		if err != nil {
			return "", nil, false, errors.Join(err, candidate.Close())
		}
		if len(candidateNames) == 0 {
			if err := removeOutputSessionCandidate(intent, name, candidate); err != nil {
				return "", nil, false, err
			}
			return authority.createSessionDirectory(intent, control, selection, ancestry)
		}
		if err := authority.completeOutputSessionCandidate(candidate, expected); err != nil {
			return "", nil, false, errors.Join(err, candidate.Close())
		}
		installedName := resumestate.SessionDirectoryName(sessionID)
		installed, err := installOutputSessionCandidate(intent, candidate, installedName)
		closeErr := candidate.Close()
		if err != nil || closeErr != nil {
			if installed != nil {
				_ = installed.Close()
			}
			return "", nil, false, errors.Join(err, closeErr)
		}
		return installedName, installed, true, nil
	}
	return authority.createSessionDirectory(intent, control, selection, ancestry)
}

func (authority *FilesystemOutputAuthority) createSessionDirectory(
	intent outputV3Directory,
	control resumestate.Control,
	selection transfer.OutputSelection,
	ancestry resumestate.OutputAncestryBinding,
) (string, outputV3Directory, bool, error) {
	for range outputStateAllocationAttempts {
		sessionID, err := authority.sessionIDs.NewOutputSessionID()
		if err != nil {
			return "", nil, false, err
		}
		if sessionID.IsZero() {
			continue
		}
		candidateName := resumestate.SessionCandidateName(sessionID)
		candidate, err := intent.CreateDirectory(candidateName, true)
		if errors.Is(err, errOutputV3Collision) {
			continue
		}
		if err != nil {
			return "", nil, false, err
		}
		if err := errors.Join(candidate.Sync(), intent.Sync()); err != nil {
			return "", nil, false, errors.Join(err, candidate.Close())
		}
		header, err := newOutputSessionHeader(control, selection, ancestry, sessionID)
		if err != nil {
			return "", nil, false, errors.Join(err, candidate.Close())
		}
		if err := authority.completeOutputSessionCandidate(candidate, header); err != nil {
			return "", nil, false, errors.Join(err, candidate.Close())
		}
		installedName := resumestate.SessionDirectoryName(sessionID)
		installed, err := installOutputSessionCandidate(intent, candidate, installedName)
		closeErr := candidate.Close()
		if err != nil || closeErr != nil {
			if installed != nil {
				_ = installed.Close()
			}
			return "", nil, false, errors.Join(err, closeErr)
		}
		return installedName, installed, true, nil
	}
	return "", nil, false, fmt.Errorf("%w: allocate output session identity", errOutputIntentUnsafe)
}

func newOutputSessionHeader(
	control resumestate.Control,
	selection transfer.OutputSelection,
	ancestry resumestate.OutputAncestryBinding,
	sessionID transfer.OutputSessionID,
) (resumestate.Header, error) {
	return resumestate.NewHeader(resumestate.HeaderSpec{
		Backend: filesystemOutputBackendID, SessionID: sessionID, Selection: selection,
		OutputRoot: control.OutputRoot(), OutputAncestry: ancestry,
	})
}

func (authority *FilesystemOutputAuthority) completeOutputSessionCandidate(
	candidate outputV3Directory,
	header resumestate.Header,
) error {
	if err := validateOutputSessionCandidateCreationCut(candidate); err != nil {
		return err
	}
	encoded, err := resumestate.EncodeHeader(header)
	if err != nil {
		return err
	}
	store := authority.stateStore(header.ResumeIntent(), header.SessionID())
	if _, err := store.ensureInitialRecord(
		candidate, resumestate.HeaderRecordName, encoded, resumestate.MaxSessionHeaderBytes,
	); err != nil {
		return err
	}
	state, err := inspectOutputSessionCandidate(candidate, header)
	if err != nil {
		return err
	}
	if state == outputSessionCandidateComplete {
		return nil
	}
	names, err := candidate.Names(len(outputSessionCandidateChildren) + 1)
	if err != nil {
		return err
	}
	present := make(map[string]struct{}, len(names))
	for _, name := range names {
		present[name] = struct{}{}
	}
	if _, ok := present[resumestate.SessionLockName]; !ok {
		lock, err := candidate.CreateFile(resumestate.SessionLockName, true, 0)
		if err != nil {
			return err
		}
		if err := errors.Join(lock.Sync(), candidate.Sync(), lock.Close()); err != nil {
			return err
		}
	}
	for _, name := range []string{
		resumestate.FilesDirectoryName, resumestate.AnchorsDirectoryName, resumestate.StagesDirectoryName,
	} {
		if _, ok := present[name]; ok {
			continue
		}
		child, err := candidate.CreateDirectory(name, true)
		if err != nil {
			return err
		}
		if err := errors.Join(child.Sync(), candidate.Sync(), child.Close()); err != nil {
			return err
		}
	}
	if err := candidate.Sync(); err != nil {
		return err
	}
	state, err = inspectOutputSessionCandidate(candidate, header)
	if err != nil || state != outputSessionCandidateComplete {
		return errors.Join(errOutputIntentUnsafe, err)
	}
	return nil
}

// validateOutputSessionCandidateCreationCut recognizes only durable prefixes of
// the creation order. In particular, a later child can never authorize
// synthesizing the missing header that must be the candidate's first durable
// authority. Canonical header temporaries are the one pre-header crash cut.
func validateOutputSessionCandidateCreationCut(candidate outputV3Directory) error {
	names, err := candidate.Names(
		len(outputSessionCandidateChildren) + outputStateAllocationAttempts + 1,
	)
	if err != nil {
		return err
	}
	present := make(map[string]struct{}, len(outputSessionCandidateChildren))
	temporaryCount := 0
	for _, name := range names {
		kind, exact, err := candidate.ClassifyExactEntry(name)
		if err != nil || !exact {
			return errors.Join(errOutputIntentUnsafe, err)
		}
		if resumestate.ClassifyHeaderUpdateTemporaryName(name).Classification() ==
			resumestate.HeaderUpdateTemporaryCanonical {
			if kind != outputV3EntryRegularFile {
				return errOutputIntentUnsafe
			}
			temporaryCount++
			if temporaryCount > outputStateAllocationAttempts {
				return errOutputIntentUnsafe
			}
			continue
		}
		index := slices.Index(outputSessionCandidateChildren, name)
		if index < 0 {
			return errOutputIntentUnsafe
		}
		expectedKind := outputV3EntryDirectory
		if index < 2 {
			expectedKind = outputV3EntryRegularFile
		}
		if kind != expectedKind {
			return errOutputIntentUnsafe
		}
		present[name] = struct{}{}
	}

	_, headerPresent := present[resumestate.HeaderRecordName]
	if !headerPresent {
		if len(present) != 0 {
			return errOutputIntentUnsafe
		}
		return nil
	}
	if temporaryCount != 0 && len(present) != 1 {
		return errOutputIntentUnsafe
	}
	for index, name := range outputSessionCandidateChildren {
		_, exists := present[name]
		if index < len(present) && !exists || index >= len(present) && exists {
			return errOutputIntentUnsafe
		}
	}
	return nil
}

func inspectOutputSessionCandidate(
	candidate outputV3Directory,
	expected resumestate.Header,
) (outputSessionCandidateState, error) {
	names, err := candidate.Names(len(outputSessionCandidateChildren) + 1)
	if err != nil {
		return 0, err
	}
	if len(names) == 0 {
		return outputSessionCandidateEmpty, nil
	}
	present := make(map[string]struct{}, len(names))
	for _, name := range names {
		if !slices.Contains(outputSessionCandidateChildren, name) {
			return 0, errOutputIntentUnsafe
		}
		present[name] = struct{}{}
	}
	for index, name := range outputSessionCandidateChildren {
		_, ok := present[name]
		if index < len(names) && !ok || index >= len(names) && ok {
			return 0, errOutputIntentUnsafe
		}
	}
	encoded, err := readStateRecord(candidate, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
	if err != nil {
		return 0, err
	}
	header, err := resumestate.DecodeHeader(encoded)
	if err != nil {
		return 0, errors.Join(errOutputIntentUnsafe, err)
	}
	if header.OutputAncestry() != expected.OutputAncestry() {
		return 0, errors.Join(
			errOutputIntentUnsafe,
			&outputAncestryHeaderMismatch{sessionID: header.SessionID()},
		)
	}
	if header != expected {
		return 0, errOutputIntentUnsafe
	}
	if _, ok := present[resumestate.SessionLockName]; ok {
		lock, err := candidate.OpenFile(resumestate.SessionLockName, true, false)
		if err != nil {
			return 0, err
		}
		size, sizeErr := lock.Size()
		closeErr := lock.Close()
		if sizeErr != nil || closeErr != nil || size != 0 {
			return 0, errors.Join(errOutputIntentUnsafe, sizeErr, closeErr)
		}
	}
	for _, name := range []string{
		resumestate.FilesDirectoryName, resumestate.AnchorsDirectoryName, resumestate.StagesDirectoryName,
	} {
		if _, ok := present[name]; !ok {
			continue
		}
		child, err := candidate.OpenDirectory(name, true)
		if err != nil {
			return 0, err
		}
		children, listErr := child.Names(1)
		closeErr := child.Close()
		if listErr != nil || closeErr != nil || len(children) != 0 {
			return 0, errors.Join(errOutputIntentUnsafe, listErr, closeErr)
		}
	}
	if len(names) == len(outputSessionCandidateChildren) {
		return outputSessionCandidateComplete, nil
	}
	return outputSessionCandidatePartial, nil
}

func installOutputSessionCandidate(
	intent outputV3Directory,
	candidate outputV3Directory,
	installedName string,
) (outputV3Directory, error) {
	installed, err := intent.InstallDirectoryNoReplace(candidate, installedName)
	if err != nil {
		return nil, err
	}
	same, compareErr := installed.SameDirectory(candidate)
	if compareErr != nil || !same {
		return installed, errors.Join(errOutputIntentUnsafe, compareErr)
	}
	if err := intent.Sync(); err != nil {
		return installed, err
	}
	if err := verifyPinnedDirectoryEntry(intent, installedName, candidate); err != nil {
		return installed, err
	}
	return installed, nil
}

func removeOutputSessionCandidate(
	intent outputV3Directory,
	name string,
	candidate outputV3Directory,
) error {
	if err := verifyPinnedDirectoryEntry(intent, name, candidate); err != nil {
		return errors.Join(err, candidate.Close())
	}
	remaining, err := candidate.Names(1)
	if err != nil || len(remaining) != 0 {
		return errors.Join(errOutputIntentUnsafe, err, candidate.Close())
	}
	if err := intent.RemoveDirectory(name, candidate); err != nil {
		return errors.Join(err, candidate.Close())
	}
	return errors.Join(intent.Sync(), candidate.Close())
}
