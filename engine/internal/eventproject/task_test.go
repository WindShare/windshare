package eventproject

import (
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestEngineLifecycleProjectionPreservesTaskIdentityAndIndependentFailure(t *testing.T) {
	for _, state := range []task.State{task.Running, task.Stopping, task.Finished} {
		for _, reason := range []task.StopReason{task.NoStop, task.Cancelled, task.ShareStopped, task.ApplicationClosed} {
			settlement := task.Settlement{}
			if state == task.Finished {
				cleanupErr := errors.New("cleanup failed")
				settlement = task.Settlement{Outcome: task.OutcomeFailed, FailureClass: task.FailureLocal, Err: cleanupErr, CleanupError: cleanupErr}
			}
			settlement.Failure, _ = ClassifyError(settlement.Err)
			settlement.CleanupFailure, _ = ClassifyError(settlement.CleanupError)
			envelope := task.Observation{TaskID: "application:42", At: time.Unix(10, 0), Event: task.Lifecycle{
				State: state, StopReason: reason, Settlement: settlement,
			}}
			spec, relevant, err := projectTaskSpec(appobs.WorkflowReceive, envelope)
			if err != nil || !relevant || spec.Task.String() != string(envelope.TaskID) || spec.At != envelope.At ||
				spec.CleanupFailure.Valid() != (state == task.Finished) || spec.Failure.Valid() != (state == task.Finished) {
				t.Fatalf("projection = %+v, %v, %v", spec, relevant, err)
			}
			if _, err := appobs.NewEngineTaskObserved(spec); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, event := range []task.Lifecycle{{State: 255}, {State: task.Running, StopReason: 255}} {
		if _, _, err := projectTaskSpec(appobs.WorkflowReceive, task.Observation{TaskID: "application", Event: event}); err == nil {
			t.Fatal("invalid lifecycle accepted")
		}
	}
	if _, _, err := projectTaskSpec(appobs.WorkflowReceive, task.Observation{TaskID: "bad id", Event: task.Lifecycle{State: task.Running}}); err == nil {
		t.Fatal("invalid task identity accepted")
	}
}
