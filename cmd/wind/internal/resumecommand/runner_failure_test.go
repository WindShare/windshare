package resumecommand

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/cmd/wind/internal/command"
	"github.com/windshare/windshare/engine"
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
		}); result != command.ExitFailure || !strings.Contains(stdout.String(), resumeDestinationUnknownReason) {
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
		}); result != command.ExitFailure || strings.Contains(stdout.String(), "private") {
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
		}); result != command.ExitFailure || inventory.discardCalls != 0 ||
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
		}); result != command.ExitFailure {
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
			if result != command.ExitFailure ||
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

func TestResumeFailureDetailRejectsOrphanNativeClassAndGatesStage(t *testing.T) {
	orphan := resumeFailureDetail{NativeClass: filesystem.NativeErrorNotFound}
	if orphan.Valid() {
		t.Fatalf("orphan native class accepted as valid: %+v", orphan)
	}
	renderer := textRenderer{}
	if _, err := renderer.ListControlStatus(resumeListStatusNeedsAttention, resumeDestinationUnknownReason, orphan); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("ListControlStatus(orphan) err = %v, want %v", err, errResumeStateContract)
	}
	if _, err := renderer.DiscardControlStatus(resumeDiscardStatusChanged, 1, resumeOperationChangedReason, orphan); !errors.Is(err, errResumeStateContract) {
		t.Fatalf("DiscardControlStatus(orphan) err = %v, want %v", err, errResumeStateContract)
	}
	var rendered strings.Builder
	renderResumeFailureDetail(&rendered, orphan)
	if strings.Contains(rendered.String(), "stage=") {
		t.Fatalf("renderResumeFailureDetail emitted empty stage: %q", rendered.String())
	}

	notFoundCause, ok := filesystem.SealOutputFailure(filesystem.OutputFailureDiagnostic{
		Stage:            output.FailureLookup,
		NativeErrorClass: filesystem.NativeErrorNotFound,
	})
	if !ok {
		t.Fatal("failed to seal not_found diagnostic")
	}
	accessDeniedCause, ok := filesystem.SealOutputFailure(filesystem.OutputFailureDiagnostic{
		Stage:            output.FailureDurability,
		NativeErrorClass: filesystem.NativeErrorAccessDenied,
	})
	if !ok {
		t.Fatal("failed to seal access_denied diagnostic")
	}
	if detail := presentFailureDetail(nil); detail != (resumeFailureDetail{}) {
		t.Fatalf("presentFailureDetail(nil) = %+v, want zero", detail)
	}
	for _, tc := range []struct {
		name    string
		failure *engine.RecoveryFailure
		want    resumeFailureDetail
	}{
		{
			name: "changed race suppresses native class without stage",
			failure: &engine.RecoveryFailure{
				Kind:   engine.RecoveryFailureChanged,
				Reason: resumeOperationChangedReason,
				Cause:  notFoundCause,
			},
			want: resumeFailureDetail{},
		},
		{
			name: "cancelled failure suppresses joined native class without stage",
			failure: &engine.RecoveryFailure{
				Kind:   engine.RecoveryFailureCancelled,
				Reason: resumeCommandCancelledReason,
				Cause:  errors.Join(context.Canceled, accessDeniedCause),
			},
			want: resumeFailureDetail{},
		},
		{
			name: "mismatched native stage is not attached to surfaced stage",
			failure: &engine.RecoveryFailure{
				Kind:   engine.RecoveryFailureNeedsAttention,
				Reason: resumeAuthorityCloseReason,
				Detail: engine.RecoveryFailureDetail{Stage: output.FailureClose},
				Cause:  accessDeniedCause,
			},
			want: resumeFailureDetail{Stage: output.FailureClose},
		},
		{
			name: "matching surfaced stage preserves native class",
			failure: &engine.RecoveryFailure{
				Kind:   engine.RecoveryFailureNeedsAttention,
				Reason: resumeNativeDurabilityReason,
				Detail: engine.RecoveryFailureDetail{Stage: output.FailureDurability},
				Cause:  accessDeniedCause,
			},
			want: resumeFailureDetail{
				Stage:       output.FailureDurability,
				NativeClass: filesystem.NativeErrorAccessDenied,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := presentFailureDetail(tc.failure); got != tc.want {
				t.Fatalf("presentFailureDetail() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

type errorResumeWriter struct{}

func (errorResumeWriter) Write([]byte) (int, error) {
	return 0, errors.New("injected output failure")
}
