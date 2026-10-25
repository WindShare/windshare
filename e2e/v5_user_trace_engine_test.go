package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const v5MaximumEngineTaskIDBytes = 128

type v5EngineTaskFacts struct {
	TaskID       string `json:"engine_task_id"`
	Stage        string `json:"stage"`
	StopReason   string `json:"stop_reason"`
	Outcome      string `json:"outcome"`
	FailureClass string `json:"failure_class"`
}

func validateV5EngineTaskRecord(t *testing.T, record v5TraceRecord) v5EngineTaskFacts {
	t.Helper()
	encoded, err := json.Marshal(record.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var facts v5EngineTaskFacts
	if err := json.Unmarshal(encoded, &facts); err != nil {
		t.Fatal(err)
	}
	if err := facts.validate(); err != nil {
		t.Fatalf("engine task observation: %v", err)
	}
	return facts
}

func (facts v5EngineTaskFacts) validate() error {
	if len(facts.TaskID) == 0 || len(facts.TaskID) > v5MaximumEngineTaskIDBytes {
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
	default:
		return fmt.Errorf("unknown stage %q", facts.Stage)
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
func assertV5CriticalEngineTask(t *testing.T, command string, records []v5TraceRecord) string {
	t.Helper()
	taskID := ""
	stages := map[string]int{}
	sessions := map[string]bool{}
	for _, record := range records {
		if record.Event == "protocol_operation" && record.Correlation != nil {
			sessions[record.Correlation.ProtocolSessionID] = true
		}
		if record.Event != "engine_task_observed" {
			continue
		}
		facts := validateV5EngineTaskRecord(t, record)
		if taskID == "" {
			taskID = facts.TaskID
			if facts.Stage != "task_started" {
				t.Fatal("task creation missing")
			}
		}
		if taskID != facts.TaskID || stages["task_settled"] != 0 {
			t.Fatal("task identity or lifecycle changed")
		}
		stages[facts.Stage]++
		if facts.Stage == "task_settled" {
			reason, outcome := "none", "success"
			if command == "share" {
				reason, outcome = "share_stopped", "stopped"
			}
			if facts.StopReason != reason || facts.Outcome != outcome || facts.FailureClass != "none" {
				t.Fatalf("task settled: %+v", facts)
			}
			for _, key := range []string{"failure", "cleanup_failure"} {
				if _, present := record.Payload[key]; present {
					t.Fatalf("unexpected %s", key)
				}
			}
		}
	}
	if stages["task_started"] != 1 || stages["task_settled"] != 1 {
		t.Fatal("task creation/settlement missing")
	}
	acquired, shareID, generations := false, "", 0
	for _, record := range records {
		if record.Scope.TaskID == "" {
			continue
		}
		if record.Scope.TaskID != taskID || record.Scope.EngineID == "" {
			t.Fatal("diagnostic lost task ownership")
		}
		if record.Event == "share_workflow" {
			switch v5TraceStringField(t, record.Payload, "stage") {
			case "source_acquiring":
				shareID = record.Correlation.ShareInstanceID
			case "source_acquired":
				acquired = shareID != "" && record.Correlation.ShareInstanceID == shareID
			case "source_acquisition_failed":
				t.Fatal("source acquisition failed")
			}
		}
		if record.Event == "receive_generation_changed" {
			generations++
			if record.Correlation == nil || !sessions[record.Correlation.ProtocolSessionID] {
				t.Fatal("generation lost protocol correlation")
			}
		}
	}
	if command == "share" && (!acquired || stages["task_stopping"] != 1) {
		t.Fatal("source acquisition or stop missing")
	}
	if command == "get" && generations == 0 {
		t.Fatal("generation ownership missing")
	}
	return taskID
}
