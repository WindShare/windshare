package checkpointstore

import (
	"errors"
	"fmt"
	"testing"

	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestErrorCodeForPreservesDirectClassifications(t *testing.T) {
	for _, code := range []ErrorCode{
		ErrorBusy, ErrorCorruptRecord, ErrorUnsafeInstall, ErrorOwnershipMismatch, ErrorStateIO,
	} {
		t.Run(string(code), func(t *testing.T) {
			// A diagnostic cause may suggest a different policy from the boundary
			// that observed the failure; only the explicit classification owns it.
			for _, cause := range []error{
				errors.New("unclassified provider cause"),
				checkpointmodel.ErrInvalidOrdinaryOperation,
				checkpointmodel.ErrRecordChecksum,
				outputcap.ErrNamespaceLockBusy,
			} {
				for _, classified := range []error{
					&Error{code: code, operation: "test", cause: cause},
					codedError(code, "test", cause),
				} {
					if got := ErrorCodeFor(classified); got != code {
						t.Fatalf("classification of %v = %s, want %s", classified, got, code)
					}
				}
			}
		})
	}
}

func TestErrorCodeForDoesNotPromoteDiagnosticWrappers(t *testing.T) {
	classified := codedError(ErrorCorruptRecord, "decode", checkpointmodel.ErrInvalidOrdinaryOperation)
	ioFailure := errors.New("storage read failed")
	for name, err := range map[string]error{
		"outer wrapper": fmt.Errorf("adapter diagnostic: %w", classified),
		"sibling I/O":   errors.Join(classified, ioFailure),
		"reversed I/O":  errors.Join(ioFailure, classified),
	} {
		t.Run(name, func(t *testing.T) {
			if got := ErrorCodeFor(err); got != ErrorStateIO {
				t.Fatalf("diagnostic wrapper acquired an inner classification: %s", got)
			}
		})
	}
}

func TestErrorCodeForRejectsInvalidAndForeignClassifications(t *testing.T) {
	var nilRepository *Error
	var nilBoundary *transferfault.BoundaryError
	outputFault, err := transferfault.NewOutput(transferfault.ScopeOutputPause, transferfault.OutputStateIO)
	if err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"nil repository": nilRepository,
		"nil boundary":   nilBoundary,
		"invalid repository": &Error{
			code: "future", operation: "decode", cause: checkpointmodel.ErrRecordChecksum,
		},
		"foreign boundary": transferfault.Wrap(outputFault, checkpointmodel.ErrRecordChecksum),
		"invalid boundary": &transferfault.BoundaryError{},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ErrorCodeFor(err); got != ErrorStateIO {
				t.Fatalf("invalid or foreign authority inferred a diagnostic code: %s", got)
			}
		})
	}
}
