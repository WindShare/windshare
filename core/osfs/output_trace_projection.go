package osfs

import "github.com/windshare/windshare/core/osfs/internal/outputruntime"

// outputRuntimeTracer is the single semantic projection from internal runtime
// observability to the stable public event vocabulary. Each enum is mapped
// explicitly so adding a runtime decision cannot silently acquire a public value.
type outputRuntimeTracer struct {
	target FilesystemOutputTracer
}

func (tracer outputRuntimeTracer) TraceFilesystemOutput(event outputruntime.FilesystemOutputTrace) {
	if tracer.target != nil {
		tracer.target.TraceFilesystemOutput(projectFilesystemOutputTrace(event))
	}
}

func projectFilesystemOutputTrace(event outputruntime.FilesystemOutputTrace) FilesystemOutputTrace {
	var ancestryDigest FilesystemOutputAncestryDigest
	copy(ancestryDigest[:], event.OutputAncestryDigest.Bytes())
	return FilesystemOutputTrace{
		Operation: projectTraceOperation(event.Operation), IntentDigest: event.IntentDigest,
		SessionID: event.SessionID, LocatorDigest: event.LocatorDigest, OutputObjectID: event.OutputObjectID,
		PreviousPhase: projectFilePhase(event.PreviousPhase), NextPhase: projectFilePhase(event.NextPhase),
		RecoveryAction: projectRecoveryAction(event.RecoveryAction), FileSettlement: event.FileSettlement,
		FileSettlementBoundary: projectFileSettlementBoundary(event.FileSettlementBoundary),
		FilePauseReason:        event.FilePauseReason, FileRetireReason: event.FileRetireReason,
		QuarantineReason: event.QuarantineReason, JobSettlement: event.JobSettlement,
		FailureScope: event.FailureScope, FailureCode: event.FailureCode,
		Certification: projectCertification(event.Certification), StateGeneration: event.StateGeneration,
		StateInstallStage: projectStateInstallStage(event.StateInstallStage),
		SelectionIdentity: event.SelectionIdentity, OutputAncestryDigest: ancestryDigest,
		AncestryBoundary:          projectAncestryBoundary(event.AncestryBoundary),
		AncestryDecision:          projectAncestryDecision(event.AncestryDecision),
		AncestryClaimCount:        event.AncestryClaimCount,
		NativeLockScope:           projectNativeLockScope(event.NativeLockScope),
		NativeLockMilestone:       projectNativeLockMilestone(event.NativeLockMilestone),
		MutationReportedFailure:   event.MutationReportedFailure,
		ParentSyncReportedFailure: event.ParentSyncReportedFailure,
		CleanupRemoved:            event.CleanupRemoved, CleanupQuarantined: event.CleanupQuarantined,
		CleanupSkipped: event.CleanupSkipped, Failed: event.Failed,
	}
}

func projectTraceOperation(value outputruntime.FilesystemOutputTraceOperation) FilesystemOutputTraceOperation {
	switch value {
	case outputruntime.TraceFilesystemCertified:
		return TraceFilesystemCertified
	case outputruntime.TraceFeatureProbeCompleted:
		return TraceFeatureProbeCompleted
	case outputruntime.TraceCheckpointCleanup:
		return TraceCheckpointCleanup
	case outputruntime.TraceControlBootstrap:
		return TraceControlBootstrap
	case outputruntime.TraceNativeLock:
		return TraceNativeLock
	case outputruntime.TraceSessionOpened:
		return TraceSessionOpened
	case outputruntime.TraceFilePhaseTransition:
		return TraceFilePhaseTransition
	case outputruntime.TraceFileRecoveryDecision:
		return TraceFileRecoveryDecision
	case outputruntime.TraceFileSettlement:
		return TraceFileSettlement
	case outputruntime.TraceSessionSettlement:
		return TraceSessionSettlement
	case outputruntime.TraceStateInstallCutAdopted:
		return TraceStateInstallCutAdopted
	case outputruntime.TraceAncestryValidation:
		return TraceAncestryValidation
	default:
		return 0
	}
}

func projectFilePhase(value outputruntime.FilesystemOutputFilePhase) FilesystemOutputFilePhase {
	switch value {
	case outputruntime.FilesystemOutputFileReserved:
		return FilesystemOutputFileReserved
	case outputruntime.FilesystemOutputFileWitnessed:
		return FilesystemOutputFileWitnessed
	case outputruntime.FilesystemOutputFilePublishing:
		return FilesystemOutputFilePublishing
	case outputruntime.FilesystemOutputFilePublishBlocked:
		return FilesystemOutputFilePublishBlocked
	case outputruntime.FilesystemOutputFilePublished:
		return FilesystemOutputFilePublished
	case outputruntime.FilesystemOutputFileRetiring:
		return FilesystemOutputFileRetiring
	case outputruntime.FilesystemOutputFileQuarantined:
		return FilesystemOutputFileQuarantined
	default:
		return 0
	}
}

func projectRecoveryAction(value outputruntime.FilesystemOutputRecoveryAction) FilesystemOutputRecoveryAction {
	switch value {
	case outputruntime.FilesystemOutputRecoveryRetryObjectCreation:
		return FilesystemOutputRecoveryRetryObjectCreation
	case outputruntime.FilesystemOutputRecoveryInstallWitness:
		return FilesystemOutputRecoveryInstallWitness
	case outputruntime.FilesystemOutputRecoveryRequireRevisionBinding:
		return FilesystemOutputRecoveryRequireRevisionBinding
	case outputruntime.FilesystemOutputRecoveryResumeContent:
		return FilesystemOutputRecoveryResumeContent
	case outputruntime.FilesystemOutputRecoveryInstallPublishing:
		return FilesystemOutputRecoveryInstallPublishing
	case outputruntime.FilesystemOutputRecoveryLinkFinalNoReplace:
		return FilesystemOutputRecoveryLinkFinalNoReplace
	case outputruntime.FilesystemOutputRecoverySyncFinalParent:
		return FilesystemOutputRecoverySyncFinalParent
	case outputruntime.FilesystemOutputRecoveryInstallPublished:
		return FilesystemOutputRecoveryInstallPublished
	case outputruntime.FilesystemOutputRecoveryInstallPublishBlocked:
		return FilesystemOutputRecoveryInstallPublishBlocked
	case outputruntime.FilesystemOutputRecoveryHoldPublishBlocked:
		return FilesystemOutputRecoveryHoldPublishBlocked
	case outputruntime.FilesystemOutputRecoveryRemovePublishedStageAndSync:
		return FilesystemOutputRecoveryRemovePublishedStageAndSync
	case outputruntime.FilesystemOutputRecoverySyncPublishedStageParent:
		return FilesystemOutputRecoverySyncPublishedStageParent
	case outputruntime.FilesystemOutputRecoveryRemoveRetiringStageAndSync:
		return FilesystemOutputRecoveryRemoveRetiringStageAndSync
	case outputruntime.FilesystemOutputRecoverySyncStageRemoveAnchorAndSync:
		return FilesystemOutputRecoverySyncStageRemoveAnchorAndSync
	case outputruntime.FilesystemOutputRecoverySyncParentsRemoveRecordAndSync:
		return FilesystemOutputRecoverySyncParentsRemoveRecordAndSync
	case outputruntime.FilesystemOutputRecoveryInstallRetiring:
		return FilesystemOutputRecoveryInstallRetiring
	case outputruntime.FilesystemOutputRecoveryInstallQuarantine:
		return FilesystemOutputRecoveryInstallQuarantine
	case outputruntime.FilesystemOutputRecoveryHoldQuarantine:
		return FilesystemOutputRecoveryHoldQuarantine
	case outputruntime.FilesystemOutputRecoveryHoldPublishedCleanup:
		return FilesystemOutputRecoveryHoldPublishedCleanup
	case outputruntime.FilesystemOutputRecoveryHoldRetiringCleanup:
		return FilesystemOutputRecoveryHoldRetiringCleanup
	default:
		return 0
	}
}

func projectFileSettlementBoundary(value outputruntime.FilesystemOutputFileSettlementBoundary) FilesystemOutputFileSettlementBoundary {
	switch value {
	case outputruntime.FilesystemOutputSettlementBeginFile:
		return FilesystemOutputSettlementBeginFile
	case outputruntime.FilesystemOutputSettlementCommit:
		return FilesystemOutputSettlementCommit
	case outputruntime.FilesystemOutputSettlementPause:
		return FilesystemOutputSettlementPause
	case outputruntime.FilesystemOutputSettlementJobPause:
		return FilesystemOutputSettlementJobPause
	case outputruntime.FilesystemOutputSettlementBeginFileCleanup:
		return FilesystemOutputSettlementBeginFileCleanup
	case outputruntime.FilesystemOutputSettlementRetire:
		return FilesystemOutputSettlementRetire
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

func projectStateInstallStage(value outputruntime.FilesystemOutputStateInstallStage) FilesystemOutputStateInstallStage {
	switch value {
	case outputruntime.FilesystemOutputStateCreate:
		return FilesystemOutputStateCreate
	case outputruntime.FilesystemOutputStateReplace:
		return FilesystemOutputStateReplace
	default:
		return 0
	}
}

func projectAncestryBoundary(value outputruntime.FilesystemOutputAncestryBoundary) FilesystemOutputAncestryBoundary {
	switch value {
	case outputruntime.FilesystemOutputAncestryAdmission:
		return FilesystemOutputAncestryAdmission
	case outputruntime.FilesystemOutputAncestryRestart:
		return FilesystemOutputAncestryRestart
	case outputruntime.FilesystemOutputAncestryBeginFile:
		return FilesystemOutputAncestryBeginFile
	case outputruntime.FilesystemOutputAncestryRecovery:
		return FilesystemOutputAncestryRecovery
	case outputruntime.FilesystemOutputAncestryPublicationPre:
		return FilesystemOutputAncestryPublicationPre
	case outputruntime.FilesystemOutputAncestryPublicationPost:
		return FilesystemOutputAncestryPublicationPost
	case outputruntime.FilesystemOutputAncestryDirectoryFinalize:
		return FilesystemOutputAncestryDirectoryFinalize
	case outputruntime.FilesystemOutputAncestrySessionFinalize:
		return FilesystemOutputAncestrySessionFinalize
	default:
		return 0
	}
}

func projectAncestryDecision(value outputruntime.FilesystemOutputAncestryDecision) FilesystemOutputAncestryDecision {
	switch value {
	case outputruntime.FilesystemOutputAncestryPrepared:
		return FilesystemOutputAncestryPrepared
	case outputruntime.FilesystemOutputAncestryMatched:
		return FilesystemOutputAncestryMatched
	case outputruntime.FilesystemOutputAncestryMismatch:
		return FilesystemOutputAncestryMismatch
	case outputruntime.FilesystemOutputAncestryAuthorityDenied:
		return FilesystemOutputAncestryAuthorityDenied
	case outputruntime.FilesystemOutputAncestryStructuralUnsafe:
		return FilesystemOutputAncestryStructuralUnsafe
	default:
		return 0
	}
}

func projectNativeLockScope(value outputruntime.FilesystemOutputNativeLockScope) FilesystemOutputNativeLockScope {
	switch value {
	case outputruntime.FilesystemOutputNativeLockCoordinator:
		return FilesystemOutputNativeLockCoordinator
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
