package outputruntime

import (
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/directoryauthority"
	"github.com/windshare/windshare/osfs/internal/fileexecution"
	"github.com/windshare/windshare/osfs/internal/outputsession"
)

func (authority *Authority) traceSelectedOperationMismatch(
	record checkpointmodel.OrdinaryOperationRecord,
	intent transfer.ReceiveIntent,
) {
	authority.trace(Trace{
		Operation:            TraceRuntimeDecision,
		DestinationBindingID: authority.binding.ID(),
		ReceiveIntentDigest:  intent.Digest(),
		ReceiveOperationID:   record.OperationID(),
		RuntimeComponent:     RuntimeSession,
		RuntimeOperation:     RuntimeSelectOperation,
		RuntimeDecision:      RuntimeSelectionMismatch,
	})
}

func (authority *Authority) traceCheckpointReconciled(
	intent transfer.ReceiveIntent,
	sessionID transfer.OutputSessionID,
	recordCount, reclaimedCandidates uint64,
	attention []checkpointstore.Attention,
	reconciliationErr error,
) {
	if authority == nil || intent.IsZero() {
		return
	}
	event := Trace{
		Operation:                         TraceCheckpointReconciled,
		ReceiveIntentDigest:               intent.Digest(),
		ReceiveOperationID:                intent.OperationID(),
		SessionID:                         sessionID,
		RuntimeComponent:                  RuntimeCheckpoint,
		RuntimeOperation:                  RuntimeReconcileCheckpoints,
		RuntimeDecision:                   RuntimeReconciled,
		CheckpointRecordCount:             recordCount,
		CheckpointReclaimedCandidateCount: reclaimedCandidates,
	}
	event.CheckpointAttentionCount = uint64(len(attention))
	for _, item := range attention {
		event.CheckpointAttentionCode = string(item.Code())
		if item.BlocksRecovery() {
			break
		}
	}
	if reconciliationErr != nil {
		diagnostic := buildDiagnostic(FailureCheckpoint, reconciliationErr)
		event.RuntimeDecision = RuntimeRejected
		event.FailureStage = diagnostic.Stage
		event.ReconciliationStep = diagnostic.ReconciliationStep
		event.NativeErrorClass = diagnostic.NativeErrorClass
		event.FaultDomain = diagnostic.FaultDomain
		event.NormalizedFaultScope = diagnostic.NormalizedScope
		event.NormalizedFaultCode = diagnostic.NormalizedCode
		event.Failed = true
	} else if checkpointstore.RecoveryBlocked(attention) {
		// Repository attention proves neither a path nor a lineage. Report only
		// the closed aggregate decision so unsafe names never cross the trace boundary.
		event.RuntimeDecision = RuntimeNeedsAttention
		event.Failed = true
	}
	authority.trace(event)
}

func (authority *Authority) outputSessionRuntimeTrace() outputsession.TraceSink {
	return outputsession.TraceSinkFunc(func(event outputsession.TraceEvent) {
		decision := runtimeSessionDecision(event.Decision)
		if event.Operation == outputsession.OperationCommitFile && event.Decision == outputsession.TraceSettled {
			decision = RuntimeSucceeded
		}
		projected := Trace{
			Operation: TraceRuntimeDecision, ReceiveIntentDigest: event.ReceiveIntentDigest,
			ReceiveOperationID: event.ReceiveOperationID, SessionID: event.SessionID,
			RuntimeComponent: RuntimeSession,
			RuntimeOperation: runtimeSessionOperation(event.Operation),
			RuntimeDecision:  decision,
			OperationID:      event.OperationID, ClaimID: uint64(event.ClaimID),
			WriteOffset: event.WriteOffset, WriteLength: event.WriteLength,
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
		authority.trace(Trace{
			Operation: TraceRuntimeDecision, ReceiveIntentDigest: intent, SessionID: sessionID,
			RuntimeComponent: RuntimeDirectory,
			RuntimeOperation: runtimeDirectoryOperation(event.Operation),
			RuntimeDecision:  runtimeDirectoryDecision(event.Outcome),
			ClaimID:          uint64(event.ClaimID), Failed: event.Outcome == directoryauthority.TraceMutationAmbiguous,
		})
	}
}

func (authority *Authority) fileRuntimeTrace() fileexecution.TraceSink {
	return fileexecution.TraceSinkFunc(func(event fileexecution.TraceEvent) {
		projected := Trace{
			Operation: TraceRuntimeDecision, ReceiveIntentDigest: event.IntentDigest,
			ReceiveOperationID: event.OperationID, SessionID: event.SessionID, FileID: event.FileID,
			RuntimeComponent:   RuntimeFile,
			RuntimeOperation:   runtimeFileOperation(event.Operation),
			RuntimeDecision:    runtimeFileDecision(event.Outcome),
			CheckpointDecision: runtimeCheckpointDecision(event.Decision),
			OperationID:        event.Sequence,
			PreviousRevision:   event.PreviousRevision, CurrentRevision: event.CurrentRevision,
			RecoveryReason: event.RecoveryReason, ReusedBytes: event.ReusedBytes, RetainedBytes: event.RetainedBytes,
		}
		applyRuntimeFault(&projected, event.Fault)
		authority.trace(projected)
	})
}

func runtimeCheckpointDecision(decision checkpointmodel.CheckpointLineageDecision) CheckpointDecision {
	switch decision {
	case checkpointmodel.CheckpointLineageDecisionAbsent:
		return CheckpointAbsent
	case checkpointmodel.CheckpointLineageDecisionMetadataMatch:
		return CheckpointMetadataMatch
	case checkpointmodel.CheckpointLineageDecisionExact:
		return CheckpointExact
	case checkpointmodel.CheckpointLineageDecisionRevisionConflict:
		return CheckpointRevisionConflict
	case checkpointmodel.CheckpointLineageDecisionReuseStateBlocked:
		return CheckpointReuseStateBlocked
	case checkpointmodel.CheckpointLineageDecisionRevisionEvidenceUnavailable:
		return CheckpointRevisionEvidenceUnavailable
	case checkpointmodel.CheckpointLineageDecisionOwnershipConflict:
		return CheckpointOwnershipConflict
	case checkpointmodel.CheckpointLineageDecisionInvalid:
		return CheckpointInvalid
	default:
		return 0
	}
}

func applyRuntimeFault(event *Trace, value transferfault.Fault) {
	if event == nil || !value.Valid() {
		return
	}
	event.FaultDomain = uint8(value.Domain())
	event.NormalizedFaultScope = uint8(value.Scope())
	event.NormalizedFaultCode = value.Code()
	event.Failed = true
}

func runtimeSessionOperation(operation outputsession.OperationKind) RuntimeOperation {
	switch operation {
	case outputsession.OperationAdmitDirectory:
		return RuntimeAdmitDirectory
	case outputsession.OperationFinalizeDirectory:
		return RuntimeFinalizeDirectory
	case outputsession.OperationBeginFile:
		return RuntimeBeginFile
	case outputsession.OperationWriteRange:
		return RuntimeWriteRange
	case outputsession.OperationCheckpointFile:
		return RuntimeCheckpointFile
	case outputsession.OperationCommitFile:
		return RuntimeCommitFile
	case outputsession.OperationPauseFile:
		return RuntimePauseFile
	case outputsession.OperationRetireFile:
		return RuntimeRetireFile
	case outputsession.OperationPauseTree:
		return RuntimePauseTree
	case outputsession.OperationFinalizeTree:
		return RuntimeFinalizeTree
	case outputsession.OperationFirstWrite:
		return RuntimeFirstWrite
	default:
		return 0
	}
}

func runtimeSessionDecision(decision outputsession.TraceDecision) RuntimeDecision {
	switch decision {
	case outputsession.TraceReserved:
		return RuntimeReserved
	case outputsession.TraceCoalesced:
		return RuntimeCoalesced
	case outputsession.TraceRejected:
		return RuntimeRejected
	case outputsession.TraceRolledBack:
		return RuntimeRolledBack
	case outputsession.TraceAdmitted:
		return RuntimeAdmitted
	case outputsession.TraceActive:
		return RuntimeActive
	case outputsession.TraceSealed:
		return RuntimeSealed
	case outputsession.TraceSettled:
		return RuntimeSettled
	case outputsession.TraceAmbiguous:
		return RuntimeAmbiguous
	case outputsession.TraceDraining:
		return RuntimeDraining
	case outputsession.TraceClosed:
		return RuntimeClosed
	case outputsession.TraceCollision:
		return RuntimeCollision
	default:
		return 0
	}
}

func runtimeDirectoryOperation(operation directoryauthority.TraceOperation) RuntimeOperation {
	switch operation {
	case directoryauthority.TraceMaterializeDirectory:
		return RuntimeMaterializeDirectory
	case directoryauthority.TraceFinalizeDirectory:
		return RuntimeFinalizeDirectory
	default:
		return 0
	}
}

func runtimeDirectoryDecision(outcome directoryauthority.TraceOutcome) RuntimeDecision {
	switch outcome {
	case directoryauthority.TraceSucceeded:
		return RuntimeSucceeded
	case directoryauthority.TraceIsolatedFailure:
		return RuntimeIsolatedFailure
	case directoryauthority.TraceNoMutation:
		return RuntimeNoChange
	case directoryauthority.TraceMutationAmbiguous:
		return RuntimeAmbiguous
	default:
		return 0
	}
}

func runtimeFileOperation(operation fileexecution.TraceOperation) RuntimeOperation {
	switch operation {
	case fileexecution.TraceBeginFile:
		return RuntimeBeginFile
	case fileexecution.TraceCreateOwnedFile:
		return RuntimeCreateOwnedFile
	case fileexecution.TraceRecoverFile:
		return RuntimeRecoverFile
	case fileexecution.TraceWriteRange:
		return RuntimeWriteRange
	case fileexecution.TraceCheckpoint:
		return RuntimeCheckpointFile
	case fileexecution.TracePublish:
		return RuntimePublishFile
	case fileexecution.TracePause:
		return RuntimePauseFile
	case fileexecution.TraceRetire:
		return RuntimeRetireFile
	case fileexecution.TraceItemBlocked:
		return RuntimeQuarantineFile
	case fileexecution.TraceDeliveryReceipt:
		return RuntimeDeliveryReceipt
	default:
		return 0
	}
}

func runtimeFileDecision(outcome fileexecution.TraceOutcome) RuntimeDecision {
	switch outcome {
	case fileexecution.TraceSucceeded:
		return RuntimeSucceeded
	case fileexecution.TraceReconciled:
		return RuntimeReconciled
	case fileexecution.TraceCollision:
		return RuntimeCollision
	case fileexecution.TraceNoChange:
		return RuntimeNoChange
	case fileexecution.TraceNeedsAttention:
		return RuntimeNeedsAttention
	default:
		return 0
	}
}
