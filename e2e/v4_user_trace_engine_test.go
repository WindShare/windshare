package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const v4MaximumEngineTaskIDBytes = 128

type v4EngineTaskFacts struct {
	TaskID                 string `json:"engine_task_id"`
	Stage                  string `json:"stage"`
	StopReason             string `json:"stop_reason"`
	Outcome                string `json:"outcome"`
	FailureClass           string `json:"failure_class"`
	ShareInstance          string `json:"share_instance"`
	ReceiveOperationID     string `json:"receive_operation_id"`
	TransferJobID          string `json:"transfer_job_id"`
	PreviousSessionID      string `json:"previous_protocol_session_id"`
	AdmissionTrigger       string `json:"admission_trigger"`
	AdmissionTerminalOwner string `json:"admission_terminal_owner"`
}

func validateV4EngineTaskRecord(t *testing.T, record v4TraceRecord) v4EngineTaskFacts {
	t.Helper()
	encoded, err := json.Marshal(record.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var facts v4EngineTaskFacts
	if err := json.Unmarshal(encoded, &facts); err != nil {
		t.Fatal(err)
	}
	if err := facts.validate(record.Command, record.Correlation); err != nil {
		t.Fatalf("engine task observation: %v", err)
	}
	return facts
}

func (facts v4EngineTaskFacts) validate(command string, correlation *v4TraceCorrelation) error {
	if len(facts.TaskID) == 0 || len(facts.TaskID) > v4MaximumEngineTaskIDBytes {
		return fmt.Errorf("invalid task identity length")
	}
	for _, character := range facts.TaskID {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune(":-_.", character) {
			continue
		}
		return fmt.Errorf("invalid task identity character")
	}
	switch facts.StopReason {
	case "none", "cancelled", "share_stopped", "application_closed":
	default:
		return fmt.Errorf("unknown stop reason %q", facts.StopReason)
	}
	switch facts.Stage {
	case "task_started", "task_stopping", "task_settled":
	case "source_acquiring", "source_acquired", "source_failed":
		if command != "share" || facts.ShareInstance == "" {
			return fmt.Errorf("source acquisition lacks share ownership")
		}
	case "generation_changed", "admission_decision":
		if command != "get" || correlation == nil || correlation.ProtocolSessionID == "" {
			return fmt.Errorf("receive decision lacks protocol session ownership")
		}
	default:
		return fmt.Errorf("unknown stage %q", facts.Stage)
	}
	if facts.Stage == "admission_decision" {
		switch facts.AdmissionTrigger {
		case "none", "peer_failed", "peer_detached", "relay_only_policy":
		default:
			return fmt.Errorf("unknown admission trigger %q", facts.AdmissionTrigger)
		}
		switch facts.AdmissionTerminalOwner {
		case "none", "lifecycle_close", "peer_session_fatal", "runtime_terminal", "resume_failure", "p2p_unavailable":
		default:
			return fmt.Errorf("unknown admission terminal owner %q", facts.AdmissionTerminalOwner)
		}
	} else if facts.AdmissionTrigger != "" || facts.AdmissionTerminalOwner != "" {
		return fmt.Errorf("admission context is attached to %q", facts.Stage)
	}
	if facts.PreviousSessionID != "" && facts.Stage != "generation_changed" {
		return fmt.Errorf("previous protocol session is attached to %q", facts.Stage)
	}
	if facts.Stage == "task_settled" {
		switch facts.Outcome {
		case "success", "partial", "paused", "cancelled", "stopped", "failed":
		default:
			return fmt.Errorf("unknown task outcome %q", facts.Outcome)
		}
		switch facts.FailureClass {
		case "none", "local", "network", "usage", "source_drift":
		default:
			return fmt.Errorf("unknown task failure class %q", facts.FailureClass)
		}
	} else if facts.Outcome != "" || facts.FailureClass != "" {
		return fmt.Errorf("terminal result is attached to %q", facts.Stage)
	}
	return nil
}

// The ordinary process scenario has one native task per command and no injected
// observation loss. Its trace must connect task ownership to real workflow facts.
func assertV4CriticalEngineTask(t *testing.T, command string, records []v4TraceRecord) string {
	t.Helper()
	taskID, shareInstance := "", ""
	stages := make(map[string]int)
	protocolSessions := make(map[string]bool)
	transferOwners := make(map[string]bool)
	for _, record := range records {
		if record.Event == "protocol_operation" && record.Correlation != nil {
			protocolSessions[record.Correlation.ProtocolSessionID] = true
		}
		if record.Event == "transfer_lifecycle" || record.Event == "transfer_progress" {
			transferOwners[v4TraceStringField(t, record.Payload, "receive_operation_id")+"/"+v4TraceStringField(t, record.Payload, "transfer_job_id")] = true
		}
	}
	for _, record := range records {
		if record.Event != "engine_task_observed" {
			continue
		}
		facts := validateV4EngineTaskRecord(t, record)
		if taskID == "" {
			taskID = facts.TaskID
			if facts.Stage != "task_started" {
				t.Fatal("engine task facts precede task creation")
			}
		}
		if facts.TaskID != taskID {
			t.Fatal("one CLI operation changed engine task identity")
		}
		if stages["task_settled"] != 0 {
			t.Fatal("engine task emitted facts after settlement")
		}
		stages[facts.Stage]++
		switch facts.Stage {
		case "source_acquiring":
			shareInstance = facts.ShareInstance
		case "source_acquired":
			if stages["source_acquiring"] != 1 || facts.ShareInstance != shareInstance {
				t.Fatal("source acquisition changed task-owned share identity")
			}
		case "source_failed":
			t.Fatal("successful critical share reported source acquisition failure")
		case "generation_changed":
			if !protocolSessions[record.Correlation.ProtocolSessionID] {
				t.Fatal("engine receive generation has no matching protocol operations")
			}
		case "admission_decision":
			if !protocolSessions[record.Correlation.ProtocolSessionID] ||
				!transferOwners[facts.ReceiveOperationID+"/"+facts.TransferJobID] {
				t.Fatal("engine admission is not correlated to its protocol session and transfer")
			}
		case "task_settled":
			wantReason := "none"
			wantOutcome := "success"
			if command == "share" {
				wantReason = "share_stopped"
				wantOutcome = "stopped"
			}
			if facts.StopReason != wantReason {
				t.Fatalf("critical %s task stop reason = %q, want %q", command, facts.StopReason, wantReason)
			}
			if facts.Outcome != wantOutcome || facts.FailureClass != "none" {
				t.Fatalf("critical %s task outcome = %q/%q", command, facts.Outcome, facts.FailureClass)
			}
			if _, failed := record.Payload["failure"]; failed {
				t.Fatal("successful critical task retained an execution failure")
			}
			if _, failed := record.Payload["cleanup_failure"]; failed {
				t.Fatal("successful critical task retained a cleanup failure")
			}
		}
	}
	if stages["task_started"] != 1 || stages["task_settled"] != 1 {
		t.Fatalf("critical %s task creation/settlement count = %d/%d", command, stages["task_started"], stages["task_settled"])
	}
	if command == "share" && (stages["source_acquiring"] != 1 || stages["source_acquired"] != 1 || stages["task_stopping"] != 1) {
		t.Fatal("critical share omitted source acquisition or explicit stop")
	}
	if command == "get" && stages["generation_changed"] == 0 {
		t.Fatal("critical receive omitted protocol generation ownership")
	}
	return taskID
}

func TestUserTraceV4EngineTaskFacts(t *testing.T) {
	runID, session, previous := v4CapacityBase64ID(1), v4CapacityBase64ID(2), v4CapacityBase64ID(3)
	for _, stage := range []string{"task_started", "task_stopping", "task_settled", "source_acquiring", "source_acquired", "source_failed", "generation_changed", "admission_decision"} {
		t.Run(stage, func(t *testing.T) {
			command := "share"
			payload := map[string]any{
				"engine_task_id": "application:1", "observed_at": "2026-09-18T00:00:00Z",
				"stage": stage, "stop_reason": "none",
			}
			switch stage {
			case "task_stopping":
				payload["stop_reason"] = "share_stopped"
			case "task_settled":
				payload["outcome"], payload["failure_class"] = "failed", "network"
				payload["failure"] = map[string]any{"code": "network", "message_key": "failure.network"}
				payload["cleanup_failure"] = map[string]any{"code": "io", "message_key": "failure.io"}
			case "source_acquiring", "source_acquired", "source_failed":
				payload["share_instance"] = v4CapacityBase64ID(4)
			case "generation_changed", "admission_decision":
				command = "get"
				payload["receive_operation_id"], payload["transfer_job_id"] = v4CapacityBase64ID(5), v4CapacityBase64ID(6)
				if stage == "generation_changed" {
					payload["previous_protocol_session_id"] = previous
				} else {
					payload["admission_trigger"], payload["admission_terminal_owner"] = "relay_only_policy", "none"
				}
			}
			record := v4CapacityTraceRecord(1, command, "engine_task_observed", runID, payload)
			if command == "get" {
				record["correlation"] = map[string]any{"protocol_session_id": session}
			}
			v4ReadTraceVectors(t, command, []map[string]any{record})
		})
	}
}

func TestUserTraceV4EngineTaskRejectsUnknownDecisionContext(t *testing.T) {
	valid := v4EngineTaskFacts{
		TaskID: "application:1", Stage: "admission_decision", StopReason: "none",
		AdmissionTrigger: "relay_only_policy", AdmissionTerminalOwner: "none",
	}
	session := &v4TraceCorrelation{ProtocolSessionID: v4CapacityBase64ID(1)}
	if err := valid.validate("get", session); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*v4EngineTaskFacts){
		"unknown stage":            func(f *v4EngineTaskFacts) { f.Stage = "future_stage" },
		"unknown stop":             func(f *v4EngineTaskFacts) { f.StopReason = "future_stop" },
		"unknown trigger":          func(f *v4EngineTaskFacts) { f.AdmissionTrigger = "future_trigger" },
		"unknown owner":            func(f *v4EngineTaskFacts) { f.AdmissionTerminalOwner = "future_owner" },
		"empty task":               func(f *v4EngineTaskFacts) { f.TaskID = "" },
		"invalid task":             func(f *v4EngineTaskFacts) { f.TaskID = "application/1" },
		"oversized task":           func(f *v4EngineTaskFacts) { f.TaskID = strings.Repeat("x", v4MaximumEngineTaskIDBytes+1) },
		"wrong stage context":      func(f *v4EngineTaskFacts) { f.Stage = "task_started" },
		"wrong generation context": func(f *v4EngineTaskFacts) { f.PreviousSessionID = v4CapacityBase64ID(2) },
	} {
		t.Run(name, func(t *testing.T) {
			facts := valid
			change(&facts)
			if facts.validate("get", session) == nil {
				t.Fatal("accepted malformed engine task context")
			}
		})
	}
	if valid.validate("get", nil) == nil || valid.validate("share", session) == nil {
		t.Fatal("accepted receive decision without receiver session ownership")
	}
	source := v4EngineTaskFacts{TaskID: valid.TaskID, Stage: "source_acquired", StopReason: "none"}
	if source.validate("share", nil) == nil {
		t.Fatal("accepted source acquisition without share identity")
	}
}
