package commandprojection

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/osfs"
)

// ProjectFilesystemOutput owns the privacy boundary for native output traces.
// Keeping its closed enum projections beside the event assembly makes it hard
// for a newly exposed authority decision to bypass unknown-value rejection.
func ProjectFilesystemOutput(value osfs.FilesystemOutputTrace) (clievent.FilesystemOutputObserved, error) {
	operation, ok := projectFilesystemOperation(value.Operation)
	if !ok {
		return clievent.FilesystemOutputObserved{}, ErrInvalidProjection
	}
	var receiveID clievent.ReceiveOperationID
	var err error
	if !value.ReceiveOperationID.IsZero() {
		receiveID, err = ReceiveOperationID(value.ReceiveOperationID)
		if err != nil {
			return clievent.FilesystemOutputObserved{}, ErrInvalidProjection
		}
	}
	var receiveIntent clievent.ReceiveIntentDigest
	if !value.ReceiveIntentDigest.IsZero() {
		receiveIntent, err = clievent.NewReceiveIntentDigest(value.ReceiveIntentDigest.Bytes())
		if err != nil {
			return clievent.FilesystemOutputObserved{}, invalidProjection(ProjectionInvalidIdentity)
		}
	}
	var outputSession clievent.OutputSessionID
	if !value.SessionID.IsZero() {
		outputSession, err = clievent.NewOutputSessionID(value.SessionID.Bytes())
		if err != nil {
			return clievent.FilesystemOutputObserved{}, invalidProjection(ProjectionInvalidIdentity)
		}
	}
	var failure clievent.Failure
	if value.Failed {
		if failure, ok = ProjectNormalizedFault(value.FaultDomain, value.NormalizedFaultScope, value.NormalizedFaultCode); !ok {
			failure = mustFailure(clievent.FailureUnexpected)
		}
	} else if value.FaultDomain != 0 || value.NormalizedFaultScope != 0 || value.NormalizedFaultCode != 0 {
		return clievent.FilesystemOutputObserved{}, ErrInvalidProjection
	}
	certification, ok := projectFilesystemCertification(value.Certification)
	if !ok {
		return clievent.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	rootDisposition, ok := projectFilesystemRootDisposition(value.RootOpenDisposition)
	if !ok {
		return clievent.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	runtimeComponent, ok := projectFilesystemRuntimeComponent(value.RuntimeComponent)
	if !ok {
		return clievent.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	runtimeOperation, ok := projectFilesystemRuntimeOperation(value.RuntimeOperation)
	if !ok {
		return clievent.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	runtimeDecision, ok := projectFilesystemRuntimeDecision(value.RuntimeDecision)
	if !ok {
		return clievent.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	checkpointDecision, ok := projectFilesystemCheckpointDecision(value.CheckpointDecision)
	if !ok {
		return clievent.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	lockScope, ok := projectFilesystemNativeLockScope(value.NativeLockScope)
	if !ok {
		return clievent.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	lockMilestone, ok := projectFilesystemNativeLockMilestone(value.NativeLockMilestone)
	if !ok {
		return clievent.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	failureStage, ok := projectFilesystemFailureStage(value.FailureStage)
	if !ok {
		return clievent.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	reconciliation, ok := projectFilesystemReconciliationStep(value.ReconciliationStep)
	if !ok {
		return clievent.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	nativeClass, ok := projectFilesystemNativeErrorClass(value.NativeErrorClass)
	if !ok {
		return clievent.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	event, err := clievent.NewFilesystemOutputObserved(clievent.FilesystemOutputSpec{
		Operation: operation, ReceiveIntent: receiveIntent, ReceiveOperation: receiveID, OutputSession: outputSession,
		Certification: certification, NativeLockScope: lockScope, NativeLockMilestone: lockMilestone,
		RootDisposition: rootDisposition, RuntimeComponent: runtimeComponent,
		RuntimeOperation: runtimeOperation, RuntimeDecision: runtimeDecision,
		CheckpointDecision: checkpointDecision,
		OperationID:        value.OperationID, ClaimID: value.ClaimID,
		Counters: clievent.FilesystemOutputCounters{
			NodeClaims: value.NodeClaimCount, DirectoryClaims: value.DirectoryClaimCount,
			FileClaims: value.FileClaimCount, ActiveFileClaims: value.ActiveFileClaimCount,
			ReservedFileSlots:      value.ReservedFileSlotCount,
			DirectoryMetadataBytes: value.DirectoryMetadataBytes,
			CheckpointRecords:      value.CheckpointRecordCount,
		},
		Failure: failure, FailureStage: failureStage, ReconciliationStep: reconciliation, NativeErrorClass: nativeClass,
	})
	if err != nil {
		return clievent.FilesystemOutputObserved{}, invalidProjection(ProjectionInvalidStageFields)
	}
	return event, nil
}

func projectFilesystemOperation(value osfs.FilesystemOutputTraceOperation) (clievent.FilesystemOutputOperation, bool) {
	switch value {
	case osfs.TraceFilesystemCertified:
		return clievent.FilesystemCertified, true
	case osfs.TraceFeatureProbeCompleted:
		return clievent.FilesystemFeatureProbeCompleted, true
	case osfs.TraceCheckpointNamespaceOpened:
		return clievent.FilesystemCheckpointNamespaceOpened, true
	case osfs.TraceNativeLock:
		return clievent.FilesystemNativeLock, true
	case osfs.TraceSessionOpened:
		return clievent.FilesystemSessionOpened, true
	case osfs.TraceCheckpointReconciled:
		return clievent.FilesystemCheckpointReconciled, true
	case osfs.TraceRuntimeDecision:
		return clievent.FilesystemRuntimeDecision, true
	default:
		return 0, false
	}
}

func projectFilesystemCertification(value osfs.FilesystemOutputCertificationID) (clievent.FilesystemCertification, bool) {
	switch value {
	case "":
		return 0, true
	case osfs.FilesystemOutputCertificationLinuxExt4ProcessRestart:
		return clievent.FilesystemCertificationLinuxExt4ProcessRestart, true
	case osfs.FilesystemOutputCertificationWindowsNTFSProcessRestart:
		return clievent.FilesystemCertificationWindowsNTFSProcessRestart, true
	default:
		return 0, false
	}
}

func projectFilesystemRootDisposition(value osfs.FilesystemOutputRootDisposition) (clievent.FilesystemRootDisposition, bool) {
	switch value {
	case "":
		return 0, true
	case osfs.FilesystemOutputCallerProvidedContainer:
		return clievent.FilesystemRootCallerProvidedContainer, true
	case osfs.FilesystemOutputAuthorityCreatedRoot:
		return clievent.FilesystemRootAuthorityCreated, true
	default:
		return 0, false
	}
}

func projectFilesystemRuntimeComponent(value osfs.FilesystemOutputRuntimeComponent) (clievent.FilesystemRuntimeComponent, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputRuntimeSession || value > osfs.FilesystemOutputRuntimeCheckpoint {
		return 0, false
	}
	return clievent.FilesystemRuntimeComponent(value), true
}

func projectFilesystemRuntimeOperation(value osfs.FilesystemOutputRuntimeOperation) (clievent.FilesystemRuntimeOperation, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputRuntimeOpenDirectTree || value > osfs.FilesystemOutputRuntimeCleanup {
		return 0, false
	}
	return clievent.FilesystemRuntimeOperation(value), true
}

func projectFilesystemRuntimeDecision(value osfs.FilesystemOutputRuntimeDecision) (clievent.FilesystemRuntimeDecisionKind, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputRuntimeValidated || value > osfs.FilesystemOutputRuntimeCleanupPending {
		return 0, false
	}
	return clievent.FilesystemRuntimeDecisionKind(value), true
}

func projectFilesystemCheckpointDecision(value osfs.FilesystemCheckpointDecision) (clievent.FilesystemCheckpointDecision, bool) {
	switch value {
	case 0:
		return 0, true
	case osfs.FilesystemCheckpointAbsent:
		return clievent.FilesystemCheckpointAbsent, true
	case osfs.FilesystemCheckpointExact:
		return clievent.FilesystemCheckpointExact, true
	case osfs.FilesystemCheckpointRevisionConflict:
		return clievent.FilesystemCheckpointRevisionConflict, true
	case osfs.FilesystemCheckpointOwnershipConflict:
		return clievent.FilesystemCheckpointOwnershipConflict, true
	case osfs.FilesystemCheckpointInvalid:
		return clievent.FilesystemCheckpointInvalid, true
	default:
		return 0, false
	}
}

func projectFilesystemNativeLockScope(value osfs.FilesystemOutputNativeLockScope) (clievent.FilesystemNativeLockScope, bool) {
	if value == 0 {
		return 0, true
	}
	if value != osfs.FilesystemOutputNativeLockSession {
		return 0, false
	}
	return clievent.FilesystemNativeLockSession, true
}

func projectFilesystemNativeLockMilestone(value osfs.FilesystemOutputNativeLockMilestone) (clievent.FilesystemNativeLockMilestone, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputNativeLockAcquired || value > osfs.FilesystemOutputNativeLockReleaseReportedFailure {
		return 0, false
	}
	return clievent.FilesystemNativeLockMilestone(value), true
}

func projectFilesystemFailureStage(value osfs.FilesystemOutputFailureStage) (clievent.FilesystemFailureStage, bool) {
	if value == 0 {
		return 0, true
	}
	if !value.Valid() {
		return 0, false
	}
	return clievent.FilesystemFailureStage(value), true
}

func projectFilesystemReconciliationStep(value osfs.FilesystemCheckpointReconciliationStep) (clievent.FilesystemReconciliationStep, bool) {
	if value == 0 {
		return 0, true
	}
	if !value.Valid() {
		return 0, false
	}
	return clievent.FilesystemReconciliationStep(value), true
}

func projectFilesystemNativeErrorClass(value osfs.FilesystemNativeErrorClass) (clievent.FilesystemNativeErrorClass, bool) {
	if value == 0 {
		return 0, true
	}
	if !value.Valid() {
		return 0, false
	}
	return clievent.FilesystemNativeErrorClass(value), true
}
