package diagnostic

import appobs "github.com/windshare/windshare/engine/observation"

type FilesystemOutputCounters struct {
	NodeClaims             uint64
	DirectoryClaims        uint64
	FileClaims             uint64
	ActiveFileClaims       uint64
	ReservedFileSlots      uint64
	DirectoryMetadataBytes uint64
	CheckpointRecords      uint64
}

type FilesystemOutputSpec struct {
	Capabilities        FilesystemDestinationCapabilities
	Operation           FilesystemOutputOperation
	ReceiveIntent       appobs.ReceiveIntentDigest
	ReceiveOperation    appobs.ReceiveOperationID
	OutputSession       appobs.OutputSessionID
	Certification       FilesystemCertification
	NativeLockScope     FilesystemNativeLockScope
	NativeLockMilestone FilesystemNativeLockMilestone
	RootDisposition     FilesystemRootDisposition
	RuntimeComponent    FilesystemRuntimeComponent
	RuntimeOperation    FilesystemRuntimeOperation
	RuntimeDecision     FilesystemRuntimeDecisionKind
	CheckpointDecision  FilesystemCheckpointDecision
	OperationID         uint64
	ClaimID             uint64
	Counters            FilesystemOutputCounters
	Failure             appobs.Failure
	FailureStage        FilesystemFailureStage
	ReconciliationStep  FilesystemReconciliationStep
	NativeErrorClass    FilesystemNativeErrorClass
}

type FilesystemOutputObserved struct{ spec FilesystemOutputSpec }

func NewFilesystemOutputObserved(spec FilesystemOutputSpec) (FilesystemOutputObserved, error) {
	if !validFilesystemOutputSpec(spec) {
		return FilesystemOutputObserved{}, appobs.ErrInvalidEvent
	}
	return FilesystemOutputObserved{spec: spec}, nil
}

func validFilesystemOutputSpec(spec FilesystemOutputSpec) bool {
	return (spec.Capabilities == (FilesystemDestinationCapabilities{}) || spec.Capabilities.Valid()) &&
		validFilesystemOutputNames(spec) &&
		validFilesystemFailureContext(spec) &&
		(spec.NativeLockScope != 0) == (spec.NativeLockMilestone != 0) &&
		validFilesystemRuntimeDecision(spec) &&
		validFilesystemReconciliation(spec)
}

type filesystemNamedValue interface {
	Name() (string, bool)
}

type optionalFilesystemName struct {
	present bool
	value   filesystemNamedValue
}

func validFilesystemOutputNames(spec FilesystemOutputSpec) bool {
	if _, ok := spec.Operation.Name(); !ok {
		return false
	}
	optional := [...]optionalFilesystemName{
		{spec.Certification != 0, spec.Certification},
		{spec.NativeLockScope != 0, spec.NativeLockScope},
		{spec.NativeLockMilestone != 0, spec.NativeLockMilestone},
		{spec.RootDisposition != 0, spec.RootDisposition},
		{spec.RuntimeComponent != 0, spec.RuntimeComponent},
		{spec.RuntimeOperation != 0, spec.RuntimeOperation},
		{spec.RuntimeDecision != 0, spec.RuntimeDecision},
		{spec.CheckpointDecision != 0, spec.CheckpointDecision},
		{spec.FailureStage != 0, spec.FailureStage},
		{spec.ReconciliationStep != 0, spec.ReconciliationStep},
		{spec.NativeErrorClass != 0, spec.NativeErrorClass},
	}
	for _, field := range optional {
		if field.present && !validFilesystemName(field.value) {
			return false
		}
	}
	return true
}

func validFilesystemName(value filesystemNamedValue) bool {
	_, ok := value.Name()
	return ok
}

func validFilesystemRuntimeDecision(spec FilesystemOutputSpec) bool {
	runtimeFields := 0
	if spec.RuntimeComponent != 0 {
		runtimeFields++
	}
	if spec.RuntimeOperation != 0 {
		runtimeFields++
	}
	if spec.RuntimeDecision != 0 {
		runtimeFields++
	}
	return runtimeFields == 0 || runtimeFields == 3
}

func validFilesystemFailureContext(spec FilesystemOutputSpec) bool {
	if spec.FailureStage != 0 {
		return spec.Failure.Valid()
	}
	if spec.ReconciliationStep != 0 || spec.NativeErrorClass != 0 {
		return false
	}
	// Runtime decisions already identify the failing component and operation.
	// Requiring a native I/O stage would discard protocol/ownership failures or
	// force producers to misclassify them as a filesystem primitive failure.
	return !spec.Failure.Valid() || spec.Operation == FilesystemRuntimeDecision &&
		spec.RuntimeComponent != 0 && spec.RuntimeOperation != 0 && spec.RuntimeDecision != 0
}

func validFilesystemReconciliation(spec FilesystemOutputSpec) bool {
	return spec.ReconciliationStep == 0 ||
		spec.FailureStage == FilesystemFailureCheckpointReconciliation ||
		spec.FailureStage == FilesystemFailureNativeDurability
}

func (FilesystemOutputObserved) EngineEvent()      {}
func (FilesystemOutputObserved) OutputDiagnostic() {}

func (FilesystemOutputObserved) Workflow() appobs.Workflow { return appobs.WorkflowReceive }

func (FilesystemOutputObserved) Level() appobs.Level { return appobs.LevelDebug }

func (value FilesystemOutputObserved) ReceiveOperationID() (appobs.ReceiveOperationID, bool) {
	return value.spec.ReceiveOperation, value.spec.ReceiveOperation.Valid()
}

func (value FilesystemOutputObserved) ReceiveIntentDigest() (appobs.ReceiveIntentDigest, bool) {
	return value.spec.ReceiveIntent, value.spec.ReceiveIntent.Valid()
}

func (value FilesystemOutputObserved) OutputSessionID() (appobs.OutputSessionID, bool) {
	return value.spec.OutputSession, value.spec.OutputSession.Valid()
}

func (value FilesystemOutputObserved) DestinationCapabilities() (FilesystemDestinationCapabilities, bool) {
	return value.spec.Capabilities, value.spec.Capabilities.Valid()
}

func (value FilesystemOutputObserved) Operation() FilesystemOutputOperation {
	return value.spec.Operation
}

func (value FilesystemOutputObserved) Certification() (FilesystemCertification, bool) {
	_, ok := value.spec.Certification.Name()
	return value.spec.Certification, ok
}

func (value FilesystemOutputObserved) NativeLock() (FilesystemNativeLockScope, FilesystemNativeLockMilestone, bool) {
	_, a := value.spec.NativeLockScope.Name()
	_, b := value.spec.NativeLockMilestone.Name()
	return value.spec.NativeLockScope, value.spec.NativeLockMilestone, a && b
}

func (value FilesystemOutputObserved) RootDisposition() (FilesystemRootDisposition, bool) {
	_, ok := value.spec.RootDisposition.Name()
	return value.spec.RootDisposition, ok
}

func (value FilesystemOutputObserved) RuntimeDecision() (FilesystemRuntimeComponent, FilesystemRuntimeOperation, FilesystemRuntimeDecisionKind, bool) {
	_, a := value.spec.RuntimeComponent.Name()
	_, b := value.spec.RuntimeOperation.Name()
	_, c := value.spec.RuntimeDecision.Name()
	return value.spec.RuntimeComponent, value.spec.RuntimeOperation, value.spec.RuntimeDecision, a && b && c
}

func (value FilesystemOutputObserved) CheckpointDecision() (FilesystemCheckpointDecision, bool) {
	_, ok := value.spec.CheckpointDecision.Name()
	return value.spec.CheckpointDecision, ok
}

func (value FilesystemOutputObserved) Correlation() (uint64, uint64) {
	return value.spec.OperationID, value.spec.ClaimID
}

func (value FilesystemOutputObserved) Counters() FilesystemOutputCounters { return value.spec.Counters }

func (value FilesystemOutputObserved) Failure() (appobs.Failure, bool) {
	return value.spec.Failure, value.spec.Failure.Valid()
}

func (value FilesystemOutputObserved) FailureClassification() (FilesystemFailureStage, FilesystemReconciliationStep, FilesystemNativeErrorClass, bool) {
	return value.spec.FailureStage, value.spec.ReconciliationStep, value.spec.NativeErrorClass, value.spec.FailureStage != 0
}

func (value FilesystemOutputObserved) Validate() error {
	if !validFilesystemOutputSpec(value.spec) {
		return appobs.ErrInvalidEvent
	}
	return nil
}
