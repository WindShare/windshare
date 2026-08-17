package outputruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

type FilesystemOutputFailureStage uint8

const (
	FilesystemOutputFailureDestinationBinding FilesystemOutputFailureStage = iota + 1
	FilesystemOutputFailureInventoryPaging
	FilesystemOutputFailureActiveLookup
	FilesystemOutputFailureOperationAcquisition
	FilesystemOutputFailureOperationAdmission
	FilesystemOutputFailureCheckpointReconciliation
	FilesystemOutputFailureNativeDurability
	FilesystemOutputFailureAuthorityClose
)

func (stage FilesystemOutputFailureStage) Valid() bool {
	return stage >= FilesystemOutputFailureDestinationBinding &&
		stage <= FilesystemOutputFailureAuthorityClose
}

func (stage FilesystemOutputFailureStage) String() string {
	switch stage {
	case FilesystemOutputFailureDestinationBinding:
		return "destination_binding"
	case FilesystemOutputFailureInventoryPaging:
		return "inventory_paging"
	case FilesystemOutputFailureActiveLookup:
		return "active_lookup"
	case FilesystemOutputFailureOperationAcquisition:
		return "operation_acquisition"
	case FilesystemOutputFailureOperationAdmission:
		return "operation_admission"
	case FilesystemOutputFailureCheckpointReconciliation:
		return "checkpoint_reconciliation"
	case FilesystemOutputFailureNativeDurability:
		return "native_durability"
	case FilesystemOutputFailureAuthorityClose:
		return "authority_close"
	default:
		return ""
	}
}

type FilesystemCheckpointReconciliationStep uint8

const (
	FilesystemCheckpointCandidateObservation FilesystemCheckpointReconciliationStep = iota + 1
	FilesystemCheckpointStageDurability
	FilesystemCheckpointNamespaceDurability
	FilesystemCheckpointRecordPromotion
)

func (step FilesystemCheckpointReconciliationStep) Valid() bool {
	return step >= FilesystemCheckpointCandidateObservation &&
		step <= FilesystemCheckpointRecordPromotion
}

func (step FilesystemCheckpointReconciliationStep) String() string {
	switch step {
	case FilesystemCheckpointCandidateObservation:
		return "candidate_observation"
	case FilesystemCheckpointStageDurability:
		return "stage_durability"
	case FilesystemCheckpointNamespaceDurability:
		return "namespace_durability"
	case FilesystemCheckpointRecordPromotion:
		return "record_promotion"
	default:
		return ""
	}
}

type FilesystemNativeErrorClass uint8

const (
	FilesystemNativeErrorAccessDenied FilesystemNativeErrorClass = iota + 1
	FilesystemNativeErrorSharingViolation
	FilesystemNativeErrorNotFound
	FilesystemNativeErrorInvalidHandle
	FilesystemNativeErrorUnsupported
	FilesystemNativeErrorIO
	FilesystemNativeErrorUnknown
)

func (class FilesystemNativeErrorClass) Valid() bool {
	return class >= FilesystemNativeErrorAccessDenied && class <= FilesystemNativeErrorUnknown
}

func (class FilesystemNativeErrorClass) String() string {
	switch class {
	case FilesystemNativeErrorAccessDenied:
		return "access_denied"
	case FilesystemNativeErrorSharingViolation:
		return "sharing_violation"
	case FilesystemNativeErrorNotFound:
		return "not_found"
	case FilesystemNativeErrorInvalidHandle:
		return "invalid_handle"
	case FilesystemNativeErrorUnsupported:
		return "unsupported"
	case FilesystemNativeErrorIO:
		return "io"
	case FilesystemNativeErrorUnknown:
		return "unknown"
	default:
		return ""
	}
}

type FilesystemOutputStateReason uint8

const (
	FilesystemOutputStateReasonNone FilesystemOutputStateReason = iota + 1
	FilesystemOutputStateDestinationOwnershipUnknown
	FilesystemOutputStateRegistryOwnershipUnknown
	FilesystemOutputStateLeaseOwnershipUnknown
	FilesystemOutputStateOperationOwnershipUnknown
	FilesystemOutputStateCleanupUncertain
)

func (reason FilesystemOutputStateReason) Valid() bool {
	return reason >= FilesystemOutputStateReasonNone &&
		reason <= FilesystemOutputStateCleanupUncertain
}

func (reason FilesystemOutputStateReason) String() string {
	switch reason {
	case FilesystemOutputStateReasonNone:
		return "none"
	case FilesystemOutputStateDestinationOwnershipUnknown:
		return "destination_ownership_unknown"
	case FilesystemOutputStateRegistryOwnershipUnknown:
		return "registry_ownership_unknown"
	case FilesystemOutputStateLeaseOwnershipUnknown:
		return "lease_ownership_unknown"
	case FilesystemOutputStateOperationOwnershipUnknown:
		return "operation_ownership_unknown"
	case FilesystemOutputStateCleanupUncertain:
		return "cleanup_uncertain"
	default:
		return ""
	}
}

// FilesystemOutputDiagnostic is intentionally value-only. Provider errors stay
// behind the runtime boundary while callers receive enough closed context to
// distinguish durable state from a failed recovery attempt.
type FilesystemOutputDiagnostic struct {
	Stage              FilesystemOutputFailureStage
	ReconciliationStep FilesystemCheckpointReconciliationStep
	NativeErrorClass   FilesystemNativeErrorClass
	FaultDomain        uint8
	NormalizedScope    uint8
	NormalizedCode     uint16
}

func (diagnostic FilesystemOutputDiagnostic) Valid() bool {
	if !diagnostic.Stage.Valid() {
		return false
	}
	if diagnostic.ReconciliationStep != 0 {
		if !diagnostic.ReconciliationStep.Valid() ||
			diagnostic.Stage != FilesystemOutputFailureCheckpointReconciliation &&
				diagnostic.Stage != FilesystemOutputFailureNativeDurability {
			return false
		}
	}
	if diagnostic.NativeErrorClass != 0 && !diagnostic.NativeErrorClass.Valid() {
		return false
	}
	faultZero := diagnostic.FaultDomain == 0 &&
		diagnostic.NormalizedScope == 0 &&
		diagnostic.NormalizedCode == 0
	faultComplete := diagnostic.FaultDomain != 0 &&
		diagnostic.NormalizedScope != 0 &&
		diagnostic.NormalizedCode != 0
	return faultZero || faultComplete
}

type filesystemOutputDiagnosticError struct {
	diagnostic FilesystemOutputDiagnostic
	cause      error
}

func (failure *filesystemOutputDiagnosticError) Error() string {
	if failure == nil || !failure.diagnostic.Valid() {
		return "filesystem output failed"
	}
	return fmt.Sprintf("filesystem output failed at %s", failure.diagnostic.Stage)
}

func (failure *filesystemOutputDiagnosticError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

func (failure *filesystemOutputDiagnosticError) FilesystemOutputDiagnostic() FilesystemOutputDiagnostic {
	if failure == nil {
		return FilesystemOutputDiagnostic{}
	}
	return failure.diagnostic
}

type filesystemOutputDiagnosticCarrier interface {
	FilesystemOutputDiagnostic() FilesystemOutputDiagnostic
}

func FilesystemOutputDiagnosticFor(err error) (FilesystemOutputDiagnostic, bool) {
	var carrier filesystemOutputDiagnosticCarrier
	if !errors.As(err, &carrier) {
		return FilesystemOutputDiagnostic{}, false
	}
	diagnostic := carrier.FilesystemOutputDiagnostic()
	return diagnostic, diagnostic.Valid()
}

func DiagnoseFilesystemOutputFailure(stage FilesystemOutputFailureStage, cause error) error {
	return diagnoseFilesystemOutputFailure(stage, cause)
}

func diagnoseFilesystemOutputFailure(stage FilesystemOutputFailureStage, cause error) error {
	if cause == nil || errors.Is(cause, context.Canceled) ||
		errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	if existing, ok := FilesystemOutputDiagnosticFor(cause); ok {
		return &filesystemOutputDiagnosticError{diagnostic: existing, cause: cause}
	}
	diagnostic := filesystemOutputDiagnostic(stage, cause)
	return &filesystemOutputDiagnosticError{diagnostic: diagnostic, cause: cause}
}

func freezeFilesystemOutputFailure(primary, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	if primary == nil {
		return diagnoseFilesystemOutputFailure(FilesystemOutputFailureAuthorityClose, cleanup)
	}
	if diagnostic, ok := FilesystemOutputDiagnosticFor(primary); ok {
		return &filesystemOutputDiagnosticError{
			diagnostic: diagnostic,
			cause:      errors.Join(primary, cleanup),
		}
	}
	return errors.Join(primary, cleanup)
}

func filesystemOutputDiagnostic(
	stage FilesystemOutputFailureStage,
	cause error,
) FilesystemOutputDiagnostic {
	diagnostic := FilesystemOutputDiagnostic{Stage: stage}
	var reconciliation *checkpointstore.ReconciliationError
	if errors.As(cause, &reconciliation) {
		diagnostic.ReconciliationStep = filesystemReconciliationStep(reconciliation.Step())
		if reconciliation.Step() == checkpointstore.ReconciliationStageDurability ||
			reconciliation.Step() == checkpointstore.ReconciliationNamespaceDurability {
			diagnostic.Stage = FilesystemOutputFailureNativeDurability
		}
		if native, ok := reconciliation.NativeClass(); ok {
			diagnostic.NativeErrorClass = filesystemNativeErrorClass(native)
		}
		applyFilesystemDiagnosticFault(&diagnostic, reconciliation.Fault())
		return diagnostic
	}
	if native, ok := outputcap.ClassifyNativeError(cause); ok {
		diagnostic.NativeErrorClass = filesystemNativeErrorClass(native)
	}
	if value, ok := transferfault.NormalizeBoundaryError(cause).Fault(); ok {
		applyFilesystemDiagnosticFault(&diagnostic, value)
	}
	return diagnostic
}

func applyFilesystemDiagnosticFault(
	diagnostic *FilesystemOutputDiagnostic,
	value transferfault.Fault,
) {
	if diagnostic == nil || !value.Valid() {
		return
	}
	diagnostic.FaultDomain = uint8(value.Domain())
	diagnostic.NormalizedScope = uint8(value.Scope())
	diagnostic.NormalizedCode = value.Code()
}

func filesystemReconciliationStep(
	step checkpointstore.ReconciliationStep,
) FilesystemCheckpointReconciliationStep {
	switch step {
	case checkpointstore.ReconciliationCandidateObservation:
		return FilesystemCheckpointCandidateObservation
	case checkpointstore.ReconciliationStageDurability:
		return FilesystemCheckpointStageDurability
	case checkpointstore.ReconciliationNamespaceDurability:
		return FilesystemCheckpointNamespaceDurability
	case checkpointstore.ReconciliationRecordPromotion:
		return FilesystemCheckpointRecordPromotion
	default:
		return 0
	}
}

func filesystemNativeErrorClass(class outputcap.NativeErrorClass) FilesystemNativeErrorClass {
	switch class {
	case outputcap.NativeErrorAccessDenied:
		return FilesystemNativeErrorAccessDenied
	case outputcap.NativeErrorSharingViolation:
		return FilesystemNativeErrorSharingViolation
	case outputcap.NativeErrorNotFound:
		return FilesystemNativeErrorNotFound
	case outputcap.NativeErrorInvalidHandle:
		return FilesystemNativeErrorInvalidHandle
	case outputcap.NativeErrorUnsupported:
		return FilesystemNativeErrorUnsupported
	case outputcap.NativeErrorIO:
		return FilesystemNativeErrorIO
	case outputcap.NativeErrorUnknown:
		return FilesystemNativeErrorUnknown
	default:
		return 0
	}
}

func filesystemOutputStateReason(
	reason checkpointmodel.OrdinaryClosedReason,
) FilesystemOutputStateReason {
	switch reason {
	case checkpointmodel.OrdinaryReasonNone:
		return FilesystemOutputStateReasonNone
	case checkpointmodel.OrdinaryReasonDestinationOwnershipUnknown:
		return FilesystemOutputStateDestinationOwnershipUnknown
	case checkpointmodel.OrdinaryReasonRegistryOwnershipUnknown:
		return FilesystemOutputStateRegistryOwnershipUnknown
	case checkpointmodel.OrdinaryReasonLeaseOwnershipUnknown:
		return FilesystemOutputStateLeaseOwnershipUnknown
	case checkpointmodel.OrdinaryReasonOperationOwnershipUnknown:
		return FilesystemOutputStateOperationOwnershipUnknown
	case checkpointmodel.OrdinaryReasonCleanupUncertain:
		return FilesystemOutputStateCleanupUncertain
	default:
		return 0
	}
}
