package outputruntime

import (
	"context"
	"errors"
	"fmt"

	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

var ErrShareStopped = errors.New("retained output belongs to a stopped share")

type FailureStage uint8

const (
	FailureDestinationBinding FailureStage = iota + 1
	FailureInventory
	FailureLookup
	FailureOpen
	FailureCreate
	FailureCheckpoint
	FailureDurability
	FailureClose
)

func (stage FailureStage) Valid() bool {
	return stage >= FailureDestinationBinding && stage <= FailureClose
}

func (stage FailureStage) String() string {
	switch stage {
	case FailureDestinationBinding:
		return "destination_binding"
	case FailureInventory:
		return "inventory"
	case FailureLookup:
		return "lookup"
	case FailureOpen:
		return "open"
	case FailureCreate:
		return "create"
	case FailureCheckpoint:
		return "checkpoint"
	case FailureDurability:
		return "durability"
	case FailureClose:
		return "close"
	default:
		return ""
	}
}

type AttentionReason string

const (
	AttentionNone        AttentionReason = ""
	AttentionDestination AttentionReason = "destination-ownership-unknown"
	AttentionRegistry    AttentionReason = "registry-ownership-unknown"
	AttentionLease       AttentionReason = "lease-ownership-unknown"
	AttentionOperation   AttentionReason = "operation-ownership-unknown"
	AttentionCleanup     AttentionReason = "cleanup-uncertain"
)

func (reason AttentionReason) Valid() bool {
	switch reason {
	case AttentionDestination, AttentionRegistry, AttentionLease, AttentionOperation, AttentionCleanup:
		return true
	default:
		return false
	}
}

type CheckpointReconciliationStep uint8

const (
	CheckpointCandidateObservation CheckpointReconciliationStep = iota + 1
	CheckpointStageDurability
	CheckpointNamespaceDurability
	CheckpointRecordPromotion
)

func (step CheckpointReconciliationStep) Valid() bool {
	return step >= CheckpointCandidateObservation && step <= CheckpointRecordPromotion
}

func (step CheckpointReconciliationStep) String() string {
	switch step {
	case CheckpointCandidateObservation:
		return "candidate_observation"
	case CheckpointStageDurability:
		return "stage_durability"
	case CheckpointNamespaceDurability:
		return "namespace_durability"
	case CheckpointRecordPromotion:
		return "record_promotion"
	default:
		return ""
	}
}

type NativeErrorClass = outputcap.NativeErrorClass

// Diagnostic is intentionally value-only. Provider errors stay behind the
// runtime boundary while callers receive enough closed context to distinguish
// durable state from a failed recovery attempt.
type Diagnostic struct {
	Stage              FailureStage
	ReconciliationStep CheckpointReconciliationStep
	NativeErrorClass   NativeErrorClass
	FaultDomain        uint8
	NormalizedScope    uint8
	NormalizedCode     uint16
}

func (diagnostic Diagnostic) Valid() bool {
	if !diagnostic.Stage.Valid() {
		return false
	}
	if diagnostic.ReconciliationStep != 0 {
		if !diagnostic.ReconciliationStep.Valid() ||
			diagnostic.Stage != FailureCheckpoint &&
				diagnostic.Stage != FailureDurability {
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

type diagnosticError struct {
	diagnostic Diagnostic
	cause      error
}

func (failure *diagnosticError) Error() string {
	if failure == nil || !failure.diagnostic.Valid() {
		return "filesystem output failed"
	}
	return fmt.Sprintf("filesystem output failed at %s", failure.diagnostic.Stage)
}

func (failure *diagnosticError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

func DiagnosticFor(err error) (Diagnostic, bool) {
	failure, ok := errors.AsType[*diagnosticError](err)
	if !ok || failure == nil {
		return Diagnostic{}, false
	}
	return failure.diagnostic, failure.diagnostic.Valid()
}

const (
	maximumFailureDepth = 64
	maximumFailureNodes = 1024
)

// FailureForStage extracts causes owned by one failure stage from an error tree.
// A FailureClose boundary claims every cause inside it so release failures stay
// separate from earlier operation failures.
func FailureForStage(cause error, stage FailureStage) error {
	if cause == nil || !stage.Valid() {
		return nil
	}
	budget := maximumFailureNodes
	return failureForStage(cause, stage, 0, &budget)
}

func failureForStage(cause error, stage FailureStage, depth int, budget *int) error {
	if cause == nil || depth >= maximumFailureDepth || *budget == 0 {
		return nil
	}
	*budget--
	//nolint:errorlint
	if annotated, ok := cause.(*diagnosticError); ok && annotated != nil && annotated.diagnostic.Valid() {
		if annotated.diagnostic.Stage == stage {
			return annotated
		}
		if annotated.diagnostic.Stage == FailureClose {
			// Release boundaries own every error inside them; non-close queries must not pierce them.
			return nil
		}
	}
	//nolint:errorlint
	switch value := cause.(type) {
	case interface{ Unwrap() []error }:
		var selected []error
		for _, child := range value.Unwrap() {
			if match := failureForStage(child, stage, depth+1, budget); match != nil {
				selected = append(selected, match)
			}
		}
		return errors.Join(selected...)
	case interface{ Unwrap() error }:
		return failureForStage(value.Unwrap(), stage, depth+1, budget)
	default:
		return nil
	}
}

func DiagnoseFailure(stage FailureStage, cause error) error {
	if cause == nil {
		return nil
	}
	if stage == FailureClose {
		// A release failure is independent of the operation that initiated
		// shutdown. Keep this boundary even when its cause has earlier evidence
		// or includes cancellation alongside another close failure.
		diagnostic := buildDiagnostic(stage, cause)
		diagnostic.Stage = stage
		diagnostic.ReconciliationStep = 0
		return &diagnosticError{diagnostic: diagnostic, cause: cause}
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	if _, existing := DiagnosticFor(cause); existing {
		return cause
	}
	diagnostic := buildDiagnostic(stage, cause)
	return &diagnosticError{diagnostic: diagnostic, cause: cause}
}

func freezeFailure(primary, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	cleanup = DiagnoseFailure(FailureClose, cleanup)
	if primary == nil {
		return cleanup
	}
	return errors.Join(primary, cleanup)
}

func buildDiagnostic(stage FailureStage, cause error) Diagnostic {
	diagnostic := Diagnostic{Stage: stage}
	if reconciliation, ok := errors.AsType[*checkpointstore.ReconciliationError](cause); ok {
		diagnostic.ReconciliationStep = reconciliationStepFromStore(reconciliation.Step())
		if reconciliation.Step() == checkpointstore.ReconciliationStageDurability ||
			reconciliation.Step() == checkpointstore.ReconciliationNamespaceDurability {
			diagnostic.Stage = FailureDurability
		}
		if native, ok := reconciliation.NativeClass(); ok {
			diagnostic.NativeErrorClass = native
		}
		applyDiagnosticFault(&diagnostic, reconciliation.Fault())
		return diagnostic
	}
	if native, ok := outputcap.ClassifyNativeError(cause); ok {
		diagnostic.NativeErrorClass = native
	}
	if value, ok := transferfault.NormalizeBoundaryError(cause).Fault(); ok {
		applyDiagnosticFault(&diagnostic, value)
	}
	return diagnostic
}

func applyDiagnosticFault(diagnostic *Diagnostic, value transferfault.Fault) {
	if diagnostic == nil || !value.Valid() {
		return
	}
	diagnostic.FaultDomain = uint8(value.Domain())
	diagnostic.NormalizedScope = uint8(value.Scope())
	diagnostic.NormalizedCode = value.Code()
}

func reconciliationStepFromStore(step checkpointstore.ReconciliationStep) CheckpointReconciliationStep {
	switch step {
	case checkpointstore.ReconciliationCandidateObservation:
		return CheckpointCandidateObservation
	case checkpointstore.ReconciliationStageDurability:
		return CheckpointStageDurability
	case checkpointstore.ReconciliationNamespaceDurability:
		return CheckpointNamespaceDurability
	case checkpointstore.ReconciliationRecordPromotion:
		return CheckpointRecordPromotion
	default:
		return 0
	}
}

func attentionReasonFromClosed(reason checkpointmodel.OrdinaryClosedReason) AttentionReason {
	switch reason {
	case checkpointmodel.OrdinaryReasonNone:
		return AttentionNone
	case checkpointmodel.OrdinaryReasonDestinationOwnershipUnknown:
		return AttentionDestination
	case checkpointmodel.OrdinaryReasonRegistryOwnershipUnknown:
		return AttentionRegistry
	case checkpointmodel.OrdinaryReasonLeaseOwnershipUnknown:
		return AttentionLease
	case checkpointmodel.OrdinaryReasonOperationOwnershipUnknown:
		return AttentionOperation
	case checkpointmodel.OrdinaryReasonCleanupUncertain:
		return AttentionCleanup
	default:
		return ""
	}
}
