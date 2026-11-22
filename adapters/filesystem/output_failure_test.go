package filesystem

import (
	"errors"
	"fmt"
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
	if sealed := sealOutputError(osfs.ErrOutputBusy); !errors.Is(sealed, output.ErrBusy) {
		t.Fatalf("busy normalization = %v", sealed)
	}
}

func TestSealedFailurePreservesAdapterDetailWithoutLeakingForgedWrappers(t *testing.T) {
	sealed, ok := SealOutputFailure(OutputFailureDiagnostic{
		Stage:              output.FailureDurability,
		ReconciliationStep: CheckpointStageDurability,
		NativeErrorClass:   NativeErrorAccessDenied,
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
		detail.ReconciliationStep != CheckpointStageDurability ||
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
