package osfs

import (
	"errors"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type filesystemOutputFileSettlementTraceContext struct {
	boundary     FilesystemOutputFileSettlementBoundary
	pauseReason  transfer.FilePauseReason
	retireReason transfer.FileRetireReason
}

func (session *filesystemOutputSession) traceReturnedFileStart(
	traceContext filesystemOutputFileSettlementTraceContext,
	start transfer.FileStart,
	resultErr error,
) {
	settlement, settled := start.ImmediateSettlement()
	if !settled {
		return
	}
	session.traceReturnedFileSettlement(traceContext, settlement, resultErr)
}

func (session *filesystemOutputSession) traceReturnedFileSettlement(
	traceContext filesystemOutputFileSettlementTraceContext,
	settlement transfer.FileSettlement,
	resultErr error,
) {
	if session == nil || settlement.Kind() < transfer.FilePublished || settlement.Kind() > transfer.FileQuarantined {
		return
	}
	target := settlement.Target()
	event := FilesystemOutputTrace{
		Operation:              TraceFileSettlement,
		ResumeIntent:           session.resumeIntent,
		SessionID:              target.OutputSessionID(),
		LocatorDigest:          target.Locator().Digest(),
		FileSettlement:         settlement.Kind(),
		FileSettlementBoundary: traceContext.boundary,
		FilePauseReason:        traceContext.pauseReason,
		FileRetireReason:       traceContext.retireReason,
		Failed:                 resultErr != nil,
	}
	if binding, bound := settlement.OutputBinding(); bound {
		event.OutputObjectID = binding.ObjectIdentity()
	}
	if _, reason, quarantined := settlement.Quarantine(); quarantined {
		event.QuarantineReason = reason
	}
	event.FailureScope, event.FailureCode = filesystemOutputTraceFailure(resultErr)
	session.owner.trace(event)
}

func filesystemOutputTraceFailure(err error) (transfer.OutputFaultScope, transfer.OutputFaultCode) {
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) {
		return 0, 0
	}
	return fault.Scope(), fault.Code()
}

func recoveryDecisionQuarantineReason(decision resumestate.RecoveryDecision) transfer.QuarantineReason {
	if decision.Action() != resumestate.RecoveryInstallQuarantine &&
		decision.Action() != resumestate.RecoveryHoldQuarantine || decision.QuarantineReason() == 0 {
		return 0
	}
	return mapQuarantineReason(decision.QuarantineReason())
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
	lock         outputV3Lock
	closeErr     error
	closed       bool
	owner        *FilesystemOutputAuthority
	traceContext filesystemOutputNativeLockContext
}

func (lock *filesystemOutputNativeLock) File() outputV3File {
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
		traceErr = outputFault(lock.traceContext.failureScope, transfer.OutputFaultStateIO, result)
	}
	lock.owner.traceNativeLock(lock.traceContext, milestone, traceErr)
	return result
}

func (authority *FilesystemOutputAuthority) acquireRuntimeNativeLock(
	acquire func() (outputV3Lock, bool, error),
	traceContext filesystemOutputNativeLockContext,
	unexpected error,
) (outputV3Lock, error) {
	raw, created, err := acquire()
	if err != nil {
		resultErr := classifyLockFailure(traceContext.failureScope, err)
		milestone := FilesystemOutputNativeLockAcquireFailed
		if errors.Is(err, errOutputV3LockBusy) {
			milestone = FilesystemOutputNativeLockContended
		}
		authority.traceNativeLock(traceContext, milestone, resultErr)
		return nil, resultErr
	}
	if raw == nil {
		resultErr := unexpected
		if resultErr == nil {
			resultErr = outputFault(traceContext.failureScope, transfer.OutputFaultContract, errOutputV3Unsafe)
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
		resultErr = outputFault(traceContext.failureScope, transfer.OutputFaultContract, errOutputV3Unsafe)
	}
	if closeErr := lock.Close(); closeErr != nil {
		resultErr = errors.Join(
			resultErr,
			outputFault(traceContext.failureScope, transfer.OutputFaultStateIO, closeErr),
		)
	}
	return nil, resultErr
}

func (authority *FilesystemOutputAuthority) traceNativeLock(
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
