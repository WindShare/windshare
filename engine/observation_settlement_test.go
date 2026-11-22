package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/task"
)

type unknownDiagnostic struct{}

func (unknownDiagnostic) EngineEvent() {}

// A failed optional path and an unrecognized event are not task outcomes.
// Both must never disturb the workflow's authoritative settlement.
func TestDiagnosticFailuresPreserveSuccessfulApplicationSettlement(t *testing.T) {
	application := newTestEngine(t, Config{ObservationCapacity: 16})
	pathCause := errors.New("optional peer path failed")
	current, err := startWorkflow(application, t.Context(), "receive", diagnostics.Sink{},
		func(_ context.Context, control task.Control) task.Completion[int] {
			control.Emit(receive.Warning{Code: receive.FailurePeerNegotiation, Cause: pathCause})
			control.Emit(unknownDiagnostic{})
			return task.Completion[int]{Value: 42, Outcome: task.OutcomeSuccess}
		})
	if err != nil {
		t.Fatal(err)
	}
	result, err := current.Wait(t.Context())
	if err != nil || result.Outcome != OutcomeSuccess || result.Err != nil || result.Value != 42 {
		t.Fatalf("diagnostic changed settlement: %+v, %v", result, err)
	}
	warnings, finished := 0, 0
	for envelope := range current.Observations() {
		if envelope.TaskID != current.ID() || envelope.At.IsZero() {
			t.Fatalf("lost correlation: %+v", envelope)
		}
		source, ok := envelope.Event.(diagnostics.Source)
		if !ok || !source.DiagnosticRecord().Valid() {
			t.Fatalf("unserializable workflow event: %T", envelope.Event)
		}
		switch event := envelope.Event.(type) {
		case Warning:
			warnings++
			if event.Failure.Code != FailureDirectUnavailable {
				t.Fatalf("path failure lost its meaning: %+v", event)
			}
		case TaskLifecycle:
			if event.Stage == "task_settled" {
				finished++
				if event.Outcome != "success" || event.Failure != nil {
					t.Fatalf("Wait and lifecycle disagree: %+v", event)
				}
			}
		}
	}
	if warnings != 1 || finished != 1 {
		t.Fatalf("warning/settlement=%d/%d", warnings, finished)
	}
}
