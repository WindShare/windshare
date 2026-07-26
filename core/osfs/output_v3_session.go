package osfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type filesystemOutputSession struct {
	owner       *FilesystemOutputAuthority
	platform    outputV3Platform
	control     *outputControlNamespace
	intentDir   outputV3Directory
	sessionDir  outputV3Directory
	filesDir    outputV3Directory
	anchorsDir  outputV3Directory
	stagesDir   outputV3Directory
	sessionLock outputV3Lock
	state       resumestate.SessionAuthority
	// Lifecycle replacement mutates state; identity getters use these immutable
	// bindings so observability cannot race a durable header transition.
	sessionID        transfer.OutputSessionID
	selection        transfer.OutputSelection
	resumeIntent     transfer.ResumeIntent
	ancestry         outputAncestrySnapshot
	store            outputStateStore
	capabilities     transfer.OutputCapabilities
	selectedFiles    map[string]transfer.OutputSelectionFile
	selectedDirs     map[string]transfer.OutputSelectionDirectory
	objectClaims     map[resumestate.OutputObjectID]resumestate.LocatorDigest
	duplicateObjects map[resumestate.LocatorDigest]struct{}

	operationGate sync.RWMutex
	stateInstall  sync.RWMutex
	mu            sync.Mutex
	poisonOnce    sync.Once
	beginWG       sync.WaitGroup
	beginning     map[resumestate.LocatorDigest]struct{}
	active        map[resumestate.LocatorDigest]*filesystemFileTransaction
	attention     []ResumeAttention
	settling      bool
	poisoned      bool
	exposed       bool
	closed        bool
}

func (authority *FilesystemOutputAuthority) openOutputSession(
	ctx context.Context,
	platform outputV3Platform,
	control *outputControlNamespace,
	admission outputSelectionAdmission,
) (*filesystemOutputSession, bool, []ResumeAttention, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, nil, err
	}
	coordinator, err := authority.acquireRuntimeNativeLock(
		func() (outputV3Lock, bool, error) {
			return control.directory.AcquireLock(resumestate.CoordinatorLockName, true)
		},
		filesystemOutputNativeLockContext{
			resumeIntent:         admission.selection.ResumeIntent(),
			selectionIdentity:    admission.selection.Identity(),
			outputAncestryDigest: filesystemOutputAncestryDigestFromState(admission.ancestry.binding),
			certification:        filesystemOutputCertificationFromState(platform.Certification()),
			scope:                FilesystemOutputNativeLockCoordinator, failureScope: transfer.OutputFaultRoot,
		},
		rootOutputFault("acquire coordinator lock", errOutputRootUnsafe),
	)
	if err != nil {
		return nil, false, nil, err
	}
	coordinatorOwned := true
	defer func() {
		if coordinatorOwned {
			_ = coordinator.Close()
		}
	}()

	if err := authority.revalidateOutputAdmissionAncestry(admission); err != nil {
		return nil, false, nil, err
	}
	intentName := resumestate.ResumeNamespaceName(admission.selection.ResumeIntent())
	intentDirectory, err := openCanonicalIntentDirectory(control.sessions, admission.selection.ResumeIntent())
	if err != nil {
		return nil, false, nil, intentOutputFault("open resume-intent namespace", err)
	}
	intentOwned := true
	defer func() {
		if intentOwned {
			_ = intentDirectory.Close()
		}
	}()
	sessionName, sessionDirectory, createdSession, err := authority.openOrCreateSessionDirectory(
		intentDirectory, control.control, admission.selection, admission.ancestry.binding,
	)
	if err != nil {
		var mismatch *outputAncestryHeaderMismatch
		if errors.As(err, &mismatch) {
			authority.traceOutputAncestry(
				admission.selection, mismatch.sessionID, resumestate.LocatorDigest{}, admission.ancestry,
				len(admission.ancestry.entries), outputAncestryAdmissionBoundary(true),
				FilesystemOutputAncestryMismatch,
			)
			return nil, false, nil, outputAncestryPauseFault("bind session-candidate ancestry", err)
		}
		return nil, false, nil, intentOutputFault("open session namespace", err)
	}
	sessionOwned := true
	defer func() {
		if sessionOwned {
			_ = sessionDirectory.Close()
		}
	}()

	lockKind, err := observeExactOutputEntry(sessionDirectory, resumestate.SessionLockName)
	if err != nil {
		return nil, false, nil, intentOutputFault("observe session lock", err)
	}
	if lockKind == outputV3EntryAbsent {
		if err := authority.revalidateOutputAdmissionAncestry(admission); err != nil {
			return nil, false, nil, err
		}
		state, bindErr := authority.bindOutputSessionStateWithAncestryTrace(
			control.control, sessionDirectory, admission.selection, admission.ancestry.binding,
			intentName, sessionName, admission,
		)
		if isMissing(bindErr) {
			names, listErr := sessionDirectory.Names(1)
			if listErr == nil && len(names) == 0 {
				if removeErr := removeEmptySessionShell(
					control.sessions, intentDirectory, sessionDirectory, intentName, sessionName,
				); removeErr == nil {
					sessionOwned, intentOwned = false, false
					closeErr := errors.Join(sessionDirectory.Close(), intentDirectory.Close(), coordinator.Close())
					coordinatorOwned = false
					if closeErr != nil {
						return nil, false, nil, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, closeErr)
					}
					return authority.openOutputSession(ctx, platform, control, admission)
				}
			}
		}
		if bindErr != nil {
			return nil, false, nil, intentOutputFault("bind lockless session header", bindErr)
		}
		if state.Header().Lifecycle() != resumestate.SessionCompleting &&
			state.Header().Lifecycle() != resumestate.SessionDiscarding {
			return nil, false, nil, intentOutputFault("validate lockless session cut", errOutputIntentUnsafe)
		}
		verifyAuthority := func() error {
			return verifyTerminalSessionAuthority(control, intentDirectory, sessionDirectory, state.Header())
		}
		if err := verifyAuthority(); err != nil {
			return nil, false, nil, err
		}
		if err := reconcileHeaderRecordTemporaries(
			sessionDirectory, state.NamespaceAuthority(), verifyAuthority,
		); err != nil {
			return nil, false, nil, intentOutputFault("reconcile lockless terminal header update", err)
		}
		if err := authority.revalidateOutputAdmissionAncestry(admission); err != nil {
			return nil, false, nil, err
		}
		state, err = authority.bindOutputSessionStateWithAncestryTrace(
			control.control, sessionDirectory, admission.selection, admission.ancestry.binding,
			intentName, sessionName, admission,
		)
		if err != nil {
			return nil, false, nil, intentOutputFault("rebind lockless terminal session", err)
		}
		if _, err := authority.recoverTerminalSession(
			platform, control, intentDirectory, sessionDirectory, state, admission,
		); err != nil {
			return nil, false, nil, err
		}
		sessionOwned, intentOwned = false, false
		closeErr := errors.Join(sessionDirectory.Close(), intentDirectory.Close(), coordinator.Close())
		coordinatorOwned = false
		if closeErr != nil {
			return nil, false, nil, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, closeErr)
		}
		return authority.openOutputSession(ctx, platform, control, admission)
	}
	if lockKind != outputV3EntryRegularFile {
		return nil, false, nil, intentOutputFault("classify session lock", errOutputIntentUnsafe)
	}
	lockSessionID, err := resumestate.ParseSessionDirectoryName(sessionName)
	if err != nil {
		return nil, false, nil, intentOutputFault("bind session lock identity", err)
	}
	lock, err := authority.acquireRuntimeNativeLock(
		func() (outputV3Lock, bool, error) {
			return sessionDirectory.AcquireLock(resumestate.SessionLockName, true)
		},
		filesystemOutputNativeLockContext{
			resumeIntent: admission.selection.ResumeIntent(), sessionID: lockSessionID,
			selectionIdentity:    admission.selection.Identity(),
			outputAncestryDigest: filesystemOutputAncestryDigestFromState(admission.ancestry.binding),
			certification:        filesystemOutputCertificationFromState(platform.Certification()),
			scope:                FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
		},
		intentOutputFault("acquire session lock", errOutputIntentUnsafe),
	)
	if err != nil {
		return nil, false, nil, err
	}
	lockOwned := true
	defer func() {
		if lockOwned {
			_ = lock.Close()
		}
	}()
	verifyAuthority := func() error {
		return verifyPinnedOutputSession(
			control.sessions, intentDirectory, sessionDirectory, intentName, sessionName,
		)
	}
	if err := verifyAuthority(); err != nil {
		return nil, false, nil, intentOutputFault("revalidate locked session", err)
	}
	if err := authority.revalidateOutputAdmissionAncestry(admission); err != nil {
		return nil, false, nil, err
	}
	provisionalState, err := authority.bindOutputSessionStateWithAncestryTrace(
		control.control, sessionDirectory, admission.selection, admission.ancestry.binding,
		intentName, sessionName, admission,
	)
	if err != nil {
		return nil, false, nil, intentOutputFault("bind locked session header for update recovery", err)
	}
	if err := reconcileHeaderRecordTemporaries(
		sessionDirectory, provisionalState.NamespaceAuthority(), verifyAuthority,
	); err != nil {
		return nil, false, nil, intentOutputFault("reconcile locked session-header update", err)
	}
	if err := authority.revalidateOutputAdmissionAncestry(admission); err != nil {
		return nil, false, nil, err
	}
	state, err := authority.bindOutputSessionStateWithAncestryTrace(
		control.control, sessionDirectory, admission.selection, admission.ancestry.binding,
		intentName, sessionName, admission,
	)
	if err != nil {
		return nil, false, nil, intentOutputFault("bind locked session authority", err)
	}
	if err := verifyAuthority(); err != nil {
		return nil, false, nil, intentOutputFault("revalidate bound session", err)
	}
	if state.Header().Lifecycle() == resumestate.SessionCompleting ||
		state.Header().Lifecycle() == resumestate.SessionDiscarding {
		if err := lock.Close(); err != nil {
			return nil, false, nil, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		lockOwned = false
		if err := authority.revalidateOutputAdmissionAncestry(admission); err != nil {
			return nil, false, nil, err
		}
		if _, err := authority.recoverTerminalSession(
			platform, control, intentDirectory, sessionDirectory, state, admission,
		); err != nil {
			return nil, false, nil, err
		}
		sessionOwned, intentOwned = false, false
		closeErr := errors.Join(sessionDirectory.Close(), intentDirectory.Close(), coordinator.Close())
		coordinatorOwned = false
		if closeErr != nil {
			return nil, false, nil, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, closeErr)
		}
		return authority.openOutputSession(ctx, platform, control, admission)
	}
	children, err := validateSessionChildren(sessionDirectory)
	if err != nil {
		return nil, false, nil, intentOutputFault("validate locked session namespace", err)
	}

	session := &filesystemOutputSession{
		owner: authority, platform: platform, control: control,
		intentDir: intentDirectory, sessionDir: sessionDirectory,
		filesDir: children.files, anchorsDir: children.anchors, stagesDir: children.stages,
		sessionLock: lock, state: state,
		sessionID: state.Header().SessionID(), selection: state.Selection(), resumeIntent: state.Header().ResumeIntent(),
		ancestry:         admission.ancestry,
		store:            authority.stateStore(state.Header().ResumeIntent(), state.Header().SessionID()),
		selectedFiles:    admission.files,
		selectedDirs:     admission.dirs,
		objectClaims:     make(map[resumestate.OutputObjectID]resumestate.LocatorDigest),
		duplicateObjects: make(map[resumestate.LocatorDigest]struct{}),
		beginning:        make(map[resumestate.LocatorDigest]struct{}),
		active:           make(map[resumestate.LocatorDigest]*filesystemFileTransaction),
	}
	session.capabilities, _ = transfer.NewOutputCapabilities(transfer.OutputCapabilities{
		Durability: transfer.DurabilityProcessRestart, Mode: transfer.OutputNativeTree,
		RandomWrite: true, FileFailureIsolation: true, ModifiedTime: true,
		ArchiveBoundary: transfer.ArchiveFailureNotApplicable,
	})
	if err := authority.revalidateOutputAdmissionAncestry(admission); err != nil {
		return nil, false, nil, errors.Join(err, closeSessionChildren(children))
	}
	fileNamespace, err := scanOutputV3FileNamespace(session)
	if err != nil {
		return nil, false, nil, errors.Join(err, closeSessionChildren(children))
	}
	if err := session.adoptFileNamespaceSnapshot(fileNamespace); err != nil {
		return nil, false, nil, errors.Join(err, closeSessionChildren(children))
	}
	if err := session.resumeLifecycle(); err != nil {
		return nil, false, nil, errors.Join(err, closeSessionChildren(children))
	}
	attention := slices.Clone(fileNamespace.attention)
	session.attention = attention
	coordinatorCloseErr := coordinator.Close()
	coordinatorOwned = false
	if coordinatorCloseErr != nil {
		return nil, false, nil, errors.Join(
			outputFault(transfer.OutputFaultRoot, transfer.OutputFaultStateIO, coordinatorCloseErr),
			closeSessionChildren(children),
		)
	}
	intentOwned, sessionOwned, lockOwned = false, false, false
	session.mu.Lock()
	session.exposed = true
	session.mu.Unlock()
	authority.trace(FilesystemOutputTrace{
		Operation: TraceSessionOpened, ResumeIntent: admission.selection.ResumeIntent(),
		SessionID: state.Header().SessionID(),
	})
	return session, createdSession, slices.Clone(attention), nil
}

func (authority *FilesystemOutputAuthority) revalidateOutputAdmissionAncestry(
	admission outputSelectionAdmission,
) error {
	if admission.validation == nil {
		err := errors.Join(errOutputAncestryUnsafe, errors.New("output ancestry admission guard is absent"))
		authority.traceOutputAncestry(
			admission.selection, transfer.OutputSessionID{}, resumestate.LocatorDigest{}, admission.ancestry,
			len(admission.ancestry.entries), outputAncestryAdmissionBoundary(admission.resuming),
			outputAncestryTraceDecision(err),
		)
		return outputAncestryPauseFault(
			"revalidate output ancestry admission",
			err,
		)
	}
	if err := admission.validation.Revalidate(outputAncestryRequirement{}); err != nil {
		authority.traceOutputAncestry(
			admission.selection, transfer.OutputSessionID{}, resumestate.LocatorDigest{}, admission.ancestry,
			len(admission.ancestry.entries), outputAncestryAdmissionBoundary(admission.resuming),
			outputAncestryTraceDecision(err),
		)
		return outputAncestryPauseFault("revalidate output ancestry admission", err)
	}
	authority.traceOutputAncestry(
		admission.selection, transfer.OutputSessionID{}, resumestate.LocatorDigest{}, admission.ancestry,
		len(admission.ancestry.entries), outputAncestryAdmissionBoundary(admission.resuming),
		FilesystemOutputAncestryMatched,
	)
	return nil
}

func (authority *FilesystemOutputAuthority) stateStore(
	intent transfer.ResumeIntent,
	sessionID transfer.OutputSessionID,
) outputStateStore {
	if authority == nil {
		return outputStateStore{}
	}
	return outputStateStore{
		random: authority.random,
		traceAdoptedInstall: func(cut outputStateInstallCut) {
			authority.traceAdoptedStateInstallCut(intent, sessionID, cut)
		},
	}
}

func (authority *FilesystemOutputAuthority) traceAdoptedStateInstallCut(
	intent transfer.ResumeIntent,
	sessionID transfer.OutputSessionID,
	cut outputStateInstallCut,
) {
	if authority == nil {
		return
	}
	generation, err := decodeStateRecordGeneration(cut.targetName, cut.encoded)
	if err != nil {
		return
	}
	var locator resumestate.LocatorDigest
	if cut.targetName != resumestate.ControlRecordName &&
		cut.targetName != resumestate.HeaderRecordName &&
		len(cut.targetName) >= resumestate.ShardHexCharacters {
		locator, err = resumestate.ParseFileRecordName(
			cut.targetName[:resumestate.ShardHexCharacters], cut.targetName,
		)
		if err != nil {
			return
		}
	}
	authority.trace(FilesystemOutputTrace{
		Operation: TraceStateInstallCutAdopted, ResumeIntent: intent,
		SessionID: sessionID, LocatorDigest: outputLocatorDigestFromState(locator), StateGeneration: generation,
		StateInstallStage: cut.stage, MutationReportedFailure: cut.mutationReportedFailure,
		ParentSyncReportedFailure: cut.parentSyncReportedFailure,
	})
}

type outputSessionChildren struct {
	files   outputV3Directory
	anchors outputV3Directory
	stages  outputV3Directory
}

func closeSessionChildren(children outputSessionChildren) error {
	return errors.Join(children.files.Close(), children.anchors.Close(), children.stages.Close())
}

func bindOutputSessionState(
	control resumestate.Control,
	sessionDirectory outputV3Directory,
	selection transfer.OutputSelection,
	ancestry resumestate.OutputAncestryBinding,
	intentName string,
	sessionName string,
) (resumestate.SessionAuthority, error) {
	headerBytes, err := readStateRecord(
		sessionDirectory, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes,
	)
	if err != nil {
		return resumestate.SessionAuthority{}, err
	}
	header, err := resumestate.DecodeHeader(headerBytes)
	if err != nil {
		return resumestate.SessionAuthority{}, err
	}
	if header.OutputAncestry() != ancestry {
		return resumestate.SessionAuthority{}, errors.Join(
			errOutputIntentUnsafe,
			&outputAncestryHeaderMismatch{sessionID: header.SessionID()},
		)
	}
	return resumestate.BindSessionAuthority(control, header, selection, intentName, sessionName)
}

type outputAncestryHeaderMismatch struct {
	sessionID transfer.OutputSessionID
}

func (mismatch *outputAncestryHeaderMismatch) Error() string {
	return "session output ancestry differs from current canonical selection"
}

func (mismatch *outputAncestryHeaderMismatch) Unwrap() error { return errOutputAncestryMismatch }

func (authority *FilesystemOutputAuthority) bindOutputSessionStateWithAncestryTrace(
	control resumestate.Control,
	sessionDirectory outputV3Directory,
	selection transfer.OutputSelection,
	ancestry resumestate.OutputAncestryBinding,
	intentName string,
	sessionName string,
	admission outputSelectionAdmission,
) (resumestate.SessionAuthority, error) {
	state, err := bindOutputSessionState(
		control, sessionDirectory, selection, ancestry, intentName, sessionName,
	)
	boundary := FilesystemOutputAncestryAdmission
	if admission.resuming {
		boundary = FilesystemOutputAncestryRestart
	}
	if err == nil {
		authority.traceOutputAncestry(
			selection, state.Header().SessionID(), resumestate.LocatorDigest{}, admission.ancestry,
			len(admission.ancestry.entries), boundary, FilesystemOutputAncestryMatched,
		)
		return state, nil
	}
	var mismatch *outputAncestryHeaderMismatch
	if errors.As(err, &mismatch) {
		authority.traceOutputAncestry(
			selection, mismatch.sessionID, resumestate.LocatorDigest{}, admission.ancestry,
			len(admission.ancestry.entries), boundary, FilesystemOutputAncestryMismatch,
		)
		return resumestate.SessionAuthority{}, outputAncestryPauseFault(
			"bind session header ancestry", err,
		)
	}
	return resumestate.SessionAuthority{}, err
}

func verifyPinnedOutputSession(
	sessionsDirectory outputV3Directory,
	intentDirectory outputV3Directory,
	sessionDirectory outputV3Directory,
	intentName string,
	sessionName string,
) error {
	if err := verifyPinnedDirectoryEntry(sessionsDirectory, intentName, intentDirectory); err != nil {
		return err
	}
	return verifyPinnedDirectoryEntry(intentDirectory, sessionName, sessionDirectory)
}

func validateSessionChildren(session outputV3Directory) (outputSessionChildren, error) {
	names, err := session.Names(6)
	if err != nil {
		return outputSessionChildren{}, err
	}
	slices.Sort(names)
	expected := []string{
		resumestate.HeaderRecordName, resumestate.SessionLockName, resumestate.FilesDirectoryName,
		resumestate.AnchorsDirectoryName, resumestate.StagesDirectoryName,
	}
	slices.Sort(expected)
	if !slices.Equal(names, expected) {
		return outputSessionChildren{}, errOutputIntentUnsafe
	}
	files, err := session.OpenDirectory(resumestate.FilesDirectoryName, true)
	if err != nil {
		return outputSessionChildren{}, err
	}
	anchors, err := session.OpenDirectory(resumestate.AnchorsDirectoryName, true)
	if err != nil {
		return outputSessionChildren{}, errors.Join(err, files.Close())
	}
	stages, err := session.OpenDirectory(resumestate.StagesDirectoryName, true)
	if err != nil {
		return outputSessionChildren{}, errors.Join(err, files.Close(), anchors.Close())
	}
	return outputSessionChildren{files: files, anchors: anchors, stages: stages}, nil
}

func (session *filesystemOutputSession) resumeLifecycle() error {
	for {
		switch session.stateSnapshot().Header().Lifecycle() {
		case resumestate.SessionActive:
			return nil
		case resumestate.SessionPaused, resumestate.SessionPausedNeedsAttention:
			return session.installLifecycle(resumestate.SessionActive)
		case resumestate.SessionPausing:
			if err := session.installLifecycle(resumestate.SessionPaused); err != nil {
				return err
			}
		case resumestate.SessionCompleting, resumestate.SessionDiscarding:
			return transfer.NewOutputSessionError(
				outputFault(transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
					fmt.Errorf("%w: terminal session transition needs recovery", errOutputIntentUnsafe)),
				true,
			)
		default:
			return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateCorrupt, errOutputIntentUnsafe)
		}
	}
}

func (session *filesystemOutputSession) stateSnapshot() resumestate.SessionAuthority {
	if session == nil {
		return resumestate.SessionAuthority{}
	}
	session.stateInstall.RLock()
	defer session.stateInstall.RUnlock()
	return session.state
}

func (session *filesystemOutputSession) installLifecycle(next resumestate.SessionLifecycle) error {
	session.stateInstall.Lock()
	defer session.stateInstall.Unlock()
	if session.stateWritesDisabled() {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultOwnership, errOutputSessionClosed)
	}
	updated, err := session.state.WithLifecycle(next)
	if err != nil {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
	}
	currentEncoded, err := resumestate.EncodeHeader(session.state.Header())
	if err != nil {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
	}
	nextEncoded, err := resumestate.EncodeHeader(updated.Header())
	if err != nil {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
	}
	outcome, replaceErr := session.store.replaceRecord(
		session.sessionDir,
		resumestate.HeaderRecordName,
		outputStateRecordImage{encoded: currentEncoded, generation: session.state.Header().StateGeneration()},
		outputStateRecordImage{encoded: nextEncoded, generation: updated.Header().StateGeneration()},
		resumestate.MaxSessionHeaderBytes,
	)
	switch outcome {
	case outputStateReplaceAdopted:
		session.state = updated
		if replaceErr != nil {
			// A verified next generation is the only state this owner may retain,
			// but a failed cleanup means this process cannot safely continue to
			// mutate the namespace. A fresh opener must recover from the adopted
			// generation after the current owner releases its handles.
			session.poisonState()
			return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, replaceErr)
		}
		return nil
	case outputStateReplaceUnchanged:
		if replaceErr == nil {
			replaceErr = errOutputV3Unsafe
		}
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, replaceErr)
	case outputStateReplaceUncertain:
		session.poisonState()
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO,
			errors.Join(errOutputV3Unsafe, replaceErr))
	default:
		session.poisonState()
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultContract, resumestate.ErrInvalidState)
	}
}

func (session *filesystemOutputSession) poisonState() {
	if session == nil {
		return
	}
	session.mu.Lock()
	session.poisoned = true
	session.settling = true
	exposed := session.exposed
	session.mu.Unlock()
	if exposed {
		session.poisonOnce.Do(func() {
			go session.teardownPoisoned()
		})
	}
}

func (session *filesystemOutputSession) teardownPoisoned() {
	session.operationGate.Lock()
	defer session.operationGate.Unlock()
	_ = session.shutdownOwnerLocked()
}

// shutdownOwnerLocked deliberately performs no state mutation. Once durable
// authority is uncertain, only a fresh opener may interpret the namespace; the
// current owner is limited to draining in-memory work and releasing its handles.
func (session *filesystemOutputSession) shutdownOwnerLocked() error {
	session.beginWG.Wait()
	session.mu.Lock()
	transactions := make([]*filesystemFileTransaction, 0, len(session.active))
	for _, transaction := range session.active {
		transactions = append(transactions, transaction)
	}
	session.settling = true
	session.closed = true
	session.mu.Unlock()
	var closeErr error
	for _, transaction := range transactions {
		transaction.mu.Lock()
		transaction.lifecycle = filesystemFileTransactionClosed
		closeErr = errors.Join(closeErr, transaction.closeHandlesLocked())
		digest := transaction.resumable.Bound().Record().LocatorDigest()
		transaction.mu.Unlock()
		session.finishFile(digest, transaction)
	}
	return errors.Join(closeErr, session.closeHandles())
}

func (session *filesystemOutputSession) beginOperation() error {
	if session == nil {
		return transfer.ErrInvalidOutputBinding
	}
	session.operationGate.RLock()
	if session.operationDisabled() {
		session.operationGate.RUnlock()
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultOwnership, errOutputSessionClosed)
	}
	return nil
}

func (session *filesystemOutputSession) endOperation() {
	session.operationGate.RUnlock()
}

func (session *filesystemOutputSession) operationDisabled() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.closed || session.settling || session.poisoned
}

func (session *filesystemOutputSession) stateWritesDisabled() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.closed || session.poisoned
}

func (session *filesystemOutputSession) BackendID() transfer.OutputBackendID {
	return filesystemOutputBackendID
}

func (session *filesystemOutputSession) SessionID() transfer.OutputSessionID {
	if session == nil {
		return transfer.OutputSessionID{}
	}
	return session.sessionID
}

func (session *filesystemOutputSession) Capabilities() transfer.OutputCapabilities {
	if session == nil {
		return transfer.OutputCapabilities{}
	}
	return session.capabilities
}

func classifyLockFailure(scope transfer.OutputFaultScope, err error) error {
	if errors.Is(err, errOutputV3LockBusy) {
		return outputFault(scope, transfer.OutputFaultOwnership, errors.Join(errOutputSessionActive, err))
	}
	return outputFault(scope, transfer.OutputFaultStateIO, err)
}

func intentOutputFault(operation string, cause error) error {
	return transfer.NewOutputSessionError(
		outputFault(transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
			errors.Join(errOutputIntentUnsafe, fmt.Errorf("%s: %w", operation, cause))),
		true,
	)
}

func (session *filesystemOutputSession) closeHandles() error {
	if session == nil {
		return nil
	}
	session.mu.Lock()
	sessionLock := session.sessionLock
	filesDir, anchorsDir, stagesDir := session.filesDir, session.anchorsDir, session.stagesDir
	sessionDir, intentDir := session.sessionDir, session.intentDir
	control, platform := session.control, session.platform
	session.sessionLock = nil
	session.filesDir, session.anchorsDir, session.stagesDir = nil, nil, nil
	session.sessionDir, session.intentDir = nil, nil
	session.control, session.platform = nil, nil
	session.mu.Unlock()
	var controlErr, platformErr error
	if control != nil {
		controlErr = control.Close()
	}
	if platform != nil {
		platformErr = platform.Close()
	}
	return errors.Join(
		closeOutputV3Lock(sessionLock),
		closeOutputV3Directory(filesDir),
		closeOutputV3Directory(anchorsDir),
		closeOutputV3Directory(stagesDir),
		closeOutputV3Directory(sessionDir),
		closeOutputV3Directory(intentDir),
		controlErr, platformErr,
	)
}

func validStateShard(name string) bool {
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

var _ transfer.OutputSession = (*filesystemOutputSession)(nil)
var _ = fs.ErrNotExist
