package observation

import "testing"

func TestTaskLifecycleSeparatesStateFromSettlement(t *testing.T) {
	id, _ := NewEngineTaskID("application:42")
	for _, stage := range []EngineTaskStage{EngineTaskStarted, EngineTaskStopping} {
		event, err := NewEngineTaskObserved(EngineTaskSpec{Workflow: WorkflowReceive, Task: id, Stage: stage})
		if err != nil || event.Facts().Task != id {
			t.Fatalf("lifecycle: %v %v", event, err)
		}
	}
	failure, _ := NewFailure(FailureUnexpected)
	tests := []struct {
		spec  EngineTaskSpec
		valid bool
	}{
		{EngineTaskSpec{Workflow: WorkflowShare, Task: id, Stage: EngineTaskSettled, Outcome: EngineOutcomeSuccess, FailureClass: EngineFailureNone}, true},
		{EngineTaskSpec{Workflow: WorkflowShare, Task: id, Stage: EngineTaskSettled, Outcome: EngineOutcomeFailed, FailureClass: EngineFailureLocal, Failure: failure}, true},
		{EngineTaskSpec{Workflow: WorkflowShare, Task: id, Stage: EngineTaskSettled, Outcome: EngineOutcomeSuccess, FailureClass: EngineFailureNone, CleanupFailure: failure}, false},
		{EngineTaskSpec{Workflow: WorkflowShare, Task: id, Stage: EngineTaskStarted, Outcome: EngineOutcomeFailed, Failure: failure}, false},
		{EngineTaskSpec{Workflow: WorkflowShare, Task: id, Stage: EngineTaskStage(255)}, false},
		{EngineTaskSpec{Workflow: WorkflowShare, Stage: EngineTaskStarted}, false},
	}
	for _, test := range tests {
		_, err := NewEngineTaskObserved(test.spec)
		if (err == nil) != test.valid {
			t.Errorf("%+v: %v", test.spec, err)
		}
	}
}
