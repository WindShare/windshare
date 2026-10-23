package eventproject

import (
	"github.com/windshare/windshare/core/osfs"
	appobs "github.com/windshare/windshare/engine/observation"
)

// ProjectFilesystemOutput owns the privacy boundary for native output traces.
// Keeping its closed enum projections beside the event assembly makes it hard
// for a newly exposed authority decision to bypass unknown-value rejection.
func ProjectFilesystemOutput(value osfs.FilesystemOutputTrace) (appobs.FilesystemOutputObserved, error) {
	operation, ok := projectFilesystemOperation(value.Operation)
	if !ok {
		return appobs.FilesystemOutputObserved{}, ErrInvalidProjection
	}
	var receiveID appobs.ReceiveOperationID
	var err error
	if !value.ReceiveOperationID.IsZero() {
		receiveID, err = ReceiveOperationID(value.ReceiveOperationID)
		if err != nil {
			return appobs.FilesystemOutputObserved{}, ErrInvalidProjection
		}
	}
	var receiveIntent appobs.ReceiveIntentDigest
	if !value.ReceiveIntentDigest.IsZero() {
		receiveIntent, err = appobs.NewReceiveIntentDigest(value.ReceiveIntentDigest.Bytes())
		if err != nil {
			return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionInvalidIdentity)
		}
	}
	var outputSession appobs.OutputSessionID
	if !value.SessionID.IsZero() {
		outputSession, err = appobs.NewOutputSessionID(value.SessionID.Bytes())
		if err != nil {
			return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionInvalidIdentity)
		}
	}
	var failure appobs.Failure
	if value.Failed {
		if failure, ok = ProjectNormalizedFault(value.FaultDomain, value.NormalizedFaultScope, value.NormalizedFaultCode); !ok {
			failure = mustFailure(appobs.FailureUnexpected)
		}
	} else if value.FaultDomain != 0 || value.NormalizedFaultScope != 0 || value.NormalizedFaultCode != 0 {
		return appobs.FilesystemOutputObserved{}, ErrInvalidProjection
	}
	certification, ok := projectFilesystemCertification(value.Certification)
	if !ok {
		return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	rootDisposition, ok := projectFilesystemRootDisposition(value.RootOpenDisposition)
	if !ok {
		return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	runtimeComponent, ok := projectFilesystemRuntimeComponent(value.RuntimeComponent)
	if !ok {
		return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	runtimeOperation, ok := projectFilesystemRuntimeOperation(value.RuntimeOperation)
	if !ok {
		return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	runtimeDecision, ok := projectFilesystemRuntimeDecision(value.RuntimeDecision)
	if !ok {
		return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	checkpointDecision, ok := projectFilesystemCheckpointDecision(value.CheckpointDecision)
	if !ok {
		return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	lockScope, ok := projectFilesystemNativeLockScope(value.NativeLockScope)
	if !ok {
		return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	lockMilestone, ok := projectFilesystemNativeLockMilestone(value.NativeLockMilestone)
	if !ok {
		return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	failureStage, ok := projectFilesystemFailureStage(value.FailureStage)
	if !ok {
		return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	reconciliation, ok := projectFilesystemReconciliationStep(value.ReconciliationStep)
	if !ok {
		return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	nativeClass, ok := projectFilesystemNativeErrorClass(value.NativeErrorClass)
	if !ok {
		return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionUnknownEnum)
	}
	capabilities, ok := projectFilesystemCapabilities(value)
	if !ok {
		return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionInvalidStageFields)
	}
	event, err := appobs.NewFilesystemOutputObserved(appobs.FilesystemOutputSpec{
		Capabilities: capabilities,
		Operation:    operation, ReceiveIntent: receiveIntent, ReceiveOperation: receiveID, OutputSession: outputSession,
		Certification: certification, NativeLockScope: lockScope, NativeLockMilestone: lockMilestone,
		RootDisposition: rootDisposition, RuntimeComponent: runtimeComponent,
		RuntimeOperation: runtimeOperation, RuntimeDecision: runtimeDecision,
		CheckpointDecision: checkpointDecision,
		OperationID:        value.OperationID, ClaimID: value.ClaimID,
		Counters: appobs.FilesystemOutputCounters{
			NodeClaims: value.NodeClaimCount, DirectoryClaims: value.DirectoryClaimCount,
			FileClaims: value.FileClaimCount, ActiveFileClaims: value.ActiveFileClaimCount,
			ReservedFileSlots:      value.ReservedFileSlotCount,
			DirectoryMetadataBytes: value.DirectoryMetadataBytes,
			CheckpointRecords:      value.CheckpointRecordCount,
		},
		Failure: failure, FailureStage: failureStage, ReconciliationStep: reconciliation, NativeErrorClass: nativeClass,
	})
	if err != nil {
		return appobs.FilesystemOutputObserved{}, invalidProjection(ProjectionInvalidStageFields)
	}
	return event, nil
}

func projectFilesystemCapabilities(value osfs.FilesystemOutputTrace) (appobs.FilesystemDestinationCapabilities, bool) {
	capabilities := value.DestinationCapabilities
	if value.ExecutionMode == 0 && capabilities.SafePublish().Fact() == 0 &&
		capabilities.OperationRecovery().Fact() == 0 && capabilities.RangeRecovery().Fact() == 0 &&
		capabilities.CrashCleanup().Fact() == 0 {
		return appobs.FilesystemDestinationCapabilities{}, true
	}
	if !value.ExecutionMode.Valid() || !capabilities.Valid() {
		return appobs.FilesystemDestinationCapabilities{}, false
	}
	result := appobs.FilesystemDestinationCapabilities{
		Mode: appobs.FilesystemExecutionMode(value.ExecutionMode),
		SafePublish: appobs.FilesystemCapability{
			Supported: capabilities.SafePublish().Supported(),
			Reason:    appobs.FilesystemCapabilityReason(capabilities.SafePublish().Reason()),
		},
		OperationRecovery: appobs.FilesystemCapability{
			Supported: capabilities.OperationRecovery().Supported(),
			Reason:    appobs.FilesystemCapabilityReason(capabilities.OperationRecovery().Reason()),
		},
		RangeRecovery: appobs.FilesystemCapability{
			Supported: capabilities.RangeRecovery().Supported(),
			Reason:    appobs.FilesystemCapabilityReason(capabilities.RangeRecovery().Reason()),
		},
		CrashCleanup: appobs.FilesystemCapability{
			Supported: capabilities.CrashCleanup().Supported(),
			Reason:    appobs.FilesystemCapabilityReason(capabilities.CrashCleanup().Reason()),
		},
	}
	return result, result.Valid()
}

func projectFilesystemOperation(value osfs.FilesystemOutputTraceOperation) (appobs.FilesystemOutputOperation, bool) {
	switch value {
	case osfs.TraceFilesystemCertified:
		return appobs.FilesystemCertified, true
	case osfs.TraceFeatureProbeCompleted:
		return appobs.FilesystemFeatureProbeCompleted, true
	case osfs.TraceCheckpointNamespaceOpened:
		return appobs.FilesystemCheckpointNamespaceOpened, true
	case osfs.TraceNativeLock:
		return appobs.FilesystemNativeLock, true
	case osfs.TraceSessionOpened:
		return appobs.FilesystemSessionOpened, true
	case osfs.TraceCheckpointReconciled:
		return appobs.FilesystemCheckpointReconciled, true
	case osfs.TraceRuntimeDecision:
		return appobs.FilesystemRuntimeDecision, true
	default:
		return 0, false
	}
}

func projectFilesystemCertification(value osfs.FilesystemOutputCertificationID) (appobs.FilesystemCertification, bool) {
	switch value {
	case "":
		return 0, true
	case osfs.FilesystemOutputCertificationLinuxExt4ProcessRestart:
		return appobs.FilesystemCertificationLinuxExt4ProcessRestart, true
	case osfs.FilesystemOutputCertificationWindowsNTFSProcessRestart:
		return appobs.FilesystemCertificationWindowsNTFSProcessRestart, true
	default:
		return 0, false
	}
}

func projectFilesystemRootDisposition(value osfs.FilesystemOutputRootDisposition) (appobs.FilesystemRootDisposition, bool) {
	switch value {
	case "":
		return 0, true
	case osfs.FilesystemOutputCallerProvidedContainer:
		return appobs.FilesystemRootCallerProvidedContainer, true
	case osfs.FilesystemOutputAuthorityCreatedRoot:
		return appobs.FilesystemRootAuthorityCreated, true
	default:
		return 0, false
	}
}

func projectFilesystemRuntimeComponent(value osfs.FilesystemOutputRuntimeComponent) (appobs.FilesystemRuntimeComponent, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputRuntimeSession || value > osfs.FilesystemOutputRuntimeCheckpoint {
		return 0, false
	}
	return appobs.FilesystemRuntimeComponent(value), true
}

func projectFilesystemRuntimeOperation(value osfs.FilesystemOutputRuntimeOperation) (appobs.FilesystemRuntimeOperation, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputRuntimeOpenDirectTree || value > osfs.FilesystemOutputRuntimeCleanup {
		return 0, false
	}
	return appobs.FilesystemRuntimeOperation(value), true
}

func projectFilesystemRuntimeDecision(value osfs.FilesystemOutputRuntimeDecision) (appobs.FilesystemRuntimeDecisionKind, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputRuntimeValidated || value > osfs.FilesystemOutputRuntimeCleanupPending {
		return 0, false
	}
	return appobs.FilesystemRuntimeDecisionKind(value), true
}

func projectFilesystemCheckpointDecision(value osfs.FilesystemCheckpointDecision) (appobs.FilesystemCheckpointDecision, bool) {
	switch value {
	case 0:
		return 0, true
	case osfs.FilesystemCheckpointAbsent:
		return appobs.FilesystemCheckpointAbsent, true
	case osfs.FilesystemCheckpointExact:
		return appobs.FilesystemCheckpointExact, true
	case osfs.FilesystemCheckpointRevisionConflict:
		return appobs.FilesystemCheckpointRevisionConflict, true
	case osfs.FilesystemCheckpointOwnershipConflict:
		return appobs.FilesystemCheckpointOwnershipConflict, true
	case osfs.FilesystemCheckpointInvalid:
		return appobs.FilesystemCheckpointInvalid, true
	default:
		return 0, false
	}
}

func projectFilesystemNativeLockScope(value osfs.FilesystemOutputNativeLockScope) (appobs.FilesystemNativeLockScope, bool) {
	if value == 0 {
		return 0, true
	}
	if value != osfs.FilesystemOutputNativeLockSession {
		return 0, false
	}
	return appobs.FilesystemNativeLockSession, true
}

func projectFilesystemNativeLockMilestone(value osfs.FilesystemOutputNativeLockMilestone) (appobs.FilesystemNativeLockMilestone, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputNativeLockAcquired || value > osfs.FilesystemOutputNativeLockReleaseReportedFailure {
		return 0, false
	}
	return appobs.FilesystemNativeLockMilestone(value), true
}

func projectFilesystemFailureStage(value osfs.FilesystemOutputFailureStage) (appobs.FilesystemFailureStage, bool) {
	if value == 0 {
		return 0, true
	}
	if !value.Valid() {
		return 0, false
	}
	return appobs.FilesystemFailureStage(value), true
}

func projectFilesystemReconciliationStep(value osfs.FilesystemCheckpointReconciliationStep) (appobs.FilesystemReconciliationStep, bool) {
	if value == 0 {
		return 0, true
	}
	if !value.Valid() {
		return 0, false
	}
	return appobs.FilesystemReconciliationStep(value), true
}

func projectFilesystemNativeErrorClass(value osfs.FilesystemNativeErrorClass) (appobs.FilesystemNativeErrorClass, bool) {
	if value == 0 {
		return 0, true
	}
	if !value.Valid() {
		return 0, false
	}
	return appobs.FilesystemNativeErrorClass(value), true
}
