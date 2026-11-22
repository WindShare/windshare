package resumecommand

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/engine/output"
)

func TestRunnerFailsClosedAtInjectedPresentationBoundaries(t *testing.T) {
	snapshot, _ := newResumeInventorySnapshot(
		[]resumeOperation{testResumeOperation("1", resumeOperationIncomplete)}, false,
	)
	t.Run("nil inventory", func(t *testing.T) {
		app, stdout, _ := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{}
		if result := app.Run(context.Background(), []string{
			"resume", "list", "-o", t.TempDir(),
		}); result != ResultFailure || !strings.Contains(stdout.String(), resumeDestinationUnknownReason) {
			t.Fatalf("result=%d stdout=%q", result, stdout.String())
		}
	})
	t.Run("snapshot failure", func(t *testing.T) {
		app, stdout, _ := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: &fakeResumeStateInventory{
			snapshot: snapshot, snapshotErr: errors.New("corrupt private record"),
		}}
		if result := app.Run(context.Background(), []string{
			"resume", "list", "-o", t.TempDir(),
		}); result != ResultFailure || strings.Contains(stdout.String(), "private") {
			t.Fatalf("result=%d stdout=%q", result, stdout.String())
		}
	})
	t.Run("terminal read failure", func(t *testing.T) {
		inventory := &fakeResumeStateInventory{snapshot: snapshot}
		app, _, stderr := newResumeTestApp()
		app.resumeInventories = &fakeResumeStateInventoryOpener{inventory: inventory}
		app.resumeConfirmation = &fakeResumeConfirmationTerminal{
			interactive: true, err: errors.New("terminal closed"),
		}
		if result := app.Run(context.Background(), []string{
			"resume", "discard", "-o", t.TempDir(), "--item", "1",
		}); result != ResultFailure || inventory.discardCalls != 0 ||
			!strings.Contains(stderr.String(), "could not be read") {
			t.Fatalf("result=%d inventory=%+v stderr=%q", result, inventory, stderr.String())
		}
	})
	t.Run("result write failure", func(t *testing.T) {
		app, _, _ := newResumeTestApp()
		app.stdout = errorResumeWriter{}
		app.resumeInventories = &fakeResumeStateInventoryOpener{
			inventory: &fakeResumeStateInventory{snapshot: snapshot},
		}
		if result := app.Run(context.Background(), []string{
			"resume", "list", "-o", t.TempDir(),
		}); result != ResultFailure {
			t.Fatalf("result=%d", result)
		}
	})
}

func TestResumeListFailureStageMatrixIsClosedAndPathFree(t *testing.T) {
	tests := []struct {
		name       string
		diagnostic filesystem.OutputFailureDiagnostic
		stage      output.FailureStage
		reason     string
		fields     []string
	}{
		{
			name: "destination binding",
			diagnostic: filesystem.OutputFailureDiagnostic{
				Stage: output.FailureDestinationBinding,
			},
			stage:  output.FailureDestinationBinding,
			reason: resumeDestinationBindingReason,
		},
		{
			name: "inventory paging",
			diagnostic: filesystem.OutputFailureDiagnostic{
				Stage: output.FailureInventory,
			},
			stage:  output.FailureInventory,
			reason: resumeInventoryPagingReason,
		},
		{
			name: "operation acquisition",
			diagnostic: filesystem.OutputFailureDiagnostic{
				Stage: output.FailureOpen,
			},
			stage:  output.FailureOpen,
			reason: resumeOperationAcquisitionReason,
		},
		{
			name: "checkpoint reconciliation",
			diagnostic: filesystem.OutputFailureDiagnostic{
				Stage: output.FailureCheckpoint,
			},
			stage:  output.FailureCheckpoint,
			reason: resumeCheckpointReconcileReason,
		},
		{
			name: "native durability",
			diagnostic: filesystem.OutputFailureDiagnostic{
				Stage:            output.FailureDurability,
				NativeErrorClass: filesystem.NativeErrorAccessDenied,
			},
			stage:  output.FailureDurability,
			reason: resumeNativeDurabilityReason,
			fields: []string{
				`native_error_class="access_denied"`,
			},
		},
		{
			name: "authority close",
			diagnostic: filesystem.OutputFailureDiagnostic{
				Stage: output.FailureClose,
			},
			stage:  output.FailureClose,
			reason: resumeAuthorityCloseReason,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, stdout, _ := newResumeTestApp()
			cause, ok := filesystem.SealOutputFailure(test.diagnostic)
			if !ok {
				t.Fatalf("diagnostic %+v was not sealed", test.diagnostic)
			}
			app.resumeInventories = &fakeResumeStateInventoryOpener{err: cause}
			result := app.Run(context.Background(), []string{
				"resume", "list", "-o", t.TempDir(),
			})
			rendered := stdout.String()
			if result != ResultFailure ||
				!strings.Contains(rendered, `reason="`+test.reason+`"`) ||
				!strings.Contains(rendered, `stage="`+test.stage.String()+`"`) {
				t.Fatalf("result=%d output=%q", result, rendered)
			}
			for _, field := range test.fields {
				if !strings.Contains(rendered, field) {
					t.Fatalf("output=%q missing=%q", rendered, field)
				}
			}
			if strings.Contains(rendered, "provider") ||
				strings.Contains(rendered, "path") ||
				strings.Contains(rendered, "capability") {
				t.Fatalf("provider canary escaped: %q", rendered)
			}
		})
	}
}

type errorResumeWriter struct{}

func (errorResumeWriter) Write([]byte) (int, error) {
	return 0, errors.New("injected output failure")
}
