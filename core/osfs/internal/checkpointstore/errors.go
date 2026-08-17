package checkpointstore

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

// ErrorCode is the closed checkpoint boundary vocabulary consumed by fault
// normalization. Callers must not infer policy from wrapped filesystem errors.
type ErrorCode string

const (
	ErrorBusy              ErrorCode = "busy"
	ErrorCorruptRecord     ErrorCode = "corrupt-record"
	ErrorUnsafeInstall     ErrorCode = "unsafe-install"
	ErrorOwnershipMismatch ErrorCode = "ownership-mismatch"
	ErrorStateIO           ErrorCode = "state-io"
)

func (code ErrorCode) Valid() bool {
	switch code {
	case ErrorBusy, ErrorCorruptRecord, ErrorUnsafeInstall, ErrorOwnershipMismatch, ErrorStateIO:
		return true
	default:
		return false
	}
}

// ReconciliationStep is the last authoritative checkpoint-recovery decision
// reached before a candidate promotion failed. Cleanup is deliberately absent:
// closing capabilities cannot rewrite the primary recovery outcome.
type ReconciliationStep uint8

const (
	ReconciliationCandidateObservation ReconciliationStep = iota + 1
	ReconciliationStageDurability
	ReconciliationNamespaceDurability
	ReconciliationRecordPromotion
)

func (step ReconciliationStep) Valid() bool {
	return step >= ReconciliationCandidateObservation && step <= ReconciliationRecordPromotion
}

func (step ReconciliationStep) String() string {
	switch step {
	case ReconciliationCandidateObservation:
		return "candidate_observation"
	case ReconciliationStageDurability:
		return "stage_durability"
	case ReconciliationNamespaceDurability:
		return "namespace_durability"
	case ReconciliationRecordPromotion:
		return "record_promotion"
	default:
		return ""
	}
}

// ReconciliationError seals the safe recovery diagnosis while the provider's
// typed native classification is still reachable. The raw cause remains private
// diagnostic evidence and never authorizes a later policy decision.
type ReconciliationError struct {
	step      ReconciliationStep
	fault     transferfault.Fault
	native    outputcap.NativeErrorClass
	hasNative bool
	cause     error
}

func (failure *ReconciliationError) Error() string {
	if failure == nil || !failure.step.Valid() || !failure.fault.Valid() {
		return "checkpoint reconciliation failed"
	}
	return fmt.Sprintf("checkpoint reconciliation %s failed: %v", failure.step, failure.cause)
}

func (failure *ReconciliationError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

func (failure *ReconciliationError) Step() ReconciliationStep {
	if failure == nil {
		return 0
	}
	return failure.step
}

func (failure *ReconciliationError) Fault() transferfault.Fault {
	if failure == nil {
		return transferfault.Fault{}
	}
	return failure.fault
}

func (failure *ReconciliationError) NativeClass() (outputcap.NativeErrorClass, bool) {
	if failure == nil || !failure.hasNative || !failure.native.Valid() {
		return 0, false
	}
	return failure.native, true
}

func reconciliationError(step ReconciliationStep, cause error) error {
	if cause == nil {
		return nil
	}
	normalized := fileOutputBoundaryErrorWithoutContext(transferfault.ScopeOutputPause, cause)
	result := transferfault.NormalizeBoundaryError(normalized)
	value, ok := result.Fault()
	if !step.Valid() || !ok {
		value = transferfault.DependencyContractFault()
	}
	native, hasNative := outputcap.ClassifyNativeError(cause)
	return &ReconciliationError{
		step: step, fault: value, native: native, hasNative: hasNative, cause: cause,
	}
}

// Error retains a raw cause only for immediate diagnostics. Its code is the sole
// authority that may cross into settlement policy.
type Error struct {
	code      ErrorCode
	operation string
	cause     error
}

func (err *Error) Error() string {
	if err == nil {
		return "<nil>"
	}
	if err.cause == nil {
		return fmt.Sprintf("checkpoint %s: %s", err.operation, err.code)
	}
	return fmt.Sprintf("checkpoint %s: %s: %v", err.operation, err.code, err.cause)
}

func (err *Error) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func (err *Error) Code() ErrorCode {
	if err == nil {
		return ""
	}
	return err.code
}

func (err *Error) Operation() string {
	if err == nil {
		return ""
	}
	return err.operation
}

func repositoryError(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(cause, context.Canceled) {
		return context.Canceled
	}
	if existing, ok := admitExactError[*transferfault.BoundaryError](cause); ok &&
		existing != nil && existing.Fault().Valid() {
		return existing
	}
	if existing, ok := admitExactError[*Error](cause); ok && existing != nil && existing.Code().Valid() {
		return checkpointBoundaryError(existing.Code(), existing.Operation(), existing.cause)
	}
	return checkpointBoundaryError(classifyError(cause), operation, cause)
}

func codedError(code ErrorCode, operation string, cause error) error {
	if cause == nil {
		return nil
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(cause, context.Canceled) {
		return context.Canceled
	}
	if existing, ok := admitExactError[*transferfault.BoundaryError](cause); ok &&
		existing != nil && existing.Fault().Valid() {
		return existing
	}
	return checkpointBoundaryError(code, operation, cause)
}

func checkpointBoundaryError(code ErrorCode, operation string, cause error) error {
	native := &Error{code: code, operation: operation, cause: cause}
	var value transferfault.Fault
	switch code {
	case ErrorBusy:
		value, _ = transferfault.NewCheckpoint(transferfault.ScopeOutputPause, transferfault.CheckpointBusy)
	case ErrorCorruptRecord:
		value, _ = transferfault.NewCheckpoint(transferfault.ScopeOutputPause, transferfault.CheckpointCorruptRecord)
	case ErrorUnsafeInstall:
		value, _ = transferfault.NewCheckpoint(transferfault.ScopeOutputPause, transferfault.CheckpointUnsafeInstall)
	case ErrorOwnershipMismatch:
		value, _ = transferfault.NewCheckpoint(transferfault.ScopeOutputPause, transferfault.CheckpointOwnershipMismatch)
	case ErrorStateIO:
		value, _ = transferfault.NewCheckpoint(transferfault.ScopeOutputPause, transferfault.CheckpointStateIO)
	default:
		value = transferfault.DependencyContractFault()
	}
	return transferfault.Wrap(value, native)
}

func dependencyBoundaryError(operation string, cause error) error {
	return transferfault.Wrap(
		transferfault.DependencyContractFault(),
		fmt.Errorf("checkpoint %s: %w", operation, cause),
	)
}

func fileOutputBoundaryError(ctx context.Context, scope transferfault.Scope, cause error) error {
	if ctx == nil {
		return dependencyBoundaryError("normalize file output failure", transfer.ErrInvalidOutputBinding)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fileOutputBoundaryErrorWithoutContext(scope, cause)
}

func fileOutputBoundaryErrorWithoutContext(scope transferfault.Scope, cause error) error {
	if cause == nil {
		return nil
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(cause, context.Canceled) {
		return context.Canceled
	}
	if existing, ok := admitExactError[*transferfault.BoundaryError](cause); ok &&
		existing != nil && existing.Fault().Valid() {
		return existing
	}
	var value transferfault.Fault
	switch {
	case errors.Is(cause, transfer.ErrInvalidOutputBinding),
		errors.Is(cause, transfer.ErrInvalidOutputSettlement):
		value = transferfault.DependencyContractFault()
	case errors.Is(cause, outputcap.ErrRecoverableOutputUnsupported):
		value, _ = transferfault.NewOutput(transferfault.ScopeOutputPause, transferfault.OutputUnsupportedFilesystem)
	case errors.Is(cause, outputcap.ErrUnsafeNamespace), errors.Is(cause, outputcap.ErrNamespaceCollision):
		value, _ = transferfault.NewOutput(transferfault.ScopeOutputPause, transferfault.OutputNamespaceUnsafe)
	case errors.Is(cause, outputcap.ErrNamespaceLockBusy):
		value, _ = transferfault.NewOutput(transferfault.ScopeOutputPause, transferfault.OutputOwnership)
	default:
		value, _ = transferfault.NewOutput(scope, transferfault.OutputStateIO)
	}
	return transferfault.Wrap(value, cause)
}

// admitExactError uses errors.As only after the direct dynamic type has been
// admitted. Repository and boundary wrappers are authority-bearing values, so
// an arbitrary outer wrapper must not acquire their closed code or fault.
func admitExactError[T error](cause error) (T, bool) {
	var admitted T
	if cause == nil || reflect.TypeOf(cause) != reflect.TypeFor[T]() {
		return admitted, false
	}
	if !errors.As(cause, &admitted) {
		return admitted, false
	}
	return admitted, true
}

func classifyError(err error) ErrorCode {
	switch {
	case candidateContention(err):
		return ErrorBusy
	case errors.Is(err, checkpointmodel.ErrInvalidOwnership),
		errors.Is(err, checkpointmodel.ErrOwnershipChecksum),
		errors.Is(err, checkpointmodel.ErrOwnershipNonCanonical):
		return ErrorOwnershipMismatch
	case errors.Is(err, checkpointmodel.ErrInvalidRecord),
		errors.Is(err, checkpointmodel.ErrRecordChecksum),
		errors.Is(err, checkpointmodel.ErrRecordNonCanonical):
		return ErrorCorruptRecord
	case errors.Is(err, outputcap.ErrUnsafeNamespace),
		errors.Is(err, checkpointmodel.ErrRecordBinding),
		errors.Is(err, checkpointmodel.ErrRecordGeneration),
		errors.Is(err, checkpointmodel.ErrRecordRecovery),
		errors.Is(err, checkpointmodel.ErrRecordCrashBoundary):
		return ErrorUnsafeInstall
	default:
		return ErrorStateIO
	}
}

// ErrorCodeFor projects raw provider failures into checkpointstore's closed
// storage vocabulary. Resume authority maps this value into policy without
// inspecting platform-specific error strings.
func ErrorCodeFor(err error) ErrorCode {
	return classifyError(err)
}
