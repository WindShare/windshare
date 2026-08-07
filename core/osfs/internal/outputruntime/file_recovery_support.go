package outputruntime

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

// stagedData is the writable stage capability. Its distinct type prevents a
// caller from accidentally using the mutable link as publication authority.
type stagedData struct {
	file outputcap.File
}

func (stage stagedData) valid() bool  { return stage.file != nil }
func (stage stagedData) Close() error { return closeOutputFile(stage.file) }
func (stage stagedData) WriteAt(data []byte, offset int64) (int, error) {
	return stage.file.WriteAt(data, offset)
}
func (stage stagedData) Sync() error { return stage.file.Sync() }
func (stage stagedData) SetModifiedTime(modified catalog.ModifiedTime) error {
	return stage.file.SetModifiedTime(modified)
}
func (stage stagedData) SameFile(anchor anchorWitness) (bool, error) {
	if !stage.valid() || !anchor.valid() {
		return false, outputcap.ErrUnsafeNamespace
	}
	return stage.file.SameFile(anchor.file)
}

// anchorWitness is the retained read-only, data-bearing link. Only this role
// can supply a source to the no-replace publication primitive.
type anchorWitness struct {
	file outputcap.File
}

func (anchor anchorWitness) valid() bool  { return anchor.file != nil }
func (anchor anchorWitness) Close() error { return closeOutputFile(anchor.file) }

// publicationWitness proves that the freshly reopened private stage and anchor
// names still identify one record-sized object. It is intentionally not
// constructible from either role alone.
type publicationWitness struct {
	stage  stagedData
	anchor anchorWitness
}

func (witness *publicationWitness) Close() error {
	if witness == nil {
		return nil
	}
	var result error
	if witness.stage.valid() {
		result = errors.Join(result, witness.stage.Close())
		witness.stage = stagedData{}
	}
	if witness.anchor.valid() {
		result = errors.Join(result, witness.anchor.Close())
		witness.anchor = anchorWitness{}
	}
	return result
}

func isMissing(err error) bool { return errors.Is(err, fs.ErrNotExist) }

type nativeRecoveryBoundary uint8

const (
	nativeBeforeEntryEvidence nativeRecoveryBoundary = iota + 1
	nativeExistingEntryUnclassified
	nativeAuthorizedMutation
)

type nativeRecoveryFailureDisposition uint8

const (
	nativeRecoveryPauseRequired nativeRecoveryFailureDisposition = iota + 1
	nativeRecoveryAmbiguous
)

// classifyNativeRecoveryFailure separates lack of operational authority from
// ambiguous namespace evidence. The former preserves the deterministic cut for
// retry; the latter must never turn a pathname into cleanup authority.
func classifyNativeRecoveryFailure(
	cause error,
	boundary nativeRecoveryBoundary,
) nativeRecoveryFailureDisposition {
	if cause == nil {
		return 0
	}
	if boundary == nativeExistingEntryUnclassified || errors.Is(cause, outputnamespace.ErrPositiveEntryEvidence) ||
		errors.Is(cause, outputcap.ErrUnsafeNamespace) || errors.Is(cause, outputcap.ErrNamespaceCollision) {
		return nativeRecoveryAmbiguous
	}
	return nativeRecoveryPauseRequired
}

func pauseRequiredFileOutputFault(cause error) error {
	return transfer.NewOutputSessionError(fileSettlementFailure(cause), true)
}

func pauseRequiredFileOperationFault(
	operation string,
	operationErr error,
	cleanupErr error,
) error {
	var result error
	if errors.Is(operationErr, errOutputAncestryUnsafe) {
		result = outputAncestryPauseFault(operation, operationErr)
	} else if operationErr != nil {
		result = pauseRequiredFileOutputFault(fileOutputFault(operation, operationErr))
	}
	if cleanupErr != nil {
		result = errors.Join(result, pauseRequiredFileOutputFault(outputfault.New(
			transfer.OutputFaultFile,
			transfer.OutputFaultStateIO,
			fmt.Errorf("clean up after %s: %w", operation, cleanupErr),
		)))
	}
	return result
}

type filesystemOutputFileSettlementTraceContext struct {
	boundary     FilesystemOutputFileSettlementBoundary
	pauseReason  transfer.FilePauseReason
	retireReason transfer.FileRetireReason
}

func (session *Session) traceReturnedFileStart(
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

func (session *Session) traceReturnedFileSettlement(
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
		IntentDigest:           session.intentDigest,
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
