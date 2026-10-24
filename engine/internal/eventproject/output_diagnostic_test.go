package eventproject

import (
	"errors"
	"fmt"
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

func TestOutputDiagnosticRejectionRetainsProviderEvidence(t *testing.T) {
	context := appobs.CaptureObservationRejection(appobs.ObservationRejection{
		Source: "document_provider", Event: "output_diagnostic", Stage: "projection",
		Field: "operation", Rule: "known_enum", Operation: "receive-7",
	}, appobs.RejectedEnum("operation", 255))
	for _, test := range []struct {
		name   string
		reason appobs.ObserverLossReason
		want   appobs.ObserverLossReason
	}{
		{"unknown_enum", appobs.ObserverLossUnknownEnum, appobs.ObserverLossUnknownEnum},
		{"invalid_identity", appobs.ObserverLossInvalidIdentity, appobs.ObserverLossInvalidIdentity},
		{"invalid_stage", appobs.ObserverLossInvalidStageFields, appobs.ObserverLossInvalidStageFields},
		{"unsupported_reason", appobs.ObserverLossReason(255), appobs.ObserverLossEventContract},
	} {
		diagnostic := &appobs.OutputDiagnosticError{Reason: test.reason, Context: context}
		for name, err := range map[string]error{
			"direct": diagnostic, "wrapped": fmt.Errorf("output provider: %w", diagnostic),
		} {
			t.Run(test.name+"/"+name, func(t *testing.T) {
				if !errors.Is(err, appobs.ErrInvalidEvent) {
					t.Fatalf("provider error lost its event classification: %v", err)
				}
				var published task.Event
				publish := func(value task.Observation) bool { published = value.Event; return true }
				if !Workflow(appobs.WorkflowReceive, task.Observation{Event: receive.OutputObserved{Err: err}}, publish) {
					t.Fatal("provider rejection stopped observation delivery")
				}
				loss, ok := published.(appobs.ObserverLossObserved)
				if !ok || loss.Category() != appobs.ObserverLossOutputDiagnostic || loss.Reason() != test.want || loss.Count() != 1 {
					t.Fatalf("provider rejection classification = %+v", published)
				}
				if rejection, ok := loss.Rejection(); !ok || rejection != context {
					t.Fatalf("provider rejection evidence = %+v, present=%t", rejection, ok)
				}
			})
		}
	}
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
