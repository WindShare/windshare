package outputnamespace

import (
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type sessionCandidateState uint8

const (
	sessionCandidateEmpty sessionCandidateState = iota + 1
	sessionCandidatePartial
	sessionCandidateComplete
)

var sessionCandidateChildren = []string{
	resumestate.HeaderRecordName,
	resumestate.SessionLockName,
	resumestate.FilesDirectoryName,
	resumestate.AnchorsDirectoryName,
	resumestate.StagesDirectoryName,
}

// SessionDisposition distinguishes an already-installed session from a candidate installed by this open.
type SessionDisposition uint8

const (
	SessionExisting SessionDisposition = iota + 1
	SessionInstalled
)

// SessionOpenResult returns fixed directory authority without an ambiguous creation flag.
type SessionOpenResult struct {
	Name        string
	Directory   outputcap.Directory
	Disposition SessionDisposition
}

// AncestryMismatchError identifies the durable session whose header bound different ancestry.
type AncestryMismatchError struct {
	sessionID transfer.OutputSessionID
}

func (mismatch *AncestryMismatchError) Error() string {
	return "osfs: output session ancestry binding changed"
}

func (mismatch *AncestryMismatchError) SessionID() transfer.OutputSessionID {
	if mismatch == nil {
		return transfer.OutputSessionID{}
	}
	return mismatch.sessionID
}

func OpenCanonicalIntent(
	sessions outputcap.Directory,
	intent transfer.TransferIntentDigest,
) (outputcap.Directory, error) {
	names, err := sessions.Names(RootInspectionLimit)
	if err != nil {
		return nil, err
	}
	canonicalName := resumestate.IntentNamespaceName(intent)
	canonicalPresent := false
	for _, name := range names {
		classified := resumestate.ClassifyIntentNamespaceName(name)
		if classified.Classification() == resumestate.IntentNamespaceOpaque || classified.Intent() != intent {
			continue
		}
		if classified.Classification() != resumestate.IntentNamespaceCanonical || name != canonicalName {
			return nil, outputfault.ErrIntentUnsafe
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

func (controller Controller) OpenOrCreateSession(
	intent outputcap.Directory,
	control resumestate.Control,
	selection transfer.OutputSelection,
	ancestry resumestate.OutputAncestryBinding,
) (SessionOpenResult, error) {
	// The intent directory owns one metadata child (the V1 checkpoint namespace)
	// in addition to its bounded session children. Inspect that child here so a
	// checkpoint tree cannot be mistaken for a session or silently widen the
	// namespace scan.
	names, err := intent.Names(resumestate.MaxSessionsPerIntent + 2)
	if err != nil {
		return SessionOpenResult{}, err
	}
	var installedNames []string
	var candidateNames []string
	for _, name := range names {
		if name == resumestate.CheckpointsDirectoryName {
			kind, observeErr := intent.ObserveEntry(name)
			if observeErr != nil || kind != outputcap.EntryDirectory {
				return SessionOpenResult{}, errors.Join(outputfault.ErrIntentUnsafe, observeErr)
			}
			continue
		}
		if _, parseErr := resumestate.ParseSessionDirectoryName(name); parseErr == nil {
			installedNames = append(installedNames, name)
			continue
		}
		if _, parseErr := resumestate.ParseSessionCandidateName(name); parseErr == nil {
			candidateNames = append(candidateNames, name)
			continue
		}
		return SessionOpenResult{}, outputfault.ErrIntentUnsafe
	}
	if len(installedNames) > 1 || len(candidateNames) > 1 || len(installedNames)+len(candidateNames) > 1 {
		return SessionOpenResult{}, outputfault.ErrIntentUnsafe
	}
	if len(installedNames) == 1 {
		opened, err := intent.OpenDirectory(installedNames[0], true)
		return SessionOpenResult{Name: installedNames[0], Directory: opened, Disposition: SessionExisting}, err
	}
	if len(candidateNames) == 1 {
		return controller.recoverSessionCandidate(
			intent, control, selection, ancestry, candidateNames[0],
		)
	}
	return controller.createSessionDirectory(intent, control, selection, ancestry)
}

func (controller Controller) recoverSessionCandidate(
	intent outputcap.Directory,
	control resumestate.Control,
	selection transfer.OutputSelection,
	ancestry resumestate.OutputAncestryBinding,
	name string,
) (SessionOpenResult, error) {
	sessionID, _ := resumestate.ParseSessionCandidateName(name)
	candidate, err := intent.OpenDirectory(name, true)
	if err != nil {
		return SessionOpenResult{}, err
	}
	expected, err := controller.newHeader(control, selection, ancestry, sessionID)
	if err != nil {
		return SessionOpenResult{}, errors.Join(err, candidate.Close())
	}
	children, err := candidate.Names(len(sessionCandidateChildren) + AllocationAttempts + 1)
	if err != nil {
		return SessionOpenResult{}, errors.Join(err, candidate.Close())
	}
	if len(children) == 0 {
		return controller.replaceEmptySessionCandidate(intent, control, selection, ancestry, name, candidate)
	}
	if err := controller.completeOutputSessionCandidate(candidate, expected); err != nil {
		return SessionOpenResult{}, errors.Join(err, candidate.Close())
	}
	return installCompletedSessionCandidate(intent, candidate, sessionID)
}

func (controller Controller) replaceEmptySessionCandidate(
	intent outputcap.Directory,
	control resumestate.Control,
	selection transfer.OutputSelection,
	ancestry resumestate.OutputAncestryBinding,
	name string,
	candidate outputcap.Directory,
) (SessionOpenResult, error) {
	if err := removeOutputSessionCandidate(intent, name, candidate); err != nil {
		return SessionOpenResult{}, err
	}
	return controller.createSessionDirectory(intent, control, selection, ancestry)
}

func installCompletedSessionCandidate(
	intent outputcap.Directory,
	candidate outputcap.Directory,
	sessionID transfer.OutputSessionID,
) (SessionOpenResult, error) {
	installedName := resumestate.SessionDirectoryName(sessionID)
	installed, err := installOutputSessionCandidate(intent, candidate, installedName)
	closeErr := candidate.Close()
	if err != nil || closeErr != nil {
		if installed != nil {
			_ = installed.Close()
		}
		return SessionOpenResult{}, errors.Join(err, closeErr)
	}
	return SessionOpenResult{Name: installedName, Directory: installed, Disposition: SessionInstalled}, nil
}

func (controller Controller) createSessionDirectory(
	intent outputcap.Directory,
	control resumestate.Control,
	selection transfer.OutputSelection,
	ancestry resumestate.OutputAncestryBinding,
) (SessionOpenResult, error) {
	for range AllocationAttempts {
		sessionID, err := controller.sessionIDs.NewOutputSessionID()
		if err != nil {
			return SessionOpenResult{}, err
		}
		if sessionID.IsZero() {
			continue
		}
		candidateName := resumestate.SessionCandidateName(sessionID)
		candidate, err := intent.CreateDirectory(candidateName, true)
		if errors.Is(err, outputcap.ErrNamespaceCollision) {
			continue
		}
		if err != nil {
			return SessionOpenResult{}, err
		}
		if err := errors.Join(candidate.Sync(), intent.Sync()); err != nil {
			return SessionOpenResult{}, errors.Join(err, candidate.Close())
		}
		header, err := controller.newHeader(control, selection, ancestry, sessionID)
		if err != nil {
			return SessionOpenResult{}, errors.Join(err, candidate.Close())
		}
		if err := controller.completeOutputSessionCandidate(candidate, header); err != nil {
			return SessionOpenResult{}, errors.Join(err, candidate.Close())
		}
		installedName := resumestate.SessionDirectoryName(sessionID)
		installed, err := installOutputSessionCandidate(intent, candidate, installedName)
		closeErr := candidate.Close()
		if err != nil || closeErr != nil {
			if installed != nil {
				_ = installed.Close()
			}
			return SessionOpenResult{}, errors.Join(err, closeErr)
		}
		return SessionOpenResult{Name: installedName, Directory: installed, Disposition: SessionInstalled}, nil
	}
	return SessionOpenResult{}, fmt.Errorf("%w: allocate output session identity", outputfault.ErrIntentUnsafe)
}

func (controller Controller) completeOutputSessionCandidate(
	candidate outputcap.Directory,
	header resumestate.Header,
) error {
	if err := validateOutputSessionCandidateCreationCut(candidate); err != nil {
		return err
	}
	encoded, err := resumestate.EncodeHeader(header)
	if err != nil {
		return err
	}
	store := controller.Store(header.IntentDigest(), header.SessionID())
	if _, err := store.EnsureInitialRecord(
		candidate, resumestate.HeaderRecordName, encoded, resumestate.MaxSessionHeaderBytes,
	); err != nil {
		return err
	}
	state, err := inspectOutputSessionCandidate(candidate, header)
	if err != nil {
		return err
	}
	if state == sessionCandidateComplete {
		return nil
	}
	names, err := candidate.Names(len(sessionCandidateChildren) + 1)
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
	if err != nil || state != sessionCandidateComplete {
		return errors.Join(outputfault.ErrIntentUnsafe, err)
	}
	return nil
}

// validateOutputSessionCandidateCreationCut recognizes only durable prefixes of
// the creation order. In particular, a later child can never authorize
// synthesizing the missing header that must be the candidate's first durable
// controller. Canonical header temporaries are the one pre-header crash cut.
func validateOutputSessionCandidateCreationCut(candidate outputcap.Directory) error {
	names, err := candidate.Names(
		len(sessionCandidateChildren) + AllocationAttempts + 1,
	)
	if err != nil {
		return err
	}
	present := make(map[string]struct{}, len(sessionCandidateChildren))
	temporaryCount := 0
	for _, name := range names {
		kind, exact, err := candidate.ClassifyExactEntry(name)
		if err != nil || !exact {
			return errors.Join(outputfault.ErrIntentUnsafe, err)
		}
		if resumestate.ClassifyHeaderUpdateTemporaryName(name).Classification() ==
			resumestate.HeaderUpdateTemporaryCanonical {
			if kind != outputcap.EntryRegularFile {
				return outputfault.ErrIntentUnsafe
			}
			temporaryCount++
			if temporaryCount > AllocationAttempts {
				return outputfault.ErrIntentUnsafe
			}
			continue
		}
		index := slices.Index(sessionCandidateChildren, name)
		if index < 0 {
			return outputfault.ErrIntentUnsafe
		}
		expectedKind := outputcap.EntryDirectory
		if index < 2 {
			expectedKind = outputcap.EntryRegularFile
		}
		if kind != expectedKind {
			return outputfault.ErrIntentUnsafe
		}
		present[name] = struct{}{}
	}

	_, headerPresent := present[resumestate.HeaderRecordName]
	if !headerPresent {
		if len(present) != 0 {
			return outputfault.ErrIntentUnsafe
		}
		return nil
	}
	if temporaryCount != 0 && len(present) != 1 {
		return outputfault.ErrIntentUnsafe
	}
	for index, name := range sessionCandidateChildren {
		_, exists := present[name]
		if index < len(present) && !exists || index >= len(present) && exists {
			return outputfault.ErrIntentUnsafe
		}
	}
	return nil
}

func inspectOutputSessionCandidate(
	candidate outputcap.Directory,
	expected resumestate.Header,
) (sessionCandidateState, error) {
	names, err := candidate.Names(len(sessionCandidateChildren) + 1)
	if err != nil {
		return 0, err
	}
	if len(names) == 0 {
		return sessionCandidateEmpty, nil
	}
	present := make(map[string]struct{}, len(names))
	for _, name := range names {
		if !slices.Contains(sessionCandidateChildren, name) {
			return 0, outputfault.ErrIntentUnsafe
		}
		present[name] = struct{}{}
	}
	for index, name := range sessionCandidateChildren {
		_, ok := present[name]
		if index < len(names) && !ok || index >= len(names) && ok {
			return 0, outputfault.ErrIntentUnsafe
		}
	}
	encoded, err := ReadRecord(candidate, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
	if err != nil {
		return 0, err
	}
	header, err := resumestate.DecodeHeader(encoded)
	if err != nil {
		return 0, errors.Join(outputfault.ErrIntentUnsafe, err)
	}
	if header.OutputAncestry() != expected.OutputAncestry() {
		return 0, errors.Join(
			outputfault.ErrIntentUnsafe,
			&AncestryMismatchError{sessionID: header.SessionID()},
		)
	}
	if header != expected {
		return 0, outputfault.ErrIntentUnsafe
	}
	if _, ok := present[resumestate.SessionLockName]; ok {
		lock, err := candidate.OpenFile(resumestate.SessionLockName, true, false)
		if err != nil {
			return 0, err
		}
		size, sizeErr := lock.Size()
		closeErr := lock.Close()
		if sizeErr != nil || closeErr != nil || size != 0 {
			return 0, errors.Join(outputfault.ErrIntentUnsafe, sizeErr, closeErr)
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
			return 0, errors.Join(outputfault.ErrIntentUnsafe, listErr, closeErr)
		}
	}
	if len(names) == len(sessionCandidateChildren) {
		return sessionCandidateComplete, nil
	}
	return sessionCandidatePartial, nil
}

func installOutputSessionCandidate(
	intent outputcap.Directory,
	candidate outputcap.Directory,
	installedName string,
) (outputcap.Directory, error) {
	installed, err := intent.InstallDirectoryNoReplace(candidate, installedName)
	if err != nil {
		return nil, err
	}
	same, compareErr := installed.SameDirectory(candidate)
	if compareErr != nil || !same {
		return installed, errors.Join(outputfault.ErrIntentUnsafe, compareErr)
	}
	if err := intent.Sync(); err != nil {
		return installed, err
	}
	if err := VerifyPinnedDirectoryEntry(intent, installedName, candidate); err != nil {
		return installed, err
	}
	return installed, nil
}

func removeOutputSessionCandidate(
	intent outputcap.Directory,
	name string,
	candidate outputcap.Directory,
) error {
	if err := VerifyPinnedDirectoryEntry(intent, name, candidate); err != nil {
		return errors.Join(err, candidate.Close())
	}
	remaining, err := candidate.Names(1)
	if err != nil || len(remaining) != 0 {
		return errors.Join(outputfault.ErrIntentUnsafe, err, candidate.Close())
	}
	if err := intent.RemoveDirectory(name, candidate); err != nil {
		return errors.Join(err, candidate.Close())
	}
	return errors.Join(intent.Sync(), candidate.Close())
}
