package outputruntime

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type Session struct {
	owner       *Authority
	platform    outputcap.Platform
	control     *outputnamespace.ControlNamespace
	intentDir   outputcap.Directory
	sessionDir  outputcap.Directory
	filesDir    outputcap.Directory
	anchorsDir  outputcap.Directory
	stagesDir   outputcap.Directory
	sessionLock outputcap.Lock
	state       resumestate.SessionAuthority
	// Lifecycle replacement mutates state; identity getters use these immutable
	// bindings so observability cannot race a durable header transition.
	sessionID        transfer.OutputSessionID
	selection        transfer.OutputSelection
	resumeIntent     transfer.ResumeIntent
	ancestry         outputAncestrySnapshot
	store            outputnamespace.Store
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
	active        map[resumestate.LocatorDigest]*FileTransaction
	attention     []ResumeAttention
	settling      bool
	poisoned      bool
	exposed       bool
	closed        bool
}

func (authority *Authority) openOutputSession(
	ctx context.Context,
	platform outputcap.Platform,
	control *outputnamespace.ControlNamespace,
	admission outputSelectionAdmission,
) (*Session, bool, []ResumeAttention, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, nil, err
	}
	attempt, err := authority.acquireOutputSessionOpenAttempt(platform, control, admission)
	if err != nil {
		return nil, false, nil, err
	}
	return attempt.finish(ctx)
}

// outputSessionOpenAttempt owns one coordinator-protected namespace view. A
// retry closes every handle before reacquiring the coordinator, so a recovered
// terminal cut can never leak stale directory authority into the next attempt.
type outputSessionOpenAttempt struct {
	authority *Authority
	platform  outputcap.Platform
	control   *outputnamespace.ControlNamespace
	admission outputSelectionAdmission

	coordinator, sessionLock          outputcap.Lock
	intentDirectory, sessionDirectory outputcap.Directory
	intentName, sessionName           string
	createdSession                    bool
	coordinatorOwned, intentOwned     bool
	sessionOwned, sessionLockOwned    bool
}

func (authority *Authority) acquireOutputSessionOpenAttempt(platform outputcap.Platform, control *outputnamespace.ControlNamespace, admission outputSelectionAdmission) (attempt *outputSessionOpenAttempt, resultErr error) {
	attempt = &outputSessionOpenAttempt{
		authority: authority, platform: platform, control: control, admission: admission,
	}
	coordinator, err := authority.acquireRuntimeNativeLock(
		func() (outputcap.Lock, bool, error) {
			return control.Directory().AcquireLock(resumestate.CoordinatorLockName, true)
		},
		filesystemOutputNativeLockContext{
			resumeIntent:         admission.selection.ResumeIntent(),
			selectionIdentity:    admission.selection.Identity(),
			outputAncestryDigest: filesystemOutputAncestryDigestFromState(admission.ancestry.binding),
			certification:        filesystemOutputCertificationFromState(platform.Certification()),
			scope:                FilesystemOutputNativeLockCoordinator, failureScope: transfer.OutputFaultRoot,
		},
		outputnamespace.RootFault("acquire coordinator lock", outputfault.ErrRootUnsafe),
	)
	if err != nil {
		return nil, err
	}
	attempt.coordinator, attempt.coordinatorOwned = coordinator, true
	ownedAttempt := attempt
	defer func() {
		if resultErr != nil {
			// Named return assignment may clear attempt before this defer runs;
			// retain the owner pointer so every acquired handle is still released.
			_ = ownedAttempt.closeOwned()
		}
	}()
	if err := authority.revalidateOutputAdmissionAncestry(admission); err != nil {
		return nil, err
	}
	intentDirectory, err := outputnamespace.OpenCanonicalIntent(
		control.Sessions(), admission.selection.ResumeIntent(),
	)
	if err != nil {
		return nil, intentOutputFault("open resume-intent namespace", err)
	}
	attempt.intentDirectory, attempt.intentOwned = intentDirectory, true
	attempt.intentName = resumestate.ResumeNamespaceName(admission.selection.ResumeIntent())
	sessionResult, err := authority.namespaceController().OpenOrCreateSession(
		intentDirectory, control.Control(), admission.selection, admission.ancestry.binding,
	)
	if err != nil {
		if mismatch, found := errors.AsType[*outputnamespace.AncestryMismatchError](err); found {
			authority.traceOutputAncestry(
				admission.selection, mismatch.SessionID(), resumestate.LocatorDigest{}, admission.ancestry,
				len(admission.ancestry.entries), outputAncestryAdmissionBoundary(true), FilesystemOutputAncestryMismatch,
			)
			return nil, outputAncestryPauseFault("bind session-candidate ancestry", err)
		}
		return nil, intentOutputFault("open session namespace", err)
	}
	attempt.sessionName = sessionResult.Name
	attempt.sessionDirectory, attempt.sessionOwned = sessionResult.Directory, true
	attempt.createdSession = sessionResult.Disposition == outputnamespace.SessionInstalled
	return attempt, nil
}

func (attempt *outputSessionOpenAttempt) finish(ctx context.Context) (*Session, bool, []ResumeAttention, error) {
	defer func() { _ = attempt.closeOwned() }()
	lockKind, err := outputnamespace.ObserveExactEntry(attempt.sessionDirectory, resumestate.SessionLockName)
	if err != nil {
		return nil, false, nil, intentOutputFault("observe session lock", err)
	}
	switch lockKind {
	case outputcap.EntryAbsent:
		return attempt.finishLockless(ctx)
	case outputcap.EntryRegularFile:
		return attempt.finishLocked(ctx)
	default:
		return nil, false, nil, intentOutputFault("classify session lock", outputfault.ErrIntentUnsafe)
	}
}

func (attempt *outputSessionOpenAttempt) finishLockless(ctx context.Context) (*Session, bool, []ResumeAttention, error) {
	if err := attempt.authority.revalidateOutputAdmissionAncestry(attempt.admission); err != nil {
		return nil, false, nil, err
	}
	state, err := attempt.bindState()
	if err != nil {
		return attempt.handleLocklessBindFailure(ctx, err)
	}
	if !isTerminalOutputSessionLifecycle(state.Header().Lifecycle()) {
		return nil, false, nil, intentOutputFault("validate lockless session cut", outputfault.ErrIntentUnsafe)
	}
	header := state.Header()
	verifyAuthority := func() error {
		return outputnamespace.VerifyTerminalAuthority(
			attempt.control, attempt.intentDirectory, attempt.sessionDirectory, header,
		)
	}
	if err := verifyAuthority(); err != nil {
		return nil, false, nil, err
	}
	if err := outputnamespace.ReconcileHeaderRecordTemporaries(
		attempt.sessionDirectory, state.NamespaceAuthority(), verifyAuthority,
	); err != nil {
		return nil, false, nil, intentOutputFault("reconcile lockless terminal header update", err)
	}
	if err := attempt.authority.revalidateOutputAdmissionAncestry(attempt.admission); err != nil {
		return nil, false, nil, err
	}
	state, err = attempt.bindState()
	if err != nil {
		return nil, false, nil, intentOutputFault("rebind lockless terminal session", err)
	}
	return attempt.recoverTerminalAndRetry(ctx, state)
}

func (attempt *outputSessionOpenAttempt) handleLocklessBindFailure(ctx context.Context, bindErr error) (*Session, bool, []ResumeAttention, error) {
	if !isMissing(bindErr) {
		return nil, false, nil, intentOutputFault("bind lockless session header", bindErr)
	}
	names, listErr := attempt.sessionDirectory.Names(1)
	if listErr != nil || len(names) != 0 {
		return nil, false, nil, intentOutputFault("bind lockless session header", bindErr)
	}
	removeErr := outputnamespace.RemoveEmptySessionShell(
		attempt.control.Sessions(), attempt.intentDirectory, attempt.sessionDirectory,
		attempt.intentName, attempt.sessionName,
	)
	if removeErr != nil {
		return nil, false, nil, intentOutputFault("bind lockless session header", bindErr)
	}
	return attempt.retry(ctx)
}

func (attempt *outputSessionOpenAttempt) finishLocked(ctx context.Context) (*Session, bool, []ResumeAttention, error) {
	lockSessionID, err := resumestate.ParseSessionDirectoryName(attempt.sessionName)
	if err != nil {
		return nil, false, nil, intentOutputFault("bind session lock identity", err)
	}
	if err := attempt.acquireSessionLock(lockSessionID); err != nil {
		return nil, false, nil, err
	}
	state, err := attempt.bindLockedState()
	if err != nil {
		return nil, false, nil, err
	}
	if isTerminalOutputSessionLifecycle(state.Header().Lifecycle()) {
		if err := closeOutputSessionOpenHandle(&attempt.sessionLock, &attempt.sessionLockOwned); err != nil {
			return nil, false, nil, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		if err := attempt.authority.revalidateOutputAdmissionAncestry(attempt.admission); err != nil {
			return nil, false, nil, err
		}
		return attempt.recoverTerminalAndRetry(ctx, state)
	}
	return attempt.activate(state)
}

func (attempt *outputSessionOpenAttempt) acquireSessionLock(sessionID transfer.OutputSessionID) error {
	lock, err := attempt.authority.acquireRuntimeNativeLock(
		func() (outputcap.Lock, bool, error) {
			return attempt.sessionDirectory.AcquireLock(resumestate.SessionLockName, true)
		},
		filesystemOutputNativeLockContext{
			resumeIntent: attempt.admission.selection.ResumeIntent(), sessionID: sessionID,
			selectionIdentity:    attempt.admission.selection.Identity(),
			outputAncestryDigest: filesystemOutputAncestryDigestFromState(attempt.admission.ancestry.binding),
			certification:        filesystemOutputCertificationFromState(attempt.platform.Certification()),
			scope:                FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
		},
		intentOutputFault("acquire session lock", outputfault.ErrIntentUnsafe),
	)
	if err != nil {
		return err
	}
	attempt.sessionLock, attempt.sessionLockOwned = lock, true
	return nil
}

func (attempt *outputSessionOpenAttempt) bindLockedState() (resumestate.SessionAuthority, error) {
	verifyAuthority := func() error {
		return verifyPinnedOutputSession(
			attempt.control.Sessions(), attempt.intentDirectory, attempt.sessionDirectory,
			attempt.intentName, attempt.sessionName,
		)
	}
	if err := verifyAuthority(); err != nil {
		return resumestate.SessionAuthority{}, intentOutputFault("revalidate locked session", err)
	}
	if err := attempt.authority.revalidateOutputAdmissionAncestry(attempt.admission); err != nil {
		return resumestate.SessionAuthority{}, err
	}
	provisionalState, err := attempt.bindState()
	if err != nil {
		return resumestate.SessionAuthority{}, intentOutputFault("bind locked session header for update recovery", err)
	}
	if err := outputnamespace.ReconcileHeaderRecordTemporaries(
		attempt.sessionDirectory, provisionalState.NamespaceAuthority(), verifyAuthority,
	); err != nil {
		return resumestate.SessionAuthority{}, intentOutputFault("reconcile locked session-header update", err)
	}
	if err := attempt.authority.revalidateOutputAdmissionAncestry(attempt.admission); err != nil {
		return resumestate.SessionAuthority{}, err
	}
	state, err := attempt.bindState()
	if err != nil {
		return resumestate.SessionAuthority{}, intentOutputFault("bind locked session authority", err)
	}
	if err := verifyAuthority(); err != nil {
		return resumestate.SessionAuthority{}, intentOutputFault("revalidate bound session", err)
	}
	return state, nil
}

func (attempt *outputSessionOpenAttempt) bindState() (resumestate.SessionAuthority, error) {
	return attempt.authority.bindOutputSessionStateWithAncestryTrace(
		attempt.control.Control(), attempt.sessionDirectory,
		attempt.admission.selection, attempt.admission.ancestry.binding,
		attempt.intentName, attempt.sessionName, attempt.admission,
	)
}

func (attempt *outputSessionOpenAttempt) activate(state resumestate.SessionAuthority) (*Session, bool, []ResumeAttention, error) {
	children, err := validateSessionChildren(attempt.sessionDirectory)
	if err != nil {
		return nil, false, nil, intentOutputFault("validate locked session namespace", err)
	}
	session := attempt.newSession(state, children)
	attention, err := attempt.prepareActiveSession(session)
	if err != nil {
		return nil, false, nil, errors.Join(err, closeSessionChildren(children))
	}
	session.attention = slices.Clone(attention)
	if err := closeOutputSessionOpenHandle(&attempt.coordinator, &attempt.coordinatorOwned); err != nil {
		return nil, false, nil, errors.Join(
			outputfault.New(transfer.OutputFaultRoot, transfer.OutputFaultStateIO, err),
			closeSessionChildren(children),
		)
	}
	attempt.intentOwned, attempt.sessionOwned, attempt.sessionLockOwned = false, false, false
	session.mu.Lock()
	session.exposed = true
	session.mu.Unlock()
	attempt.authority.trace(FilesystemOutputTrace{
		Operation: TraceSessionOpened, ResumeIntent: attempt.admission.selection.ResumeIntent(),
		SessionID: state.Header().SessionID(),
	})
	return session, attempt.createdSession, slices.Clone(attention), nil
}

func (attempt *outputSessionOpenAttempt) newSession(state resumestate.SessionAuthority, children outputSessionChildren) *Session {
	session := &Session{
		owner: attempt.authority, platform: attempt.platform, control: attempt.control,
		intentDir: attempt.intentDirectory, sessionDir: attempt.sessionDirectory,
		filesDir: children.files, anchorsDir: children.anchors, stagesDir: children.stages,
		sessionLock: attempt.sessionLock, state: state,
		sessionID: state.Header().SessionID(), selection: state.Selection(), resumeIntent: state.Header().ResumeIntent(),
		ancestry:         attempt.admission.ancestry,
		store:            attempt.authority.stateStore(state.Header().ResumeIntent(), state.Header().SessionID()),
		selectedFiles:    attempt.admission.files,
		selectedDirs:     attempt.admission.dirs,
		objectClaims:     make(map[resumestate.OutputObjectID]resumestate.LocatorDigest),
		duplicateObjects: make(map[resumestate.LocatorDigest]struct{}),
		beginning:        make(map[resumestate.LocatorDigest]struct{}),
		active:           make(map[resumestate.LocatorDigest]*FileTransaction),
	}
	session.capabilities, _ = transfer.NewOutputCapabilities(transfer.OutputCapabilities{
		Durability: transfer.DurabilityProcessRestart, Mode: transfer.OutputNativeTree,
		RandomWrite: true, FileFailureIsolation: true, ModifiedTime: true,
		ArchiveBoundary: transfer.ArchiveFailureNotApplicable,
	})
	return session
}

func (attempt *outputSessionOpenAttempt) prepareActiveSession(session *Session) ([]ResumeAttention, error) {
	if err := attempt.authority.revalidateOutputAdmissionAncestry(attempt.admission); err != nil {
		return nil, err
	}
	fileNamespace, err := scanOutputV3FileNamespace(session)
	if err != nil {
		return nil, err
	}
	if err := session.adoptFileNamespaceSnapshot(fileNamespace); err != nil {
		return nil, err
	}
	if err := session.resumeLifecycle(); err != nil {
		return nil, err
	}
	return slices.Clone(fileNamespace.attention), nil
}

func (attempt *outputSessionOpenAttempt) recoverTerminalAndRetry(ctx context.Context, state resumestate.SessionAuthority) (*Session, bool, []ResumeAttention, error) {
	if _, err := attempt.authority.recoverTerminalSession(
		attempt.platform, attempt.control, attempt.intentDirectory,
		attempt.sessionDirectory, state, attempt.admission,
	); err != nil {
		return nil, false, nil, err
	}
	return attempt.retry(ctx)
}

func (attempt *outputSessionOpenAttempt) retry(ctx context.Context) (*Session, bool, []ResumeAttention, error) {
	if err := attempt.closeOwned(); err != nil {
		return nil, false, nil, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return attempt.authority.openOutputSession(ctx, attempt.platform, attempt.control, attempt.admission)
}

func (attempt *outputSessionOpenAttempt) closeOwned() error {
	return errors.Join(
		closeOutputSessionOpenHandle(&attempt.sessionLock, &attempt.sessionLockOwned),
		closeOutputSessionOpenHandle(&attempt.sessionDirectory, &attempt.sessionOwned),
		closeOutputSessionOpenHandle(&attempt.intentDirectory, &attempt.intentOwned),
		closeOutputSessionOpenHandle(&attempt.coordinator, &attempt.coordinatorOwned),
	)
}

func closeOutputSessionOpenHandle[T interface{ Close() error }](handle *T, owned *bool) error {
	if !*owned {
		return nil
	}
	*owned = false
	return (*handle).Close()
}

func isTerminalOutputSessionLifecycle(lifecycle resumestate.SessionLifecycle) bool {
	return lifecycle == resumestate.SessionCompleting || lifecycle == resumestate.SessionDiscarding
}

type filesystemOutputNativeLockContext struct {
	resumeIntent         transfer.ResumeIntent
	sessionID            transfer.OutputSessionID
	selectionIdentity    transfer.SelectionIdentity
	outputAncestryDigest FilesystemOutputAncestryDigest
	certification        FilesystemOutputCertificationID
	scope                FilesystemOutputNativeLockScope
	failureScope         transfer.OutputFaultScope
}

type filesystemOutputNativeLock struct {
	mu           sync.Mutex
	lock         outputcap.Lock
	closeErr     error
	closed       bool
	owner        *Authority
	traceContext filesystemOutputNativeLockContext
}

func (lock *filesystemOutputNativeLock) File() outputcap.File {
	if lock == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed || lock.lock == nil {
		return nil
	}
	return lock.lock.File()
}

func (lock *filesystemOutputNativeLock) Close() error {
	if lock == nil {
		return nil
	}
	lock.mu.Lock()
	if lock.closed {
		result := lock.closeErr
		lock.mu.Unlock()
		return result
	}
	lock.closed = true
	raw := lock.lock
	lock.lock = nil
	if raw != nil {
		lock.closeErr = raw.Close()
	}
	result := lock.closeErr
	lock.mu.Unlock()

	milestone := FilesystemOutputNativeLockReleased
	var traceErr error
	if result != nil {
		milestone = FilesystemOutputNativeLockReleaseReportedFailure
		traceErr = outputfault.New(lock.traceContext.failureScope, transfer.OutputFaultStateIO, result)
	}
	lock.owner.traceNativeLock(lock.traceContext, milestone, traceErr)
	return result
}

func (authority *Authority) acquireRuntimeNativeLock(
	acquire func() (outputcap.Lock, bool, error),
	traceContext filesystemOutputNativeLockContext,
	unexpected error,
) (outputcap.Lock, error) {
	raw, created, err := acquire()
	if err != nil {
		resultErr := classifyLockFailure(traceContext.failureScope, err)
		milestone := FilesystemOutputNativeLockAcquireFailed
		if errors.Is(err, outputcap.ErrNamespaceLockBusy) {
			milestone = FilesystemOutputNativeLockContended
		}
		authority.traceNativeLock(traceContext, milestone, resultErr)
		return nil, resultErr
	}
	if raw == nil {
		resultErr := unexpected
		if resultErr == nil {
			resultErr = outputfault.New(traceContext.failureScope, transfer.OutputFaultContract, outputcap.ErrUnsafeNamespace)
		}
		authority.traceNativeLock(traceContext, FilesystemOutputNativeLockAcquireFailed, resultErr)
		return nil, resultErr
	}
	lock := &filesystemOutputNativeLock{
		lock: raw, owner: authority, traceContext: traceContext,
	}
	authority.traceNativeLock(traceContext, FilesystemOutputNativeLockAcquired, nil)
	if !created {
		return lock, nil
	}
	resultErr := unexpected
	if resultErr == nil {
		resultErr = outputfault.New(traceContext.failureScope, transfer.OutputFaultContract, outputcap.ErrUnsafeNamespace)
	}
	if closeErr := lock.Close(); closeErr != nil {
		resultErr = errors.Join(
			resultErr,
			outputfault.New(traceContext.failureScope, transfer.OutputFaultStateIO, closeErr),
		)
	}
	return nil, resultErr
}

func (authority *Authority) traceNativeLock(
	traceContext filesystemOutputNativeLockContext,
	milestone FilesystemOutputNativeLockMilestone,
	err error,
) {
	event := FilesystemOutputTrace{
		Operation:            TraceNativeLock,
		ResumeIntent:         traceContext.resumeIntent,
		SessionID:            traceContext.sessionID,
		SelectionIdentity:    traceContext.selectionIdentity,
		OutputAncestryDigest: traceContext.outputAncestryDigest,
		Certification:        traceContext.certification,
		NativeLockScope:      traceContext.scope,
		NativeLockMilestone:  milestone,
		Failed: milestone == FilesystemOutputNativeLockAcquireFailed ||
			milestone == FilesystemOutputNativeLockReleaseReportedFailure,
	}
	if event.Failed {
		event.FailureScope, event.FailureCode = filesystemOutputTraceFailure(err)
	}
	authority.trace(event)
}

// recoverTerminalSession coordinates file-record settlement owned by osfs with
// the durable namespace suffix owned by outputnamespace.
func (authority *Authority) recoverTerminalSession(platform outputcap.Platform, control *outputnamespace.ControlNamespace, intentDirectory outputcap.Directory, sessionDirectory outputcap.Directory, state resumestate.SessionAuthority, admission outputSelectionAdmission) (resultAttention bool, resultErr error) {
	layout, err := outputnamespace.InspectTerminalLayout(
		sessionDirectory,
		state.Header(),
		func() (outputcap.Lock, error) {
			return authority.acquireRuntimeNativeLock(
				func() (outputcap.Lock, bool, error) {
					return sessionDirectory.AcquireLock(resumestate.SessionLockName, true)
				},
				filesystemOutputNativeLockContext{
					resumeIntent: state.Header().ResumeIntent(), sessionID: state.Header().SessionID(),
					selectionIdentity:    state.Header().SelectionIdentity(),
					outputAncestryDigest: filesystemOutputAncestryDigestFromState(state.Header().OutputAncestry()),
					certification:        filesystemOutputCertificationFromState(platform.Certification()),
					scope:                FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
				},
				intentOutputFault("acquire terminal-recovery session lock", outputfault.ErrIntentUnsafe),
			)
		},
	)
	if err != nil {
		return false, intentOutputFault("inspect terminal session cut", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, layout.Close())
	}()
	if err := outputnamespace.VerifyTerminalAuthority(
		control, intentDirectory, sessionDirectory, state.Header(),
	); err != nil {
		return false, err
	}

	session := &Session{
		owner: authority, platform: platform, control: control,
		intentDir: intentDirectory, sessionDir: sessionDirectory,
		filesDir: layout.Files(), anchorsDir: layout.Anchors(), stagesDir: layout.Stages(),
		sessionLock: layout.Lock(), state: state,
		sessionID: state.Header().SessionID(), selection: state.Selection(), resumeIntent: state.Header().ResumeIntent(),
		ancestry:         admission.ancestry,
		store:            authority.stateStore(state.Header().ResumeIntent(), state.Header().SessionID()),
		selectedFiles:    admission.files,
		selectedDirs:     admission.dirs,
		objectClaims:     make(map[resumestate.OutputObjectID]resumestate.LocatorDigest),
		duplicateObjects: make(map[resumestate.LocatorDigest]struct{}),
		beginning:        make(map[resumestate.LocatorDigest]struct{}),
		active:           make(map[resumestate.LocatorDigest]*FileTransaction),
	}

	attention, err := authority.completeTerminalSessionFiles(session, state, layout)
	if err != nil || attention {
		return attention, err
	}

	return false, outputnamespace.RecoverTerminalNamespace(
		control, intentDirectory, sessionDirectory, state.Header(), layout,
		state.Header().Lifecycle() == resumestate.SessionDiscarding,
	)
}

func (authority *Authority) completeTerminalSessionFiles(
	session *Session,
	state resumestate.SessionAuthority,
	layout *outputnamespace.TerminalLayout,
) (bool, error) {
	if state.Header().Lifecycle() != resumestate.SessionCompleting || layout.Cut() != 0 {
		return false, nil
	}
	preflight, err := scanOutputV3FileNamespace(session)
	if err != nil {
		return false, err
	}
	if err := session.adoptFileNamespaceSnapshot(preflight); err != nil {
		return false, err
	}
	attention, err := session.completeFileRecords(preflight)
	if err != nil {
		return false, err
	}
	if !attention {
		attention, err = session.inspectAndRemoveEmptyShards()
		if err != nil {
			return false, err
		}
	}
	if !attention {
		return false, nil
	}
	if err := session.installLifecycle(resumestate.SessionPausedNeedsAttention); err != nil {
		return false, err
	}
	return true, nil
}

func closeOutputV3Directory(directory outputcap.Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}

func closeOutputV3Lock(lock outputcap.Lock) error {
	if lock == nil {
		return nil
	}
	return lock.Close()
}
