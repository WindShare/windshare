package outputruntime

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func (authority *Authority) revalidateOutputAdmissionAncestry(
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

func (authority *Authority) stateStore(
	intent transfer.ResumeIntent,
	sessionID transfer.OutputSessionID,
) outputnamespace.Store {
	return authority.namespaceController().Store(intent, sessionID)
}

func (authority *Authority) namespaceController() outputnamespace.Controller {
	config := outputnamespace.ControllerConfig{Backend: filesystemOutputBackendID}
	if authority == nil {
		return outputnamespace.NewController(config)
	}
	config.Random, config.SessionIDs = authority.random, authority.sessionIDs
	config.Observer = outputnamespace.ObserverFunc(func(event outputnamespace.StateInstallEvent) {
		authority.traceAdoptedStateInstallCut(event.ResumeIntent, event.SessionID, event.Cut)
	})
	return outputnamespace.NewController(config)
}

func (authority *Authority) traceAdoptedStateInstallCut(
	intent transfer.ResumeIntent,
	sessionID transfer.OutputSessionID,
	cut outputnamespace.StateInstallCut,
) {
	if authority == nil {
		return
	}
	encoded := cut.Encoded()
	targetName := cut.TargetName()
	generation, err := outputnamespace.DecodeRecordGeneration(targetName, encoded)
	if err != nil {
		return
	}
	var locator resumestate.LocatorDigest
	if targetName != resumestate.ControlRecordName &&
		targetName != resumestate.HeaderRecordName &&
		len(targetName) >= resumestate.ShardHexCharacters {
		locator, err = resumestate.ParseFileRecordName(
			targetName[:resumestate.ShardHexCharacters], targetName,
		)
		if err != nil {
			return
		}
	}
	authority.trace(FilesystemOutputTrace{
		Operation: TraceStateInstallCutAdopted, ResumeIntent: intent,
		SessionID: sessionID, LocatorDigest: outputLocatorDigestFromState(locator), StateGeneration: generation,
		StateInstallStage:         filesystemOutputStateInstallStage(cut.Stage()),
		MutationReportedFailure:   cut.MutationReportedFailure(),
		ParentSyncReportedFailure: cut.ParentSyncReportedFailure(),
	})
}

func filesystemOutputStateInstallStage(stage outputnamespace.StateInstallStage) FilesystemOutputStateInstallStage {
	switch stage {
	case outputnamespace.StateInstallCreate:
		return FilesystemOutputStateCreate
	case outputnamespace.StateInstallReplace:
		return FilesystemOutputStateReplace
	default:
		return 0
	}
}

type outputSessionChildren struct {
	files   outputcap.Directory
	anchors outputcap.Directory
	stages  outputcap.Directory
}

func closeSessionChildren(children outputSessionChildren) error {
	return errors.Join(children.files.Close(), children.anchors.Close(), children.stages.Close())
}

func bindOutputSessionState(
	control resumestate.Control,
	sessionDirectory outputcap.Directory,
	selection transfer.OutputSelection,
	ancestry resumestate.OutputAncestryBinding,
	intentName string,
	sessionName string,
) (resumestate.SessionAuthority, error) {
	headerBytes, err := outputnamespace.ReadRecord(
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
			outputfault.ErrIntentUnsafe,
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

func (authority *Authority) bindOutputSessionStateWithAncestryTrace(
	control resumestate.Control,
	sessionDirectory outputcap.Directory,
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
	if mismatch, found := errors.AsType[*outputAncestryHeaderMismatch](err); found {
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
	sessionsDirectory outputcap.Directory,
	intentDirectory outputcap.Directory,
	sessionDirectory outputcap.Directory,
	intentName string,
	sessionName string,
) error {
	if err := outputnamespace.VerifyPinnedDirectoryEntry(sessionsDirectory, intentName, intentDirectory); err != nil {
		return err
	}
	return outputnamespace.VerifyPinnedDirectoryEntry(intentDirectory, sessionName, sessionDirectory)
}

func validateSessionChildren(session outputcap.Directory) (outputSessionChildren, error) {
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
		return outputSessionChildren{}, outputfault.ErrIntentUnsafe
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

func (session *Session) resumeLifecycle() error {
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
				outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
					fmt.Errorf("%w: terminal session transition needs recovery", outputfault.ErrIntentUnsafe)),
				true,
			)
		default:
			return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateCorrupt, outputfault.ErrIntentUnsafe)
		}
	}
}

func (session *Session) stateSnapshot() resumestate.SessionAuthority {
	if session == nil {
		return resumestate.SessionAuthority{}
	}
	session.stateInstall.RLock()
	defer session.stateInstall.RUnlock()
	return session.state
}

func (session *Session) installLifecycle(next resumestate.SessionLifecycle) error {
	session.stateInstall.Lock()
	defer session.stateInstall.Unlock()
	if session.stateWritesDisabled() {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed)
	}
	updated, err := session.state.WithLifecycle(next)
	if err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
	}
	currentEncoded, err := resumestate.EncodeHeader(session.state.Header())
	if err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
	}
	nextEncoded, err := resumestate.EncodeHeader(updated.Header())
	if err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
	}
	outcome, replaceErr := session.store.ReplaceRecord(
		session.sessionDir,
		resumestate.HeaderRecordName,
		outputnamespace.NewRecordImage(currentEncoded, session.state.Header().StateGeneration()),
		outputnamespace.NewRecordImage(nextEncoded, updated.Header().StateGeneration()),
		resumestate.MaxSessionHeaderBytes,
	)
	switch outcome {
	case outputnamespace.ReplaceAdopted:
		session.state = updated
		if replaceErr != nil {
			// A verified next generation is the only state this owner may retain,
			// but a failed cleanup means this process cannot safely continue to
			// mutate the namespace. A fresh opener must recover from the adopted
			// generation after the current owner releases its handles.
			session.poisonState()
			return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, replaceErr)
		}
		return nil
	case outputnamespace.ReplaceUnchanged:
		if replaceErr == nil {
			replaceErr = outputcap.ErrUnsafeNamespace
		}
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, replaceErr)
	case outputnamespace.ReplaceUncertain:
		session.poisonState()
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO,
			errors.Join(outputcap.ErrUnsafeNamespace, replaceErr))
	default:
		session.poisonState()
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultContract, resumestate.ErrInvalidState)
	}
}

func (session *Session) poisonState() {
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

func (session *Session) teardownPoisoned() {
	_ = session.shutdownOwner()
}

// shutdownOwner is the boundary for callers that do not already own the
// exclusive operation gate. Closing outside this boundary could race an
// ancestry revalidation that still relies on the platform handles.
func (session *Session) shutdownOwner() error {
	if session == nil {
		return nil
	}
	session.operationGate.Lock()
	defer session.operationGate.Unlock()
	return session.shutdownOwnerLocked()
}

// shutdownOwnerLocked deliberately performs no state mutation. Once durable
// authority is uncertain, only a fresh opener may interpret the namespace; the
// current owner is limited to draining in-memory work and releasing its handles.
func (session *Session) shutdownOwnerLocked() error {
	session.beginWG.Wait()
	session.mu.Lock()
	transactions := make([]*FileTransaction, 0, len(session.active))
	for _, transaction := range session.active {
		transactions = append(transactions, transaction)
	}
	session.settling = true
	session.closed = true
	session.mu.Unlock()
	var closeErr error
	for _, transaction := range transactions {
		transaction.mu.Lock()
		transaction.lifecycle = FileTransactionClosed
		closeErr = errors.Join(closeErr, transaction.closeHandlesLocked())
		digest := transaction.resumable.Bound().Record().LocatorDigest()
		transaction.mu.Unlock()
		session.finishFile(digest, transaction)
	}
	return errors.Join(closeErr, session.closeHandles())
}

func (session *Session) beginOperation() error {
	if session == nil {
		return transfer.ErrInvalidOutputBinding
	}
	session.operationGate.RLock()
	if session.operationDisabled() {
		session.operationGate.RUnlock()
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed)
	}
	return nil
}

func (session *Session) endOperation() {
	session.operationGate.RUnlock()
}

func (session *Session) operationDisabled() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.closed || session.settling || session.poisoned
}

func (session *Session) stateWritesDisabled() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.closed || session.poisoned
}

func (session *Session) BackendID() transfer.OutputBackendID {
	return filesystemOutputBackendID
}

func (session *Session) SessionID() transfer.OutputSessionID {
	if session == nil {
		return transfer.OutputSessionID{}
	}
	return session.sessionID
}

func (session *Session) Capabilities() transfer.OutputCapabilities {
	if session == nil {
		return transfer.OutputCapabilities{}
	}
	return session.capabilities
}

func classifyLockFailure(scope transfer.OutputFaultScope, err error) error {
	if errors.Is(err, outputcap.ErrNamespaceLockBusy) {
		return outputfault.New(scope, transfer.OutputFaultOwnership, errors.Join(outputfault.ErrSessionActive, err))
	}
	return outputfault.New(scope, transfer.OutputFaultStateIO, err)
}

func intentOutputFault(operation string, cause error) error {
	return transfer.NewOutputSessionError(
		outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
			errors.Join(outputfault.ErrIntentUnsafe, fmt.Errorf("%s: %w", operation, cause))),
		true,
	)
}

func (session *Session) closeHandles() error {
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

var _ transfer.OutputSession = (*Session)(nil)

func (session *Session) PauseJob(
	ctx context.Context,
	reason transfer.JobPauseReason,
) (transfer.JobSettlement, error) {
	if session == nil || reason < transfer.JobPauseInterrupted || reason > transfer.JobPauseOutputFailure {
		return transfer.JobSettlement{}, outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	if err := session.beginSettlement(); err != nil {
		return transfer.JobSettlement{}, err
	}
	defer session.endSettlement()
	session.beginWG.Wait()
	if err := session.installLifecycle(resumestate.SessionPausing); err != nil {
		return transfer.JobSettlement{}, session.failOwnerSettlement(err)
	}
	session.mu.Lock()
	transactions := make([]*FileTransaction, 0, len(session.active))
	for _, transaction := range session.active {
		transactions = append(transactions, transaction)
	}
	attention := len(session.attention) != 0
	session.mu.Unlock()
	fileReason := filePauseReasonForJob(reason)
	var settleErr error
	for _, transaction := range transactions {
		settlement, err := transaction.pauseForSessionSettlement(ctx, fileReason)
		if err != nil {
			settleErr = errors.Join(settleErr, err)
			attention = true
			continue
		}
		if settlement.Kind() == transfer.FileQuarantined {
			attention = true
		}
	}

	next := resumestate.SessionPaused
	settlementKind := transfer.JobPaused
	if attention || settleErr != nil {
		next = resumestate.SessionPausedNeedsAttention
		settlementKind = transfer.JobPausedNeedsAttention
	}
	stateErr := session.installLifecycle(next)
	closeErr := session.shutdownOwnerLocked()
	if err := errors.Join(settleErr, stateErr, closeErr); err != nil {
		return transfer.JobSettlement{}, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	settlement, err := transfer.NewJobSettlement(settlementKind)
	if err != nil {
		return transfer.JobSettlement{}, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
	}
	session.owner.trace(FilesystemOutputTrace{
		Operation: TraceSessionSettlement, ResumeIntent: session.resumeIntent,
		SessionID: session.SessionID(), JobSettlement: settlementKind,
	})
	return settlement, nil
}

func (session *Session) beginSettlement() error {
	if session == nil {
		return outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultContract, transfer.ErrInvalidOutputBinding,
		)
	}
	session.operationGate.Lock()
	session.mu.Lock()
	if session.closed || session.settling || session.poisoned {
		session.mu.Unlock()
		session.operationGate.Unlock()
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed)
	}
	session.settling = true
	session.mu.Unlock()
	return nil
}

func (session *Session) endSettlement() {
	session.operationGate.Unlock()
}

func (session *Session) failOwnerSettlement(cause error) error {
	cause = sessionSettlementFailure(cause)
	closeErr := session.shutdownOwnerLocked()
	if closeErr == nil {
		return cause
	}
	return errors.Join(
		cause,
		outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, closeErr),
	)
}
