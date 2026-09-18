package osfs

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputruntime"
)

func TestFilesystemOutputFailureSelectionSeparatesPublicAndNativeReleaseCauses(t *testing.T) {
	primaryCause := errors.New("primary failure")
	nativeCloseCause := errors.New("native authority release failure")
	publicClose := filesystemOutputDiagnosticTestCarrier{
		diagnostic: FilesystemOutputDiagnostic{Stage: FilesystemOutputFailureAuthorityClose},
	}
	primary := outputruntime.DiagnoseFilesystemOutputFailure(outputruntime.FilesystemOutputFailureCheckpointReconciliation, primaryCause)
	nativeClose := outputruntime.DiagnoseFilesystemOutputFailure(outputruntime.FilesystemOutputFailureAuthorityClose, nativeCloseCause)
	mixed := fmt.Errorf("recovery: %w", errors.Join(primary, publicClose, nativeClose))
	selected := FilesystemOutputFailureForStage(mixed, FilesystemOutputFailureAuthorityClose)
	if !errors.Is(selected, publicClose) || !errors.Is(selected, nativeCloseCause) || errors.Is(selected, primaryCause) {
		t.Fatalf("release projection=%v", selected)
	}
	if selected := FilesystemOutputFailureForStage(mixed, 0); selected != nil {
		t.Fatalf("invalid stage selected errors: %v", selected)
	}
	if selected := FilesystemOutputFailureForStage(nil, FilesystemOutputFailureAuthorityClose); selected != nil {
		t.Fatalf("nil failure selected errors: %v", selected)
	}
}

func TestFilesystemOutputCloseAnnotationOwnsAllReleaseCausesInsideIt(t *testing.T) {
	earlierCause := errors.New("checkpoint close failed")
	otherCloseCause := errors.New("registry close failed")
	earlierDiagnostic := outputruntime.DiagnoseFilesystemOutputFailure(
		outputruntime.FilesystemOutputFailureNativeDurability, earlierCause,
	)
	closeBoundary := outputruntime.DiagnoseFilesystemOutputFailure(
		outputruntime.FilesystemOutputFailureAuthorityClose,
		errors.Join(context.Canceled, earlierDiagnostic, otherCloseCause),
	)
	selected := FilesystemOutputFailureForStage(closeBoundary, FilesystemOutputFailureAuthorityClose)
	for _, cause := range []error{context.Canceled, earlierCause, otherCloseCause} {
		if !errors.Is(selected, cause) {
			t.Fatalf("release annotation lost cause %v: %v", cause, selected)
		}
	}
	diagnostic, ok := FilesystemOutputDiagnosticFor(selected)
	if !ok || diagnostic.Stage != FilesystemOutputFailureAuthorityClose {
		t.Fatalf("release annotation was hidden by earlier evidence: %+v", diagnostic)
	}
}
