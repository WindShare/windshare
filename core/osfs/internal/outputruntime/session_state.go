package outputruntime

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/transfer"
)

func (session *Session) poisonState() {
	if session == nil {
		return
	}
	session.mu.Lock()
	session.poisoned = true
	session.settling = true
	session.mu.Unlock()
}

func (session *Session) teardownPoisoned() {
	_ = session.shutdownOwner()
}

func (session *Session) shutdownOwner() error {
	if session == nil {
		return nil
	}
	session.operationGate.Lock()
	defer session.operationGate.Unlock()
	return session.shutdownOwnerLocked()
}

// shutdownOwnerLocked releases only live capabilities. FileCheckpointV1 is the
// durable settlement boundary, so shutdown never manufactures a session header
// or secondary durable state while unwinding an uncertain owner.
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
		digest := transaction.resumable.BoundState().State().LocatorDigest()
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
	session.teardownPoisonedAfterOperation()
}

func (session *Session) teardownPoisonedAfterOperation() {
	session.mu.Lock()
	shouldTeardown := session.exposed && session.poisoned
	session.mu.Unlock()
	if shouldTeardown {
		session.poisonOnce.Do(session.teardownPoisoned)
	}
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

func sessionSettlementFailure(cause error) error {
	if _, found := errors.AsType[*transfer.OutputFault](cause); found {
		return cause
	}
	return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, cause)
}

func filePauseReasonForJob(reason transfer.JobPauseReason) transfer.FilePauseReason {
	switch reason {
	case transfer.JobPauseInterrupted:
		return transfer.FilePauseInterrupted
	case transfer.JobPauseShutdown:
		return transfer.FilePauseShutdown
	case transfer.JobPauseTransportFailure:
		return transfer.FilePauseTransportFailure
	case transfer.JobPauseSessionFailure:
		return transfer.FilePauseSessionFailure
	default:
		return transfer.FilePauseOutputFailure
	}
}

func (session *Session) closeHandles() error {
	if session == nil {
		return nil
	}
	session.mu.Lock()
	sessionLock := session.sessionLock
	anchorsDir, stagesDir := session.anchorsDir, session.stagesDir
	checkpointsDir, sessionDir := session.checkpointsDir, session.sessionDir
	platform := session.platform
	session.sessionLock = nil
	session.anchorsDir, session.stagesDir = nil, nil
	session.checkpointsDir, session.sessionDir = nil, nil
	session.platform = nil
	session.mu.Unlock()
	var platformErr error
	if platform != nil {
		platformErr = platform.Close()
	}
	return errors.Join(
		closeOutputLock(sessionLock),
		closeOutputDirectory(anchorsDir),
		closeOutputDirectory(stagesDir),
		closeOutputDirectory(checkpointsDir),
		closeOutputDirectory(sessionDir),
		platformErr,
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

func (session *Session) PauseJob(
	ctx context.Context,
	reason transfer.JobPauseReason,
) (transfer.JobSettlement, error) {
	if session == nil || reason < transfer.JobPauseInterrupted || reason > transfer.JobPauseDependencyContract {
		return transfer.JobSettlement{}, outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	if err := session.beginSettlement(); err != nil {
		return transfer.JobSettlement{}, err
	}
	defer session.endSettlement()
	session.beginWG.Wait()
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
	settlementKind := transfer.JobPaused
	if attention || settleErr != nil {
		settlementKind = transfer.JobPausedNeedsAttention
	}
	closeErr := session.shutdownOwnerLocked()
	if err := errors.Join(settleErr, closeErr); err != nil {
		return transfer.JobSettlement{}, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	settlement, err := transfer.NewJobSettlement(settlementKind)
	if err != nil {
		return transfer.JobSettlement{}, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
	}
	session.owner.trace(FilesystemOutputTrace{
		Operation: TraceSessionSettlement, IntentDigest: session.intentDigest,
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
