package filesystem

import (
	fsobs "github.com/windshare/windshare/adapters/filesystem/diagnostic"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/osfs"
)

// ProjectFilesystemOutput owns the privacy boundary for native output traces.
// Keeping its closed enum projections beside the event assembly makes it hard
// for a newly exposed authority decision to bypass unknown-value rejection.
func ProjectFilesystemOutput(value osfs.FilesystemOutputTrace) (fsobs.FilesystemOutputObserved, error) {
	operation, ok := projectFilesystemOperation(value.Operation)
	if !ok {
		return fsobs.FilesystemOutputObserved{}, appobs.ErrInvalidEvent
	}
	var receiveID appobs.ReceiveOperationID
	var err error
	if !value.ReceiveOperationID.IsZero() {
		receiveID, err = appobs.NewReceiveOperationID(value.ReceiveOperationID.Bytes())
		if err != nil {
			return fsobs.FilesystemOutputObserved{}, appobs.ErrInvalidEvent
		}
	}
	var receiveIntent appobs.ReceiveIntentDigest
	if !value.ReceiveIntentDigest.IsZero() {
		receiveIntent, err = appobs.NewReceiveIntentDigest(value.ReceiveIntentDigest.Bytes())
		if err != nil {
			return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossInvalidIdentity)
		}
	}
	var outputSession appobs.OutputSessionID
	if !value.SessionID.IsZero() {
		outputSession, err = appobs.NewOutputSessionID(value.SessionID.Bytes())
		if err != nil {
			return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossInvalidIdentity)
		}
	}
	var failure appobs.Failure
	if value.Failed {
		if failure, ok = appobs.ProjectNormalizedFault(value.FaultDomain, value.NormalizedFaultScope, value.NormalizedFaultCode); !ok {
			failure, _ = appobs.NewFailure(appobs.FailureUnexpected)
		}
	} else if value.FaultDomain != 0 || value.NormalizedFaultScope != 0 || value.NormalizedFaultCode != 0 {
		return fsobs.FilesystemOutputObserved{}, appobs.ErrInvalidEvent
	}
	certification, ok := projectFilesystemCertification(value.Certification)
	if !ok {
		return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossUnknownEnum)
	}
	rootDisposition, ok := projectFilesystemRootDisposition(value.RootOpenDisposition)
	if !ok {
		return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossUnknownEnum)
	}
	runtimeComponent, ok := projectFilesystemRuntimeComponent(value.RuntimeComponent)
	if !ok {
		return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossUnknownEnum)
	}
	runtimeOperation, ok := projectFilesystemRuntimeOperation(value.RuntimeOperation)
	if !ok {
		return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossUnknownEnum)
	}
	runtimeDecision, ok := projectFilesystemRuntimeDecision(value.RuntimeDecision)
	if !ok {
		return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossUnknownEnum)
	}
	checkpointDecision, ok := projectFilesystemCheckpointDecision(value.CheckpointDecision)
	if !ok {
		return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossUnknownEnum)
	}
	lockScope, ok := projectFilesystemNativeLockScope(value.NativeLockScope)
	if !ok {
		return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossUnknownEnum)
	}
	lockMilestone, ok := projectFilesystemNativeLockMilestone(value.NativeLockMilestone)
	if !ok {
		return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossUnknownEnum)
	}
	failureStage, ok := projectFilesystemFailureStage(value.FailureStage)
	if !ok {
		return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossUnknownEnum)
	}
	reconciliation, ok := projectFilesystemReconciliationStep(value.ReconciliationStep)
	if !ok {
		return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossUnknownEnum)
	}
	nativeClass, ok := projectFilesystemNativeErrorClass(value.NativeErrorClass)
	if !ok {
		return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossUnknownEnum)
	}
	capabilities, ok := projectFilesystemCapabilities(value)
	if !ok {
		return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossInvalidStageFields)
	}
	event, err := fsobs.NewFilesystemOutputObserved(fsobs.FilesystemOutputSpec{
		Capabilities: capabilities,
		Operation:    operation, ReceiveIntent: receiveIntent, ReceiveOperation: receiveID, OutputSession: outputSession,
		Certification: certification, NativeLockScope: lockScope, NativeLockMilestone: lockMilestone,
		RootDisposition: rootDisposition, RuntimeComponent: runtimeComponent,
		RuntimeOperation: runtimeOperation, RuntimeDecision: runtimeDecision,
		CheckpointDecision: checkpointDecision,
		OperationID:        value.OperationID, ClaimID: value.ClaimID,
		Counters: fsobs.FilesystemOutputCounters{
			NodeClaims: value.NodeClaimCount, DirectoryClaims: value.DirectoryClaimCount,
			FileClaims: value.FileClaimCount, ActiveFileClaims: value.ActiveFileClaimCount,
			ReservedFileSlots:      value.ReservedFileSlotCount,
			DirectoryMetadataBytes: value.DirectoryMetadataBytes,
			CheckpointRecords:      value.CheckpointRecordCount,
		},
		Failure: failure, FailureStage: failureStage, ReconciliationStep: reconciliation, NativeErrorClass: nativeClass,
	})
	if err != nil {
		return fsobs.FilesystemOutputObserved{}, invalidProjection(appobs.ObserverLossInvalidStageFields)
	}
	return event, nil
}

func projectFilesystemCapabilities(value osfs.FilesystemOutputTrace) (fsobs.FilesystemDestinationCapabilities, bool) {
	capabilities := value.DestinationCapabilities
	if value.ExecutionMode == 0 && capabilities.SafePublish().Fact() == 0 &&
		capabilities.OperationRecovery().Fact() == 0 && capabilities.RangeRecovery().Fact() == 0 &&
		capabilities.CrashCleanup().Fact() == 0 {
		return fsobs.FilesystemDestinationCapabilities{}, true
	}
	if !value.ExecutionMode.Valid() || !capabilities.Valid() {
		return fsobs.FilesystemDestinationCapabilities{}, false
	}
	result := fsobs.FilesystemDestinationCapabilities{
		Mode: fsobs.FilesystemExecutionMode(value.ExecutionMode),
		SafePublish: fsobs.FilesystemCapability{
			Supported: capabilities.SafePublish().Supported(),
			Reason:    fsobs.FilesystemCapabilityReason(capabilities.SafePublish().Reason()),
		},
		OperationRecovery: fsobs.FilesystemCapability{
			Supported: capabilities.OperationRecovery().Supported(),
			Reason:    fsobs.FilesystemCapabilityReason(capabilities.OperationRecovery().Reason()),
		},
		RangeRecovery: fsobs.FilesystemCapability{
			Supported: capabilities.RangeRecovery().Supported(),
			Reason:    fsobs.FilesystemCapabilityReason(capabilities.RangeRecovery().Reason()),
		},
		CrashCleanup: fsobs.FilesystemCapability{
			Supported: capabilities.CrashCleanup().Supported(),
			Reason:    fsobs.FilesystemCapabilityReason(capabilities.CrashCleanup().Reason()),
		},
	}
	return result, result.Valid()
}

func projectFilesystemOperation(value osfs.FilesystemOutputTraceOperation) (fsobs.FilesystemOutputOperation, bool) {
	switch value {
	case osfs.TraceFilesystemCertified:
		return fsobs.FilesystemCertified, true
	case osfs.TraceFeatureProbeCompleted:
		return fsobs.FilesystemFeatureProbeCompleted, true
	case osfs.TraceCheckpointNamespaceOpened:
		return fsobs.FilesystemCheckpointNamespaceOpened, true
	case osfs.TraceNativeLock:
		return fsobs.FilesystemNativeLock, true
	case osfs.TraceSessionOpened:
		return fsobs.FilesystemSessionOpened, true
	case osfs.TraceCheckpointReconciled:
		return fsobs.FilesystemCheckpointReconciled, true
	case osfs.TraceRuntimeDecision:
		return fsobs.FilesystemRuntimeDecision, true
	default:
		return 0, false
	}
}

func projectFilesystemCertification(value osfs.FilesystemOutputCertificationID) (fsobs.FilesystemCertification, bool) {
	switch value {
	case "":
		return 0, true
	case osfs.FilesystemOutputCertificationLinuxExt4ProcessRestart:
		return fsobs.FilesystemCertificationLinuxExt4ProcessRestart, true
	case osfs.FilesystemOutputCertificationWindowsNTFSProcessRestart:
		return fsobs.FilesystemCertificationWindowsNTFSProcessRestart, true
	default:
		return 0, false
	}
}

func projectFilesystemRootDisposition(value osfs.FilesystemOutputRootDisposition) (fsobs.FilesystemRootDisposition, bool) {
	switch value {
	case "":
		return 0, true
	case osfs.FilesystemOutputCallerProvidedContainer:
		return fsobs.FilesystemRootCallerProvidedContainer, true
	case osfs.FilesystemOutputAuthorityCreatedRoot:
		return fsobs.FilesystemRootAuthorityCreated, true
	default:
		return 0, false
	}
}

func projectFilesystemRuntimeComponent(value osfs.FilesystemOutputRuntimeComponent) (fsobs.FilesystemRuntimeComponent, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputRuntimeSession || value > osfs.FilesystemOutputRuntimeCheckpoint {
		return 0, false
	}
	return fsobs.FilesystemRuntimeComponent(value), true
}

func projectFilesystemRuntimeOperation(value osfs.FilesystemOutputRuntimeOperation) (fsobs.FilesystemRuntimeOperation, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputRuntimeOpenDirectTree || value > osfs.FilesystemOutputRuntimeCleanup {
		return 0, false
	}
	return fsobs.FilesystemRuntimeOperation(value), true
}

func projectFilesystemRuntimeDecision(value osfs.FilesystemOutputRuntimeDecision) (fsobs.FilesystemRuntimeDecisionKind, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputRuntimeValidated || value > osfs.FilesystemOutputRuntimeCleanupPending {
		return 0, false
	}
	return fsobs.FilesystemRuntimeDecisionKind(value), true
}

func projectFilesystemCheckpointDecision(value osfs.FilesystemCheckpointDecision) (fsobs.FilesystemCheckpointDecision, bool) {
	switch value {
	case 0:
		return 0, true
	case osfs.FilesystemCheckpointAbsent:
		return fsobs.FilesystemCheckpointAbsent, true
	case osfs.FilesystemCheckpointExact:
		return fsobs.FilesystemCheckpointExact, true
	case osfs.FilesystemCheckpointRevisionConflict:
		return fsobs.FilesystemCheckpointRevisionConflict, true
	case osfs.FilesystemCheckpointOwnershipConflict:
		return fsobs.FilesystemCheckpointOwnershipConflict, true
	case osfs.FilesystemCheckpointInvalid:
		return fsobs.FilesystemCheckpointInvalid, true
	default:
		return 0, false
	}
}

func projectFilesystemNativeLockScope(value osfs.FilesystemOutputNativeLockScope) (fsobs.FilesystemNativeLockScope, bool) {
	if value == 0 {
		return 0, true
	}
	if value != osfs.FilesystemOutputNativeLockSession {
		return 0, false
	}
	return fsobs.FilesystemNativeLockSession, true
}

func projectFilesystemNativeLockMilestone(value osfs.FilesystemOutputNativeLockMilestone) (fsobs.FilesystemNativeLockMilestone, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputNativeLockAcquired || value > osfs.FilesystemOutputNativeLockReleaseReportedFailure {
		return 0, false
	}
	return fsobs.FilesystemNativeLockMilestone(value), true
}

func projectFilesystemFailureStage(value osfs.FilesystemOutputFailureStage) (fsobs.FilesystemFailureStage, bool) {
	if value == 0 {
		return 0, true
	}
	if !value.Valid() {
		return 0, false
	}
	return fsobs.FilesystemFailureStage(value), true
}

func projectFilesystemReconciliationStep(value osfs.FilesystemCheckpointReconciliationStep) (fsobs.FilesystemReconciliationStep, bool) {
	if value == 0 {
		return 0, true
	}
	if !value.Valid() {
		return 0, false
	}
	return fsobs.FilesystemReconciliationStep(value), true
}

func projectFilesystemNativeErrorClass(value osfs.FilesystemNativeErrorClass) (fsobs.FilesystemNativeErrorClass, bool) {
	if value == 0 {
		return 0, true
	}
	if !value.Valid() {
		return 0, false
	}
	return fsobs.FilesystemNativeErrorClass(value), true
}

func invalidProjection(reason appobs.ObserverLossReason) error {
	return &appobs.OutputDiagnosticError{Reason: reason, Context: appobs.ObservationRejection{
		Source: "filesystem", Event: "filesystem_output", Stage: "projection", Field: "event", Rule: "provider_contract",
	}}
}
func traceOutput(observe func(appobs.OutputDiagnostic, error)) osfs.FilesystemOutputTracer {
	if observe == nil {
		return nil
	}
	return osfs.FilesystemOutputTraceFunc(func(value osfs.FilesystemOutputTrace) {
		event, err := ProjectFilesystemOutput(value)
		observe(event, err)
	})
}
