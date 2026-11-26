package filesystem

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/engine/output"
	"github.com/windshare/windshare/osfs"
)

func TestSealedFailureSeparatesCleanupRegardlessOfCauseOrder(t *testing.T) {
	primaryCause := errors.New("checkpoint promotion failed")
	closeCauseA := errors.New("close repository failed")
	closeCauseB := errors.New("close destination failed")
	primary, ok := output.SealFailure(primaryCause, output.FailureDiagnostic{Stage: output.FailureCheckpoint})
	if !ok {
		t.Fatal("primary diagnostic was not sealed")
	}
	closeA, ok := output.SealFailure(closeCauseA, output.FailureDiagnostic{Stage: output.FailureClose})
	if !ok {
		t.Fatal("closeA diagnostic was not sealed")
	}
	closeB, ok := output.SealFailure(closeCauseB, output.FailureDiagnostic{Stage: output.FailureClose})
	if !ok {
		t.Fatal("closeB diagnostic was not sealed")
	}
	for _, causes := range [][]error{{primary, closeA, closeB}, {closeA, primary, closeB}} {
		joined := fmt.Errorf("native operation: %w", errors.Join(causes...))
		cleanup := output.FailureForStage(joined, output.FailureClose)
		if !errors.Is(cleanup, closeCauseA) || !errors.Is(cleanup, closeCauseB) || errors.Is(cleanup, primaryCause) {
			t.Fatalf("cleanup claimed unrelated work: %v", cleanup)
		}
	}
	if sealed := sealOutputError(osfs.ErrOutputShareStopped); !errors.Is(sealed, output.ErrShareStopped) {
		t.Fatalf("share-stopped normalization = %v", sealed)
	}
	if sealed := sealOutputError(osfs.ErrOutputAlreadyRunning); !errors.Is(sealed, output.ErrOperationAlreadyRunning) {
		t.Fatalf("already-running normalization = %v", sealed)
	}
	if sealed := sealOutputError(osfs.ErrOutputAmbiguous); !errors.Is(sealed, output.ErrOperationAmbiguous) {
		t.Fatalf("ambiguous normalization = %v", sealed)
	}
	if sealed := sealOutputError(&osfs.OutputAttentionError{Reason: osfs.OutputAttentionDestination}); sealed == nil {
		t.Fatal("attention normalization returned nil")
	} else if attention, ok := errors.AsType[*output.AttentionError](sealed); !ok || attention.Reason != output.AttentionDestination {
		t.Fatalf("attention normalization = (%v, %t)", attention, ok)
	}
	if sealed := sealOutputError(osfs.ErrOutputBusy); !errors.Is(sealed, output.ErrBusy) {
		t.Fatalf("busy normalization = %v", sealed)
	}
}

func TestSealedFailurePreservesAdapterDetailWithoutLeakingForgedWrappers(t *testing.T) {
	sealed, ok := SealOutputFailure(OutputFailureDiagnostic{
		Stage:            output.FailureDurability,
		NativeErrorClass: NativeErrorAccessDenied,
	})
	if !ok {
		t.Fatal("valid diagnostic was not sealed")
	}
	got, present := output.DiagnosticFor(sealed)
	if !present || got.Stage != output.FailureDurability {
		t.Fatalf("diagnostic=%+v present=%v", got, present)
	}
	if _, forged := output.DiagnosticFor(fmt.Errorf("arbitrary wrapper: %w", sealed)); forged {
		t.Fatal("direct proof traversed untrusted wrapper")
	}
	detail, ok := FailureDetailFor(sealed)
	if !ok || !detail.Valid() || detail.Stage != output.FailureDurability ||
		detail.NativeErrorClass != NativeErrorAccessDenied {
		t.Fatalf("detail=%+v ok=%v", detail, ok)
	}
	if _, ok := FailureDetailFor(errors.New("opaque provider failure")); ok {
		t.Fatal("opaque error produced native failure detail")
	}
	if _, ok := SealOutputFailure(OutputFailureDiagnostic{}); ok {
		t.Fatal("zero diagnostic was sealed")
	}
}

func TestSealOutputErrorSealsNativeDiagnosticWithoutLeakingHostPath(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "secret-host-dir", "missing")
	authority, err := FilesystemOutput{RootPath: missingRoot, CreateRoot: false}.NewOutputAuthority(output.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	bindErr := authority.BindDestination(context.Background())
	if bindErr == nil {
		t.Fatal("expected BindDestination on missing root to fail")
	}
	if strings.Contains(bindErr.Error(), "secret-host-dir") || strings.Contains(bindErr.Error(), missingRoot) {
		t.Fatalf("sealed error text leaked host path: %q", bindErr.Error())
	}
	diag, ok := output.DiagnosticFor(bindErr)
	if !ok || diag.Stage != output.FailureDestinationBinding || !diag.Fault.Valid() {
		t.Fatalf("output.DiagnosticFor = (%+v, %t), want destination_binding with valid fault", diag, ok)
	}
	detail, ok := FailureDetailFor(bindErr)
	if !ok || detail.Stage != output.FailureDestinationBinding || detail.NativeErrorClass != NativeErrorNotFound {
		t.Fatalf("FailureDetailFor = (%+v, %t), want destination_binding/not_found", detail, ok)
	}
}
