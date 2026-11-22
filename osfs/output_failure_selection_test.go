package osfs

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/osfs/internal/output"
)

func TestOutputFailureSelectionSeparatesPrimaryAndReleaseCauses(t *testing.T) {
	primaryCause := errors.New("primary failure")
	nativeCloseCause := errors.New("native authority release failure")
	otherCloseCause := errors.New("secondary release failure")
	primary := output.DiagnoseFailure(OutputFailureCheckpoint, primaryCause)
	nativeClose := output.DiagnoseFailure(OutputFailureClose, nativeCloseCause)
	otherClose := output.DiagnoseFailure(OutputFailureClose, otherCloseCause)
	mixed := fmt.Errorf("recovery: %w", errors.Join(primary, otherClose, nativeClose))
	selected := OutputFailureForStage(mixed, OutputFailureClose)
	if !errors.Is(selected, otherCloseCause) || !errors.Is(selected, nativeCloseCause) || errors.Is(selected, primaryCause) {
		t.Fatalf("release projection=%v", selected)
	}
	if selected := OutputFailureForStage(mixed, 0); selected != nil {
		t.Fatalf("invalid stage selected errors: %v", selected)
	}
	if selected := OutputFailureForStage(nil, OutputFailureClose); selected != nil {
		t.Fatalf("nil failure selected errors: %v", selected)
	}
}

func TestOutputCloseAnnotationOwnsAllReleaseCausesInsideIt(t *testing.T) {
	earlierCause := errors.New("checkpoint close failed")
	otherCloseCause := errors.New("registry close failed")
	earlierDiagnostic := output.DiagnoseFailure(OutputFailureDurability, earlierCause)
	closeBoundary := output.DiagnoseFailure(
		OutputFailureClose,
		errors.Join(context.Canceled, earlierDiagnostic, otherCloseCause),
	)
	selected := OutputFailureForStage(closeBoundary, OutputFailureClose)
	for _, cause := range []error{context.Canceled, earlierCause, otherCloseCause} {
		if !errors.Is(selected, cause) {
			t.Fatalf("release annotation lost cause %v: %v", cause, selected)
		}
	}
	diagnostic, ok := OutputDiagnosticFor(selected)
	if !ok || diagnostic.Stage != OutputFailureClose {
		t.Fatalf("release annotation was hidden by earlier evidence: %+v", diagnostic)
	}
}
