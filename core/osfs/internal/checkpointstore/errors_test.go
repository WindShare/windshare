package checkpointstore

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func TestRepositoryErrorCodesAreClosedAndDeterministic(t *testing.T) {
	for _, code := range []ErrorCode{
		ErrorBusy, ErrorCorruptRecord, ErrorUnsafeInstall, ErrorOwnershipMismatch, ErrorStateIO,
	} {
		if !code.Valid() {
			t.Fatalf("declared repository code %q is invalid", code)
		}
	}
	for _, code := range []ErrorCode{"", "future-code"} {
		if code.Valid() {
			t.Fatalf("unknown repository code %q is valid", code)
		}
	}

	stateFailure := errors.New("state failure")
	for name, test := range map[string]struct {
		cause error
		want  ErrorCode
	}{
		"busy":      {cause: outputcap.ErrNamespaceLockBusy, want: ErrorBusy},
		"ownership": {cause: checkpointmodel.ErrOwnershipChecksum, want: ErrorOwnershipMismatch},
		"corrupt":   {cause: checkpointmodel.ErrRecordChecksum, want: ErrorCorruptRecord},
		"unsafe":    {cause: outputcap.ErrUnsafeNamespace, want: ErrorUnsafeInstall},
		"state I/O": {cause: stateFailure, want: ErrorStateIO},
	} {
		t.Run(name, func(t *testing.T) {
			err := repositoryError("test operation", test.cause)
			var repositoryErr *Error
			if !errors.As(err, &repositoryErr) || repositoryErr.Code() != test.want ||
				repositoryErr.Operation() != "test operation" || !errors.Is(err, test.cause) {
				t.Fatalf("repository error = %#v, want code %q", err, test.want)
			}
			if repositoryErr.Error() == "" || repositoryErr.Unwrap() == nil {
				t.Fatal("repository error lost its immediate diagnostic cause")
			}
		})
	}
}

func TestRepositoryErrorHelpersPreserveExistingBoundariesAndNil(t *testing.T) {
	existing := codedError(ErrorCorruptRecord, "decode", checkpointmodel.ErrInvalidRecord)
	if got := repositoryError("outer", existing); got != existing {
		t.Fatal("repository boundary wrapped an already normalized error")
	}
	invalidCause := errors.New("invalid code cause")
	invalid := codedError("future", "invalid", invalidCause)
	var boundary *transferfault.BoundaryError
	if repositoryError("nil", nil) != nil || !errors.As(invalid, &boundary) ||
		boundary.Fault() != transferfault.DependencyContractFault() || !errors.Is(invalid, invalidCause) {
		t.Fatal("invalid checkpoint code did not fail closed at its boundary")
	}
	if codedError(ErrorBusy, "nil", nil) != nil {
		t.Fatal("coded error manufactured a failure without a cause")
	}
	var repositoryErr *Error
	if repositoryErr.Code() != "" || repositoryErr.Operation() != "" || repositoryErr.Unwrap() != nil ||
		repositoryErr.Error() != "<nil>" {
		t.Fatal("nil repository error accessors are not stable")
	}
	withoutCause := &Error{code: ErrorStateIO, operation: "close"}
	if withoutCause.Error() == "" || withoutCause.Unwrap() != nil {
		t.Fatal("repository error without a cause is not stable")
	}
}

func TestAdmitExactErrorPopulatesDirectBoundaryAndRejectsWrapper(t *testing.T) {
	value, err := transferfault.NewOutput(
		transferfault.ScopeOutputPause,
		transferfault.OutputOwnership,
	)
	if err != nil {
		t.Fatal(err)
	}
	direct := transferfault.Wrap(value, errors.New("diagnostic"))
	admitted, ok := admitExactError[*transferfault.BoundaryError](direct)
	if !ok || admitted == nil || admitted.Fault() != value {
		t.Fatalf("direct boundary admission = %#v, %t", admitted, ok)
	}

	wrapped := fmt.Errorf("untrusted adapter wrapper: %w", direct)
	admitted, ok = admitExactError[*transferfault.BoundaryError](wrapped)
	if ok || admitted != nil {
		t.Fatalf("wrapped boundary acquired authority: %#v, %t", admitted, ok)
	}
}

func TestReconciliationDiagnosisFreezesBoundedRecoveryEvidence(t *testing.T) {
	steps := []struct {
		value ReconciliationStep
		name  string
	}{
		{ReconciliationCandidateObservation, "candidate_observation"},
		{ReconciliationStageDurability, "stage_durability"},
		{ReconciliationNamespaceDurability, "namespace_durability"},
		{ReconciliationRecordPromotion, "record_promotion"},
	}
	for _, step := range steps {
		if !step.value.Valid() || step.value.String() != step.name {
			t.Fatalf("reconciliation step %d = %q, want %q", step.value, step.value, step.name)
		}
	}
	if ReconciliationStep(0).String() != "" || ReconciliationStep(255).String() != "" {
		t.Fatal("unknown reconciliation step acquired a diagnostic name")
	}

	cause := reconciliationNativeFailure{class: outputcap.NativeErrorAccessDenied}
	err := reconciliationError(ReconciliationStageDurability, cause)
	var diagnostic *ReconciliationError
	if !errors.As(err, &diagnostic) || diagnostic.Step() != ReconciliationStageDurability ||
		diagnostic.Fault().Valid() == false || !errors.Is(err, cause) ||
		diagnostic.Error() != "checkpoint reconciliation stage_durability failed: provider failure" {
		t.Fatalf("reconciliation diagnosis = (%#v, %v)", diagnostic, err)
	}
	if class, ok := diagnostic.NativeClass(); !ok || class != outputcap.NativeErrorAccessDenied {
		t.Fatalf("native class = (%s, %t), want access denied", class, ok)
	}

	if reconciliationError(ReconciliationStageDurability, nil) != nil {
		t.Fatal("nil recovery cause manufactured a reconciliation failure")
	}
	invalid := reconciliationError(0, errors.New("invalid step"))
	if !errors.As(invalid, &diagnostic) || diagnostic.Fault() != transferfault.DependencyContractFault() {
		t.Fatal("invalid reconciliation step did not fail closed")
	}
	withoutNative := reconciliationError(ReconciliationRecordPromotion, errors.New("record failure"))
	if !errors.As(withoutNative, &diagnostic) {
		t.Fatalf("record failure was not diagnosed: %v", withoutNative)
	}
	if _, ok := diagnostic.NativeClass(); ok {
		t.Fatal("untyped record failure acquired a native error class")
	}

	var nilDiagnostic *ReconciliationError
	if nilDiagnostic.Error() != "checkpoint reconciliation failed" || nilDiagnostic.Unwrap() != nil ||
		nilDiagnostic.Step() != 0 || nilDiagnostic.Fault().Valid() {
		t.Fatal("nil reconciliation diagnostic accessors are not stable")
	}
	if _, ok := nilDiagnostic.NativeClass(); ok {
		t.Fatal("nil reconciliation diagnostic acquired a native error class")
	}
}

func TestCheckpointBoundaryNormalizationPreservesControlAndClosedAuthority(t *testing.T) {
	for name, test := range map[string]struct {
		call func(error) error
	}{
		"repository": {call: func(cause error) error { return repositoryError("recover", cause) }},
		"coded":      {call: func(cause error) error { return codedError(ErrorStateIO, "recover", cause) }},
		"file output": {call: func(cause error) error {
			return fileOutputBoundaryErrorWithoutContext(transferfault.ScopeOutputPause, cause)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			for _, control := range []error{context.Canceled, context.DeadlineExceeded} {
				if got := test.call(control); got != control {
					t.Fatalf("control failure = %v, want %v", got, control)
				}
			}
		})
	}

	existingRepository := &Error{code: ErrorBusy, operation: "candidate", cause: outputcap.ErrNamespaceLockBusy}
	normalized := repositoryError("outer", existingRepository)
	var repositoryErr *Error
	if !errors.As(normalized, &repositoryErr) || repositoryErr.Code() != ErrorBusy ||
		repositoryErr.Operation() != "candidate" {
		t.Fatalf("repository authority = %#v", normalized)
	}

	existingBoundary := checkpointBoundaryError(ErrorUnsafeInstall, "candidate", outputcap.ErrUnsafeNamespace)
	if got := codedError(ErrorStateIO, "outer", existingBoundary); got != existingBoundary {
		t.Fatal("coded error replaced an existing typed boundary")
	}
	if got := fileOutputBoundaryErrorWithoutContext(transferfault.ScopeOutputPause, existingBoundary); got != existingBoundary {
		t.Fatal("file output normalization replaced an existing typed boundary")
	}
	if got := fileOutputBoundaryError(context.TODO(), transferfault.ScopeOutputPause, errors.New("provider failure")); got == nil {
		t.Fatal("provider failure normalization returned nil")
	}

	for name, cause := range map[string]error{
		"unsupported filesystem": outputcap.ErrRecoverableOutputUnsupported,
		"namespace lock":         outputcap.ErrNamespaceLockBusy,
	} {
		t.Run(name, func(t *testing.T) {
			err := fileOutputBoundaryErrorWithoutContext(transferfault.ScopeOutputPause, cause)
			var boundary *transferfault.BoundaryError
			if !errors.As(err, &boundary) || !boundary.Fault().Valid() || !errors.Is(err, cause) {
				t.Fatalf("file output boundary = %#v", err)
			}
		})
	}
}
