package wire_test

import (
	"encoding/json"
	"testing"

	appobs "github.com/windshare/windshare/engine/observation"
	wire "github.com/windshare/windshare/engine/observation/wire"
)

func TestEngineTaskTraceExportsExplicitOutcomeAndFailureClass(t *testing.T) {
	id, _ := appobs.NewEngineTaskID("application:42")
	failure, _ := appobs.NewFailure(appobs.FailureInvalidInput)
	for _, spec := range []appobs.EngineTaskSpec{
		{Workflow: appobs.WorkflowReceive, Task: id, Stage: appobs.EngineTaskSettled, Outcome: appobs.EngineOutcomeFailed, FailureClass: appobs.EngineFailureUsage, Failure: failure},
		{Workflow: appobs.WorkflowReceive, Task: id, Stage: appobs.EngineTaskSettled, Outcome: appobs.EngineOutcomeSuccess, FailureClass: appobs.EngineFailureNone},
		{Workflow: appobs.WorkflowShare, Task: id, Stage: appobs.EngineTaskSettled, Outcome: appobs.EngineOutcomeStopped, FailureClass: appobs.EngineFailureNone},
	} {
		event, err := appobs.NewEngineTaskObserved(spec)
		if err != nil {
			t.Fatal(err)
		}
		record, err := wire.Encode(event)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(record.Payload)
		if err != nil {
			t.Fatal(err)
		}
		var decoded wire.EngineTaskPayload
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Outcome != spec.Outcome || decoded.FailureClass != spec.FailureClass || (decoded.Failure != nil) != spec.Failure.Valid() {
			t.Fatalf("serialized terminal result is ambiguous: %s, %v", raw, err)
		}
	}
}
