package resumecommand

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs"
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

type resumeDiagnosticTestError struct {
	diagnostic osfs.FilesystemOutputDiagnostic
}

func (failure resumeDiagnosticTestError) Error() string {
	return "provider path and capability canary"
}

func (failure resumeDiagnosticTestError) FilesystemOutputDiagnostic() osfs.FilesystemOutputDiagnostic {
	return failure.diagnostic
}

func TestResumeListFailureStageMatrixIsClosedAndPathFree(t *testing.T) {
	tests := []struct {
		name       string
		diagnostic osfs.FilesystemOutputDiagnostic
		reason     string
		fields     []string
	}{
		{
			name: "destination binding",
			diagnostic: osfs.FilesystemOutputDiagnostic{
				Stage: osfs.FilesystemOutputFailureDestinationBinding,
			},
			reason: resumeDestinationBindingReason,
		},
		{
			name: "inventory paging",
			diagnostic: osfs.FilesystemOutputDiagnostic{
				Stage: osfs.FilesystemOutputFailureInventoryPaging,
			},
			reason: resumeInventoryPagingReason,
		},
		{
			name: "operation acquisition",
			diagnostic: osfs.FilesystemOutputDiagnostic{
				Stage: osfs.FilesystemOutputFailureOperationAcquisition,
			},
			reason: resumeOperationAcquisitionReason,
		},
		{
			name: "checkpoint reconciliation",
			diagnostic: osfs.FilesystemOutputDiagnostic{
				Stage:              osfs.FilesystemOutputFailureCheckpointReconciliation,
				ReconciliationStep: osfs.FilesystemCheckpointRecordPromotion,
			},
			reason: resumeCheckpointReconcileReason,
			fields: []string{`reconciliation_stage="record_promotion"`},
		},
		{
			name: "native durability",
			diagnostic: osfs.FilesystemOutputDiagnostic{
				Stage:              osfs.FilesystemOutputFailureNativeDurability,
				ReconciliationStep: osfs.FilesystemCheckpointStageDurability,
				NativeErrorClass:   osfs.FilesystemNativeErrorAccessDenied,
			},
			reason: resumeNativeDurabilityReason,
			fields: []string{
				`reconciliation_stage="stage_durability"`,
				`native_error_class="access_denied"`,
			},
		},
		{
			name: "authority close",
			diagnostic: osfs.FilesystemOutputDiagnostic{
				Stage: osfs.FilesystemOutputFailureAuthorityClose,
			},
			reason: resumeAuthorityCloseReason,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app, stdout, _ := newResumeTestApp()
			app.resumeInventories = &fakeResumeStateInventoryOpener{
				err: resumeDiagnosticTestError{diagnostic: test.diagnostic},
			}
			result := app.Run(context.Background(), []string{
				"resume", "list", "-o", t.TempDir(),
			})
			rendered := stdout.String()
			if result != ResultFailure ||
				!strings.Contains(rendered, `reason="`+test.reason+`"`) ||
				!strings.Contains(rendered, `stage="`+test.diagnostic.Stage.String()+`"`) {
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
