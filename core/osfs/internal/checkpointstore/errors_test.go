package checkpointstore

import (
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
