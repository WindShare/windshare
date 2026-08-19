package osfs

import "github.com/windshare/windshare/core/osfs/internal/outputruntime"

// outputRuntimeTracer is the single semantic projection from internal runtime
// observability to the public event vocabulary. Explicit mappings prevent new
// internal decisions from silently becoming public API.
type outputRuntimeTracer struct {
	target FilesystemOutputTracer
}

func (tracer outputRuntimeTracer) TraceFilesystemOutput(event outputruntime.FilesystemOutputTrace) {
	if tracer.target != nil {
		tracer.target.TraceFilesystemOutput(projectFilesystemOutputTrace(event))
	}
}

func projectFilesystemOutputTrace(event outputruntime.FilesystemOutputTrace) FilesystemOutputTrace {
	return FilesystemOutputTrace{
		Operation:              projectTraceOperation(event.Operation),
		ReceiveIntentDigest:    event.ReceiveIntentDigest,
		ReceiveOperationID:     event.ReceiveOperationID,
		SessionID:              event.SessionID,
		Certification:          projectCertification(event.Certification),
		NativeLockScope:        projectNativeLockScope(event.NativeLockScope),
		NativeLockMilestone:    projectNativeLockMilestone(event.NativeLockMilestone),
		RootOpenDisposition:    projectRootOpenDisposition(event.RootOpenDisposition),
		RuntimeComponent:       projectRuntimeComponent(event.RuntimeComponent),
		RuntimeOperation:       projectRuntimeOperation(event.RuntimeOperation),
		RuntimeDecision:        projectRuntimeDecision(event.RuntimeDecision),
		CheckpointDecision:     projectCheckpointDecision(event.CheckpointDecision),
		OperationID:            event.OperationID,
		ClaimID:                event.ClaimID,
		FaultDomain:            event.FaultDomain,
		NormalizedFaultScope:   event.NormalizedFaultScope,
		NormalizedFaultCode:    event.NormalizedFaultCode,
		NodeClaimCount:         event.NodeClaimCount,
		DirectoryClaimCount:    event.DirectoryClaimCount,
		FileClaimCount:         event.FileClaimCount,
		ActiveFileClaimCount:   event.ActiveFileClaimCount,
		ReservedFileSlotCount:  event.ReservedFileSlotCount,
		DirectoryMetadataBytes: event.DirectoryMetadataBytes,
		CheckpointRecordCount:  event.CheckpointRecordCount,
		FailureStage:           projectFilesystemFailureStage(event.FailureStage),
		ReconciliationStep:     projectFilesystemReconciliationStep(event.ReconciliationStep),
		NativeErrorClass:       projectFilesystemNativeErrorClass(event.NativeErrorClass),
		Failed:                 event.Failed,
	}
}

func projectCheckpointDecision(
	value outputruntime.FilesystemCheckpointDecision,
) FilesystemCheckpointDecision {
	switch value {
	case outputruntime.FilesystemCheckpointAbsent:
		return FilesystemCheckpointAbsent
	case outputruntime.FilesystemCheckpointExact:
		return FilesystemCheckpointExact
	case outputruntime.FilesystemCheckpointRevisionConflict:
		return FilesystemCheckpointRevisionConflict
	case outputruntime.FilesystemCheckpointOwnershipConflict:
		return FilesystemCheckpointOwnershipConflict
	case outputruntime.FilesystemCheckpointInvalid:
		return FilesystemCheckpointInvalid
	default:
		return 0
	}
}

func projectFilesystemOutputDiagnostic(
	value outputruntime.FilesystemOutputDiagnostic,
) FilesystemOutputDiagnostic {
	return FilesystemOutputDiagnostic{
		Stage:              projectFilesystemFailureStage(value.Stage),
		ReconciliationStep: projectFilesystemReconciliationStep(value.ReconciliationStep),
		NativeErrorClass:   projectFilesystemNativeErrorClass(value.NativeErrorClass),
		FaultDomain:        value.FaultDomain,
		NormalizedScope:    value.NormalizedScope,
		NormalizedCode:     value.NormalizedCode,
	}
}

func projectFilesystemFailureStage(
	value outputruntime.FilesystemOutputFailureStage,
) FilesystemOutputFailureStage {
	switch value {
	case outputruntime.FilesystemOutputFailureDestinationBinding:
		return FilesystemOutputFailureDestinationBinding
	case outputruntime.FilesystemOutputFailureInventoryPaging:
		return FilesystemOutputFailureInventoryPaging
	case outputruntime.FilesystemOutputFailureActiveLookup:
		return FilesystemOutputFailureActiveLookup
	case outputruntime.FilesystemOutputFailureOperationAcquisition:
		return FilesystemOutputFailureOperationAcquisition
	case outputruntime.FilesystemOutputFailureOperationAdmission:
		return FilesystemOutputFailureOperationAdmission
	case outputruntime.FilesystemOutputFailureCheckpointReconciliation:
		return FilesystemOutputFailureCheckpointReconciliation
	case outputruntime.FilesystemOutputFailureNativeDurability:
		return FilesystemOutputFailureNativeDurability
	case outputruntime.FilesystemOutputFailureAuthorityClose:
		return FilesystemOutputFailureAuthorityClose
	default:
		return 0
	}
}

func projectFilesystemReconciliationStep(
	value outputruntime.FilesystemCheckpointReconciliationStep,
) FilesystemCheckpointReconciliationStep {
	switch value {
	case outputruntime.FilesystemCheckpointCandidateObservation:
		return FilesystemCheckpointCandidateObservation
	case outputruntime.FilesystemCheckpointStageDurability:
		return FilesystemCheckpointStageDurability
	case outputruntime.FilesystemCheckpointNamespaceDurability:
		return FilesystemCheckpointNamespaceDurability
	case outputruntime.FilesystemCheckpointRecordPromotion:
		return FilesystemCheckpointRecordPromotion
	default:
		return 0
	}
}

func projectFilesystemNativeErrorClass(
	value outputruntime.FilesystemNativeErrorClass,
) FilesystemNativeErrorClass {
	switch value {
	case outputruntime.FilesystemNativeErrorAccessDenied:
		return FilesystemNativeErrorAccessDenied
	case outputruntime.FilesystemNativeErrorSharingViolation:
		return FilesystemNativeErrorSharingViolation
	case outputruntime.FilesystemNativeErrorNotFound:
		return FilesystemNativeErrorNotFound
	case outputruntime.FilesystemNativeErrorInvalidHandle:
		return FilesystemNativeErrorInvalidHandle
	case outputruntime.FilesystemNativeErrorUnsupported:
		return FilesystemNativeErrorUnsupported
	case outputruntime.FilesystemNativeErrorIO:
		return FilesystemNativeErrorIO
	case outputruntime.FilesystemNativeErrorUnknown:
		return FilesystemNativeErrorUnknown
	default:
		return 0
	}
}

func projectFilesystemOutputStateReason(
	value outputruntime.FilesystemOutputStateReason,
) FilesystemOutputStateReason {
	switch value {
	case outputruntime.FilesystemOutputStateReasonNone:
		return FilesystemOutputStateReasonNone
	case outputruntime.FilesystemOutputStateDestinationOwnershipUnknown:
		return FilesystemOutputStateDestinationOwnershipUnknown
	case outputruntime.FilesystemOutputStateRegistryOwnershipUnknown:
		return FilesystemOutputStateRegistryOwnershipUnknown
	case outputruntime.FilesystemOutputStateLeaseOwnershipUnknown:
		return FilesystemOutputStateLeaseOwnershipUnknown
	case outputruntime.FilesystemOutputStateOperationOwnershipUnknown:
		return FilesystemOutputStateOperationOwnershipUnknown
	case outputruntime.FilesystemOutputStateCleanupUncertain:
		return FilesystemOutputStateCleanupUncertain
	default:
		return 0
	}
}

func projectTraceOperation(value outputruntime.FilesystemOutputTraceOperation) FilesystemOutputTraceOperation {
	switch value {
	case outputruntime.TraceFilesystemCertified:
		return TraceFilesystemCertified
	case outputruntime.TraceFeatureProbeCompleted:
		return TraceFeatureProbeCompleted
	case outputruntime.TraceCheckpointNamespaceOpened:
		return TraceCheckpointNamespaceOpened
	case outputruntime.TraceNativeLock:
		return TraceNativeLock
	case outputruntime.TraceSessionOpened:
		return TraceSessionOpened
	case outputruntime.TraceCheckpointReconciled:
		return TraceCheckpointReconciled
	case outputruntime.TraceRuntimeDecision:
		return TraceRuntimeDecision
	default:
		return 0
	}
}

func projectRootOpenDisposition(
	value outputruntime.FilesystemOutputRootDisposition,
) FilesystemOutputRootDisposition {
	switch value {
	case outputruntime.FilesystemOutputCallerProvidedContainer:
		return FilesystemOutputCallerProvidedContainer
	case outputruntime.FilesystemOutputAuthorityCreatedRoot:
		return FilesystemOutputAuthorityCreatedRoot
	default:
		return ""
	}
}

func projectRuntimeComponent(
	value outputruntime.FilesystemOutputRuntimeComponent,
) FilesystemOutputRuntimeComponent {
	switch value {
	case outputruntime.FilesystemOutputRuntimeSession:
		return FilesystemOutputRuntimeSession
	case outputruntime.FilesystemOutputRuntimeDirectory:
		return FilesystemOutputRuntimeDirectory
	case outputruntime.FilesystemOutputRuntimeFile:
		return FilesystemOutputRuntimeFile
	case outputruntime.FilesystemOutputRuntimeCheckpoint:
		return FilesystemOutputRuntimeCheckpoint
	default:
		return 0
	}
}

func projectRuntimeOperation(
	value outputruntime.FilesystemOutputRuntimeOperation,
) FilesystemOutputRuntimeOperation {
	switch value {
	case outputruntime.FilesystemOutputRuntimeOpenDirectTree:
		return FilesystemOutputRuntimeOpenDirectTree
	case outputruntime.FilesystemOutputRuntimeAcquireOperationLease:
		return FilesystemOutputRuntimeAcquireOperationLease
	case outputruntime.FilesystemOutputRuntimeReconcileCheckpoints:
		return FilesystemOutputRuntimeReconcileCheckpoints
	case outputruntime.FilesystemOutputRuntimeAdmitDirectory:
		return FilesystemOutputRuntimeAdmitDirectory
	case outputruntime.FilesystemOutputRuntimeFinalizeDirectory:
		return FilesystemOutputRuntimeFinalizeDirectory
	case outputruntime.FilesystemOutputRuntimeBeginFile:
		return FilesystemOutputRuntimeBeginFile
	case outputruntime.FilesystemOutputRuntimeWriteRange:
		return FilesystemOutputRuntimeWriteRange
	case outputruntime.FilesystemOutputRuntimeCheckpointFile:
		return FilesystemOutputRuntimeCheckpointFile
	case outputruntime.FilesystemOutputRuntimeCommitFile:
		return FilesystemOutputRuntimeCommitFile
	case outputruntime.FilesystemOutputRuntimePauseFile:
		return FilesystemOutputRuntimePauseFile
	case outputruntime.FilesystemOutputRuntimeRetireFile:
		return FilesystemOutputRuntimeRetireFile
	case outputruntime.FilesystemOutputRuntimePauseTree:
		return FilesystemOutputRuntimePauseTree
	case outputruntime.FilesystemOutputRuntimeFinalizeTree:
		return FilesystemOutputRuntimeFinalizeTree
	case outputruntime.FilesystemOutputRuntimeMaterializeDirectory:
		return FilesystemOutputRuntimeMaterializeDirectory
	case outputruntime.FilesystemOutputRuntimeCreateOwnedFile:
		return FilesystemOutputRuntimeCreateOwnedFile
	case outputruntime.FilesystemOutputRuntimeRecoverFile:
		return FilesystemOutputRuntimeRecoverFile
	case outputruntime.FilesystemOutputRuntimePublishFile:
		return FilesystemOutputRuntimePublishFile
	case outputruntime.FilesystemOutputRuntimeQuarantineFile:
		return FilesystemOutputRuntimeQuarantineFile
	case outputruntime.FilesystemOutputRuntimeAdmitDestination:
		return FilesystemOutputRuntimeAdmitDestination
	case outputruntime.FilesystemOutputRuntimeFirstWrite:
		return FilesystemOutputRuntimeFirstWrite
	case outputruntime.FilesystemOutputRuntimeCleanup:
		return FilesystemOutputRuntimeCleanup
	default:
		return 0
	}
}

func projectRuntimeDecision(
	value outputruntime.FilesystemOutputRuntimeDecision,
) FilesystemOutputRuntimeDecision {
	switch value {
	case outputruntime.FilesystemOutputRuntimeValidated:
		return FilesystemOutputRuntimeValidated
	case outputruntime.FilesystemOutputRuntimeReserved:
		return FilesystemOutputRuntimeReserved
	case outputruntime.FilesystemOutputRuntimeCoalesced:
		return FilesystemOutputRuntimeCoalesced
	case outputruntime.FilesystemOutputRuntimeRejected:
		return FilesystemOutputRuntimeRejected
	case outputruntime.FilesystemOutputRuntimeRolledBack:
		return FilesystemOutputRuntimeRolledBack
	case outputruntime.FilesystemOutputRuntimeAdmitted:
		return FilesystemOutputRuntimeAdmitted
	case outputruntime.FilesystemOutputRuntimeActive:
		return FilesystemOutputRuntimeActive
	case outputruntime.FilesystemOutputRuntimeSealed:
		return FilesystemOutputRuntimeSealed
	case outputruntime.FilesystemOutputRuntimeSettled:
		return FilesystemOutputRuntimeSettled
	case outputruntime.FilesystemOutputRuntimeAmbiguous:
		return FilesystemOutputRuntimeAmbiguous
	case outputruntime.FilesystemOutputRuntimeDraining:
		return FilesystemOutputRuntimeDraining
	case outputruntime.FilesystemOutputRuntimeClosed:
		return FilesystemOutputRuntimeClosed
	case outputruntime.FilesystemOutputRuntimeSucceeded:
		return FilesystemOutputRuntimeSucceeded
	case outputruntime.FilesystemOutputRuntimeReconciled:
		return FilesystemOutputRuntimeReconciled
	case outputruntime.FilesystemOutputRuntimeCollision:
		return FilesystemOutputRuntimeCollision
	case outputruntime.FilesystemOutputRuntimeNoChange:
		return FilesystemOutputRuntimeNoChange
	case outputruntime.FilesystemOutputRuntimeNeedsAttention:
		return FilesystemOutputRuntimeNeedsAttention
	case outputruntime.FilesystemOutputRuntimeIsolatedFailure:
		return FilesystemOutputRuntimeIsolatedFailure
	case outputruntime.FilesystemOutputRuntimeCleanupPending:
		return FilesystemOutputRuntimeCleanupPending
	default:
		return 0
	}
}

func projectCertification(value outputruntime.FilesystemOutputCertificationID) FilesystemOutputCertificationID {
	switch value {
	case outputruntime.FilesystemOutputCertificationLinuxExt4ProcessRestart:
		return FilesystemOutputCertificationLinuxExt4ProcessRestart
	case outputruntime.FilesystemOutputCertificationWindowsNTFSProcessRestart:
		return FilesystemOutputCertificationWindowsNTFSProcessRestart
	default:
		return ""
	}
}

func projectNativeLockScope(value outputruntime.FilesystemOutputNativeLockScope) FilesystemOutputNativeLockScope {
	switch value {
	case outputruntime.FilesystemOutputNativeLockSession:
		return FilesystemOutputNativeLockSession
	default:
		return 0
	}
}

func projectNativeLockMilestone(value outputruntime.FilesystemOutputNativeLockMilestone) FilesystemOutputNativeLockMilestone {
	switch value {
	case outputruntime.FilesystemOutputNativeLockAcquired:
		return FilesystemOutputNativeLockAcquired
	case outputruntime.FilesystemOutputNativeLockContended:
		return FilesystemOutputNativeLockContended
	case outputruntime.FilesystemOutputNativeLockAcquireFailed:
		return FilesystemOutputNativeLockAcquireFailed
	case outputruntime.FilesystemOutputNativeLockReleased:
		return FilesystemOutputNativeLockReleased
	case outputruntime.FilesystemOutputNativeLockReleaseReportedFailure:
		return FilesystemOutputNativeLockReleaseReportedFailure
	default:
		return 0
	}
}
