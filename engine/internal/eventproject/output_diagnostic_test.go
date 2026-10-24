package eventproject

import (
	"testing"

	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
)

type documentProviderDiagnostic struct{ invalid bool }

func (documentProviderDiagnostic) EngineEvent()              {}
func (documentProviderDiagnostic) OutputDiagnostic()         {}
func (documentProviderDiagnostic) Workflow() appobs.Workflow { return appobs.WorkflowReceive }
func (documentProviderDiagnostic) Level() appobs.Level       { return appobs.LevelDebug }
func (value documentProviderDiagnostic) Validate() error {
	if value.invalid {
		return appobs.ErrInvalidEvent
	}
	return nil
}

func TestProviderDiagnosticsCrossTheWorkflowWithoutFilesystemTypes(t *testing.T) {
	event := documentProviderDiagnostic{}
	var published task.Event
	publish := func(value task.Observation) bool { published = value.Event; return true }
	if !Workflow(appobs.WorkflowReceive, task.Observation{Event: receive.OutputObserved{Value: event}}, publish) {
		t.Fatal("provider diagnostic rejected")
	}
	if published != event {
		t.Fatalf("provider schema replaced: %T", published)
	}
	for _, invalid := range []receive.OutputObserved{{}, {Value: documentProviderDiagnostic{invalid: true}}} {
		if !Workflow(appobs.WorkflowReceive, task.Observation{Event: invalid}, publish) {
			t.Fatal("diagnostic rejection stopped observation delivery")
		}
		loss, ok := published.(appobs.ObserverLossObserved)
		if !ok || loss.Category() != appobs.ObserverLossOutputDiagnostic {
			t.Fatalf("invalid evidence bypassed bounded loss reporting: %T", published)
		}
	}
}
