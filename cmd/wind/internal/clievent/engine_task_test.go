package clievent

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
		command := CommandShare
		if stage == EngineGenerationChanged || stage == EngineAdmissionDecision {
			command = CommandGet
		}
		spec := EngineTaskSpec{Command: command, Task: id, At: time.Unix(5, 0), Stage: stage, ShareInstance: share, Session: session}
		if stage == EngineTaskSettled {
			spec.Outcome, spec.FailureClass = EngineOutcomeStopped, EngineFailureNone
		}
		event, err := NewEngineTaskObserved(spec)
		if err != nil {
			t.Fatal(err)
		}
		if event.Command() != command || event.Level() != LevelDebug || event.Facts() != spec || event.Facts().Task.String() != "application:42" {
			t.Fatal("observation lost task identity or milestone")
		}
		visitor := &exhaustiveVisitor{}
		if err := event.Accept(visitor); err != nil || visitor.visited != "engine_task" {
			t.Fatalf("visit = %q, %v", visitor.visited, err)
		}
		if err := event.Accept(nil); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("nil visitor = %v", err)
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
		{Command: CommandShare, Task: id, Stage: EngineTaskStage(255)},
		{Command: CommandShare, Task: id, Stage: EngineTaskStarted, StopReason: EngineStopReason(255)},
		{Command: CommandShare, Task: id, Stage: EngineSourceAcquired},
		{Command: CommandGet, Task: id, Stage: EngineGenerationChanged},
		{Command: CommandGet, Task: id, Stage: EngineTaskStarted, AdmissionTrigger: "unbounded-cause"},
		{Command: CommandGet, Task: id, Stage: EngineTaskStarted, AdmissionTerminalOwner: "unbounded-owner"},
		{Command: CommandGet, Task: id, Stage: EngineTaskSettled},
		{Command: CommandGet, Task: id, Stage: EngineTaskStarted, Outcome: EngineOutcomeSuccess, FailureClass: EngineFailureNone},
		{Command: CommandGet, Task: id, Stage: EngineTaskSettled, Outcome: EngineOutcomeFailed, FailureClass: EngineFailureUsage},
		{Command: CommandGet, Task: id, Stage: EngineTaskSettled, Outcome: "unknown", FailureClass: EngineFailureNone},
		{Command: CommandGet, Task: id, Stage: EngineTaskSettled, Outcome: EngineOutcomeSuccess, FailureClass: "unknown"},
	} {
		if _, err := NewEngineTaskObserved(spec); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("invalid ownership accepted: %+v", spec)
		}
	}
	if err := (EngineTaskObserved{}).Accept(&exhaustiveVisitor{}); !errors.Is(err, ErrInvalidEvent) {
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
