package outputruntime

import (
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func (authority *Authority) traceCheckpointReconciled(
	intent transfer.ReceiveIntent,
	sessionID transfer.OutputSessionID,
	recordCount uint64,
	repositoryAttention bool,
	reconciliationErr error,
) {
	if authority == nil || intent.IsZero() {
		return
	}
	event := FilesystemOutputTrace{
		Operation:             TraceCheckpointReconciled,
		ReceiveIntentDigest:   intent.Digest(),
		ReceiveOperationID:    intent.OperationID(),
		SessionID:             sessionID,
		RuntimeComponent:      FilesystemOutputRuntimeCheckpoint,
		RuntimeOperation:      FilesystemOutputRuntimeReconcileCheckpoints,
		RuntimeDecision:       FilesystemOutputRuntimeReconciled,
		CheckpointRecordCount: recordCount,
	}
	if reconciliationErr != nil {
		diagnostic := filesystemOutputDiagnostic(
			FilesystemOutputFailureCheckpointReconciliation,
			reconciliationErr,
		)
		event.RuntimeDecision = FilesystemOutputRuntimeRejected
		event.FailureStage = diagnostic.Stage
		event.ReconciliationStep = diagnostic.ReconciliationStep
		event.NativeErrorClass = diagnostic.NativeErrorClass
		event.FaultDomain = diagnostic.FaultDomain
		event.NormalizedFaultScope = diagnostic.NormalizedScope
		event.NormalizedFaultCode = diagnostic.NormalizedCode
		event.Failed = true
	} else if repositoryAttention {
		// Repository attention proves neither a path nor a lineage. Report only
		// the closed aggregate decision so unsafe names never cross the trace boundary.
		event.RuntimeDecision = FilesystemOutputRuntimeNeedsAttention
		event.Failed = true
	}
	authority.trace(event)
}

func (authority *Authority) outputSessionRuntimeTrace() outputsession.TraceSink {
	return outputsession.TraceSinkFunc(func(event outputsession.TraceEvent) {
		decision := runtimeSessionDecision(event.Decision)
		if event.Operation == outputsession.OperationCommitFile && event.Decision == outputsession.TraceSettled {
			decision = FilesystemOutputRuntimeSucceeded
		}
		projected := FilesystemOutputTrace{
			Operation: TraceRuntimeDecision, ReceiveIntentDigest: event.ReceiveIntentDigest,
			ReceiveOperationID: event.ReceiveOperationID, SessionID: event.SessionID,
			RuntimeComponent: FilesystemOutputRuntimeSession,
			RuntimeOperation: runtimeSessionOperation(event.Operation),
			RuntimeDecision:  decision,
			OperationID:      event.OperationID, ClaimID: uint64(event.ClaimID),
			NodeClaimCount: event.NodeClaims, DirectoryClaimCount: event.DirectoryClaims,
			FileClaimCount: event.FileClaims, ActiveFileClaimCount: event.ActiveFileClaims,
			ReservedFileSlotCount:  event.ReservedFileSlots,
			DirectoryMetadataBytes: event.DirectoryMetadataBytes,
		}
		applyRuntimeFault(&projected, event.Fault)
		authority.trace(projected)
	})
}

func (authority *Authority) directoryRuntimeTrace(
	intent transfer.ReceiveIntentDigest,
	sessionID transfer.OutputSessionID,
) func(directoryauthority.TraceEvent) {
	return func(event directoryauthority.TraceEvent) {
		authority.trace(FilesystemOutputTrace{
			Operation: TraceRuntimeDecision, ReceiveIntentDigest: intent, SessionID: sessionID,
			RuntimeComponent: FilesystemOutputRuntimeDirectory,
			RuntimeOperation: runtimeDirectoryOperation(event.Operation),
			RuntimeDecision:  runtimeDirectoryDecision(event.Outcome),
			ClaimID:          uint64(event.ClaimID), Failed: event.Outcome == directoryauthority.TraceMutationAmbiguous,
		})
	}
}

func (authority *Authority) fileRuntimeTrace() fileexecution.TraceSink {
	return fileexecution.TraceSinkFunc(func(event fileexecution.TraceEvent) {
		projected := FilesystemOutputTrace{
			Operation: TraceRuntimeDecision, ReceiveIntentDigest: event.IntentDigest,
			ReceiveOperationID: event.OperationID, SessionID: event.SessionID,
			RuntimeComponent:   FilesystemOutputRuntimeFile,
			RuntimeOperation:   runtimeFileOperation(event.Operation),
			RuntimeDecision:    runtimeFileDecision(event.Outcome),
			CheckpointDecision: runtimeCheckpointDecision(event.Decision),
			OperationID:        event.Sequence,
		}
		applyRuntimeFault(&projected, event.Fault)
		authority.trace(projected)
	})
}

func runtimeCheckpointDecision(
	decision checkpointmodel.CheckpointLineageDecision,
) FilesystemCheckpointDecision {
	switch decision {
	case checkpointmodel.CheckpointLineageDecisionAbsent:
		return FilesystemCheckpointAbsent
	case checkpointmodel.CheckpointLineageDecisionExact:
		return FilesystemCheckpointExact
	case checkpointmodel.CheckpointLineageDecisionRevisionConflict:
		return FilesystemCheckpointRevisionConflict
	case checkpointmodel.CheckpointLineageDecisionOwnershipConflict:
		return FilesystemCheckpointOwnershipConflict
	case checkpointmodel.CheckpointLineageDecisionInvalid:
		return FilesystemCheckpointInvalid
	default:
		return 0
	}
}

func applyRuntimeFault(event *FilesystemOutputTrace, value transferfault.Fault) {
	if event == nil || !value.Valid() {
		return
	}
	event.FaultDomain = uint8(value.Domain())
	event.NormalizedFaultScope = uint8(value.Scope())
	event.NormalizedFaultCode = value.Code()
	event.Failed = true
}

func runtimeSessionOperation(operation outputsession.OperationKind) FilesystemOutputRuntimeOperation {
	switch operation {
	case outputsession.OperationAdmitDirectory:
		return FilesystemOutputRuntimeAdmitDirectory
	case outputsession.OperationFinalizeDirectory:
		return FilesystemOutputRuntimeFinalizeDirectory
	case outputsession.OperationBeginFile:
		return FilesystemOutputRuntimeBeginFile
	case outputsession.OperationWriteRange:
		return FilesystemOutputRuntimeWriteRange
	case outputsession.OperationCheckpointFile:
		return FilesystemOutputRuntimeCheckpointFile
	case outputsession.OperationCommitFile:
		return FilesystemOutputRuntimeCommitFile
	case outputsession.OperationPauseFile:
		return FilesystemOutputRuntimePauseFile
	case outputsession.OperationRetireFile:
		return FilesystemOutputRuntimeRetireFile
	case outputsession.OperationPauseTree:
		return FilesystemOutputRuntimePauseTree
	case outputsession.OperationFinalizeTree:
		return FilesystemOutputRuntimeFinalizeTree
	case outputsession.OperationFirstWrite:
		return FilesystemOutputRuntimeFirstWrite
	default:
		return 0
	}
}

func runtimeSessionDecision(decision outputsession.TraceDecision) FilesystemOutputRuntimeDecision {
	switch decision {
	case outputsession.TraceReserved:
		return FilesystemOutputRuntimeReserved
	case outputsession.TraceCoalesced:
		return FilesystemOutputRuntimeCoalesced
	case outputsession.TraceRejected:
		return FilesystemOutputRuntimeRejected
	case outputsession.TraceRolledBack:
		return FilesystemOutputRuntimeRolledBack
	case outputsession.TraceAdmitted:
		return FilesystemOutputRuntimeAdmitted
	case outputsession.TraceActive:
		return FilesystemOutputRuntimeActive
	case outputsession.TraceSealed:
		return FilesystemOutputRuntimeSealed
	case outputsession.TraceSettled:
		return FilesystemOutputRuntimeSettled
	case outputsession.TraceAmbiguous:
		return FilesystemOutputRuntimeAmbiguous
	case outputsession.TraceDraining:
		return FilesystemOutputRuntimeDraining
	case outputsession.TraceClosed:
		return FilesystemOutputRuntimeClosed
	case outputsession.TraceCollision:
		return FilesystemOutputRuntimeCollision
	default:
		return 0
	}
}

func runtimeDirectoryOperation(operation directoryauthority.TraceOperation) FilesystemOutputRuntimeOperation {
	switch operation {
	case directoryauthority.TraceMaterializeDirectory:
		return FilesystemOutputRuntimeMaterializeDirectory
	case directoryauthority.TraceFinalizeDirectory:
		return FilesystemOutputRuntimeFinalizeDirectory
	default:
		return 0
	}
}

func runtimeDirectoryDecision(outcome directoryauthority.TraceOutcome) FilesystemOutputRuntimeDecision {
	switch outcome {
	case directoryauthority.TraceSucceeded:
		return FilesystemOutputRuntimeSucceeded
	case directoryauthority.TraceIsolatedFailure:
		return FilesystemOutputRuntimeIsolatedFailure
	case directoryauthority.TraceNoMutation:
		return FilesystemOutputRuntimeNoChange
	case directoryauthority.TraceMutationAmbiguous:
		return FilesystemOutputRuntimeAmbiguous
	default:
		return 0
	}
}

func runtimeFileOperation(operation fileexecution.TraceOperation) FilesystemOutputRuntimeOperation {
	switch operation {
	case fileexecution.TraceBeginFile:
		return FilesystemOutputRuntimeBeginFile
	case fileexecution.TraceCreateOwnedFile:
		return FilesystemOutputRuntimeCreateOwnedFile
	case fileexecution.TraceRecoverFile:
		return FilesystemOutputRuntimeRecoverFile
	case fileexecution.TraceWriteRange:
		return FilesystemOutputRuntimeWriteRange
	case fileexecution.TraceCheckpoint:
		return FilesystemOutputRuntimeCheckpointFile
	case fileexecution.TracePublish:
		return FilesystemOutputRuntimePublishFile
	case fileexecution.TracePause:
		return FilesystemOutputRuntimePauseFile
	case fileexecution.TraceRetire:
		return FilesystemOutputRuntimeRetireFile
	case fileexecution.TraceItemBlocked:
		return FilesystemOutputRuntimeQuarantineFile
	default:
		return 0
	}
}

func runtimeFileDecision(outcome fileexecution.TraceOutcome) FilesystemOutputRuntimeDecision {
	switch outcome {
	case fileexecution.TraceSucceeded:
		return FilesystemOutputRuntimeSucceeded
	case fileexecution.TraceReconciled:
		return FilesystemOutputRuntimeReconciled
	case fileexecution.TraceCollision:
		return FilesystemOutputRuntimeCollision
	case fileexecution.TraceNoChange:
		return FilesystemOutputRuntimeNoChange
	case fileexecution.TraceNeedsAttention:
		return FilesystemOutputRuntimeNeedsAttention
	default:
		return 0
	}
}
