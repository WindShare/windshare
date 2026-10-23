package observation

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEngineTaskObservationsRetainTypedOwnershipAcrossStages(t *testing.T) {
	id, err := NewEngineTaskID("application:42")
	if err != nil {
		t.Fatal(err)
	}
	share, _ := NewSharingInstanceID(bytes16(1))
	session, _ := NewProtocolSessionID(bytes16(2))
	for _, stage := range []EngineTaskStage{EngineTaskStarted, EngineTaskStopping, EngineTaskSettled, EngineSourceAcquiring, EngineSourceAcquired, EngineSourceFailed, EngineGenerationChanged, EngineAdmissionDecision} {
		command := WorkflowShare
		if stage == EngineGenerationChanged || stage == EngineAdmissionDecision {
			command = WorkflowReceive
		}
		spec := EngineTaskSpec{Workflow: Workflow(command), Task: id, At: time.Unix(5, 0), Stage: stage, ShareInstance: share, Session: session}
		if stage == EngineTaskSettled {
			spec.Outcome, spec.FailureClass = EngineOutcomeStopped, EngineFailureNone
		}
		event, err := NewEngineTaskObserved(spec)
		if err != nil {
			t.Fatal(err)
		}
		if event.Workflow() != command || event.Level() != LevelDebug || event.Facts() != spec || event.Facts().Task.String() != "application:42" {
			t.Fatal("observation lost task identity or milestone")
		}
		if err := event.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEngineTaskObservationRejectsAmbiguousIdentityAndOwnership(t *testing.T) {
	for _, value := range []string{"", "user path/secret", "line\ncanary", strings.Repeat("a", maximumEngineTaskIDBytes+1)} {
		if _, err := NewEngineTaskID(value); !errors.Is(err, ErrInvalidIdentity) {
			t.Fatalf("identity accepted %q: %v", value, err)
		}
	}
	id, _ := NewEngineTaskID("abcDEF_-.09:1")
	for _, spec := range []EngineTaskSpec{
		{},
		{Workflow: WorkflowShare, Task: id, Stage: EngineTaskStage(255)},
		{Workflow: WorkflowShare, Task: id, Stage: EngineTaskStarted, StopReason: EngineStopReason(255)},
		{Workflow: WorkflowShare, Task: id, Stage: EngineSourceAcquired},
		{Workflow: WorkflowReceive, Task: id, Stage: EngineGenerationChanged},
		{Workflow: WorkflowReceive, Task: id, Stage: EngineTaskStarted, AdmissionTrigger: "unbounded-cause"},
		{Workflow: WorkflowReceive, Task: id, Stage: EngineTaskStarted, AdmissionTerminalOwner: "unbounded-owner"},
		{Workflow: WorkflowReceive, Task: id, Stage: EngineTaskSettled},
		{Workflow: WorkflowReceive, Task: id, Stage: EngineTaskStarted, Outcome: EngineOutcomeSuccess, FailureClass: EngineFailureNone},
		{Workflow: WorkflowReceive, Task: id, Stage: EngineTaskSettled, Outcome: EngineOutcomeFailed, FailureClass: EngineFailureUsage},
		{Workflow: WorkflowReceive, Task: id, Stage: EngineTaskSettled, Outcome: "unknown", FailureClass: EngineFailureNone},
		{Workflow: WorkflowReceive, Task: id, Stage: EngineTaskSettled, Outcome: EngineOutcomeSuccess, FailureClass: "unknown"},
	} {
		if _, err := NewEngineTaskObserved(spec); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("invalid ownership accepted: %+v", spec)
		}
	}
	if err := (EngineTaskObserved{}).Validate(); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("zero event = %v", err)
	}
	for _, reason := range []EngineStopReason{EngineNoStop, EngineCancelled, EngineShareStopped, EngineApplicationClosed} {
		if name, ok := reason.Name(); !ok || name == "" {
			t.Fatalf("reason = %v", reason)
		}
	}
	for _, trigger := range []EngineAdmissionTrigger{"", "peer_failed", "peer_detached", "relay_only_policy"} {
		if name, ok := trigger.Name(); !ok || name == "" {
			t.Fatalf("trigger = %v", trigger)
		}
	}
	for _, owner := range []EngineAdmissionTerminalOwner{"", "none", "lifecycle_close", "peer_session_fatal", "runtime_terminal", "resume_failure", "p2p_unavailable"} {
		if name, ok := owner.Name(); !ok || name == "" {
			t.Fatalf("owner = %v", owner)
		}
	}
}
