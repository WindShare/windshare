// Package output owns native resumable receive output, task locking, journal
// persistence, compatible-name mapping, and recovery inventory.
package output

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

var (
	ErrOutOfRange         = errors.New("osfs: range write is out of bounds or overlaps")
	ErrPathEscape         = errors.New("osfs: path escapes output root")
	ErrShareStopped       = errors.New("retained output belongs to a stopped share")
	ErrBusy               = errors.New("retained output operation is currently active")
	ErrAlreadyRunning     = errors.New("matching output operation is already running")
	ErrAmbiguous          = errors.New("matching output operation ownership is ambiguous")
	ErrOperationChanged   = errors.New("retained output operation no longer matches")
	ErrInvalidContract    = errors.New("native recovery contract is invalid")
	ErrUnsafeNamespace    = errors.New("osfs: output namespace is unsafe")
	ErrNamespaceCollision = errors.New("osfs: output namespace entry already exists")
	ErrNameRejected       = errors.New("osfs: destination rejects the output name")
	ErrNamespaceLockBusy  = errors.New("osfs: output namespace lock is already held")
	ErrUnsupportedOutput  = errors.New("osfs: recoverable native output is unsupported by the destination")
)

// AttentionError preserves the attention reason when CreateOperation re-checks
// an automatic selection and encounters a damaged existing operation.
type AttentionError struct {
	Reason AttentionReason
}

func (err *AttentionError) Error() string {
	return "retained output needs attention: " + string(err.Reason)
}

func isAdmissionOutcomeError(err error) bool {
	if errors.Is(err, ErrShareStopped) || errors.Is(err, ErrAlreadyRunning) || errors.Is(err, ErrAmbiguous) {
		return true
	}
	_, ok := errors.AsType[*AttentionError](err)
	return ok
}

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

type NativeErrorClass uint8

const (
	NativeErrorAccessDenied NativeErrorClass = iota + 1
	NativeErrorSharingViolation
	NativeErrorNotFound
	NativeErrorInvalidHandle
	NativeErrorUnsupported
	NativeErrorIO
	NativeErrorUnknown
)

func (class NativeErrorClass) Valid() bool {
	return class >= NativeErrorAccessDenied && class <= NativeErrorUnknown
}

func (class NativeErrorClass) String() string {
	switch class {
	case NativeErrorAccessDenied:
		return "access_denied"
	case NativeErrorSharingViolation:
		return "sharing_violation"
	case NativeErrorNotFound:
		return "not_found"
	case NativeErrorInvalidHandle:
		return "invalid_handle"
	case NativeErrorUnsupported:
		return "unsupported"
	case NativeErrorIO:
		return "io"
	case NativeErrorUnknown:
		return "unknown"
	default:
		return ""
	}
}

// ClassifyNativeError maps an underlying OS error into a closed diagnostic class
// so higher layers can present bounded guidance without leaking host paths.
func ClassifyNativeError(err error) (NativeErrorClass, bool) {
	if err == nil {
		return 0, false
	}
	if class, ok := classifyPlatformError(err); ok {
		return class, true
	}
	switch {
	case errors.Is(err, fs.ErrPermission):
		return NativeErrorAccessDenied, true
	case errors.Is(err, fs.ErrNotExist):
		return NativeErrorNotFound, true
	case errors.Is(err, ErrUnsupportedOutput):
		return NativeErrorUnsupported, true
	default:
		return 0, false
	}
}

// Diagnostic is value-only native failure evidence extracted at adapter edges.
type Diagnostic struct {
	Stage            FailureStage
	NativeErrorClass NativeErrorClass
	Fault            transferfault.Fault
}

func (diagnostic Diagnostic) Valid() bool {
	if !diagnostic.Stage.Valid() {
		return false
	}
	if diagnostic.NativeErrorClass != 0 && !diagnostic.NativeErrorClass.Valid() {
		return false
	}
	return diagnostic.Fault.IsZero() || diagnostic.Fault.Valid()
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
		diagnostic := buildDiagnostic(stage, cause)
		diagnostic.Stage = stage
		return &diagnosticError{diagnostic: diagnostic, cause: cause}
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	if _, existing := DiagnosticFor(cause); existing {
		return cause
	}
	return &diagnosticError{diagnostic: buildDiagnostic(stage, cause), cause: cause}
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
	if native, ok := ClassifyNativeError(cause); ok {
		diagnostic.NativeErrorClass = native
	}
	if value, ok := transferfault.NormalizeBoundaryError(cause).Fault(); ok {
		diagnostic.Fault = value
	}
	return diagnostic
}

func wrapOutputFault(ctx context.Context, code transferfault.OutputCode, op string, cause error) error {
	if cause == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	value, _ := transferfault.NewOutput(transferfault.ScopeOutputPause, code)
	return transferfault.Wrap(value, fmt.Errorf("%s: %w", op, cause))
}

func wrapFileFault(ctx context.Context, code transferfault.OutputCode, op string, cause error) error {
	if cause == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	value, _ := transferfault.NewOutput(transferfault.ScopeFileLocal, code)
	return transferfault.Wrap(value, fmt.Errorf("%s: %w", op, cause))
}
