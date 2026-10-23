package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
)

type unknownDiagnostic struct{}

func (unknownDiagnostic) EngineEvent() {}

// A failed optional path and an unrecognized diagnostic are not task outcomes.
// Both must remain visible without replacing the workflow's successful result.
func TestDiagnosticFailuresPreserveSuccessfulApplicationSettlement(t *testing.T) {
	application := newTestEngine(t, Config{ObservationCapacity: 16})
	pathCause := errors.New("optional peer path failed")
	current, err := startWorkflow(application, t.Context(), appobs.WorkflowReceive,
		func(_ context.Context, control task.Control) task.Completion[int] {
			control.Emit(receive.Warning{Code: receive.FailurePeerNegotiation, Cause: pathCause})
			control.Emit(unknownDiagnostic{})
			return task.Completion[int]{Value: 42, Settlement: task.Settlement{Outcome: task.OutcomeSuccess}}
		})
	if err != nil {
		t.Fatal(err)
	}
	result, err := current.Wait(t.Context())
	if err != nil || result.Outcome != OutcomeSuccess || result.Err != nil ||
		result.Failure != (appobs.Failure{}) || result.Value != 42 {
		t.Fatalf("diagnostic changed settlement: %+v, %v", result, err)
	}
	warnings, rejected, finished := 0, 0, 0
	for envelope := range current.Observations() {
		if envelope.TaskID != current.ID() || envelope.At.IsZero() {
			t.Fatalf("lost correlation: %+v", envelope)
		}
		switch event := envelope.Event.(type) {
		case appobs.Warning:
			warnings++
			if event.Failure().Code() != appobs.FailurePeerNegotiation {
				t.Fatalf("path failure lost its meaning: %+v", event)
			}
		case appobs.ObserverLossObserved:
			rejected++
			if event.Count() != 1 || event.Category() != appobs.ObserverLossEngineObservation {
				t.Fatalf("diagnostic rejection was not accounted: %+v", event)
			}
		case task.Lifecycle:
			if event.State == task.Finished {
				finished++
				if event.Settlement != result.Settlement {
					t.Fatal("Wait and lifecycle disagree")
				}
			}
		}
	}
	if warnings != 1 || rejected != 1 || finished != 1 {
		t.Fatalf("warning/rejection/settlement=%d/%d/%d", warnings, rejected, finished)
	}
}
