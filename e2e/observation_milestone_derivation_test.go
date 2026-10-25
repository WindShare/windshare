package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/observation/wire"
	"github.com/windshare/windshare/internal/testrun"
)

func TestObservationMilestonesRespectAdmissionAndSettlementSemantics(t *testing.T) {
	for _, test := range []struct {
		name, command, event string
		payload              any
		milestone            string
		outcome              testrun.Outcome
	}{
		{"share published", "share", "ready", wire.EmptyPayload{}, "sender_ready", testrun.OutcomeSucceeded},
		{"candidate is not admission", "share", "peer_attempt", diagnosticMilestonePayload{Stage: "datachannel-open"}, "", ""},
		{"TURN is not direct", "share", "peer_attempt", diagnosticMilestonePayload{Stage: "admitted", Route: "turn"}, "", ""},
		{"direct admission", "share", "peer_attempt", diagnosticMilestonePayload{Stage: "admitted", Route: "direct"}, "sender_direct_lane", testrun.OutcomeSucceeded},
		{"relay ownership", "get", "receive_relay_content_claimed", diagnosticMilestonePayload{}, "receiver_relay_content", testrun.OutcomeSucceeded},
		{"decision is not another claim", "get", "receive_admission_decision", diagnosticMilestonePayload{}, "", ""},
		{"cleanup start", "share", "share_workflow", diagnosticMilestonePayload{Stage: "share_stopping"}, "sender_stop", testrun.OutcomeStarted},
		{"cleanup failure", "share", "share_workflow", diagnosticMilestonePayload{Stage: "share_stopped", Failure: "unexpected"}, "sender_stop", testrun.OutcomeFailed},
		{"stopped relay is not terminal until settlement", "get", "warning", wire.WarningPayload{Failure: wire.Failure{Code: "relay_stopped"}}, "", ""},
		{"exhausted stopped join", "get", "engine_task_observed", wire.EngineTaskPayload{Stage: "task_settled", Outcome: appobs.EngineOutcomeFailed, Failure: &wire.Failure{Code: "relay_stopped"}}, "receiver_join_stopped", testrun.OutcomeFailed},
		{"initial registration is not recovery", "share", "relay_recovering", wire.RelayRecoveringPayload{State: "succeeded", Details: &wire.RelayRecoveryDetails{Generation: "1"}}, "", ""},
		{"re-registration", "share", "relay_recovering", wire.RelayRecoveringPayload{State: "succeeded", Details: &wire.RelayRecoveryDetails{Generation: "2"}}, "sender_relay_recovery", testrun.OutcomeSucceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(test.payload)
			if err != nil {
				t.Fatal(err)
			}
			got, relevant, err := (&observationMilestones{}).derive(observationRecord{Event: test.event, Command: test.command, Payload: payload})
			if err != nil || relevant != (test.milestone != "") || got.name != test.milestone || relevant && got.outcome != test.outcome {
				t.Fatalf("interpretation=%+v relevant=%v error=%v", got, relevant, err)
			}
			if test.name == "re-registration" && string(got.payload) != `{"connection_generation":2}` {
				t.Fatalf("recovery generation lost: %s", got.payload)
			}
		})
	}
}

func TestObservationMilestonesRejectLostOrTruncatedEvidence(t *testing.T) {
	ready := `{"schema_version":5,"event":"ready","command":"share","payload":{}}`
	summary := `{"schema_version":5,"event":"trace_summary","command":"share","payload":{"contract_rejected":"0","lifecycle_dropped":"0","writer_failed":false,"flush_failed":false,"schema_limited":false}}`
	for _, test := range []struct {
		name, data string
		complete   bool
	}{
		{"complete", ready + "\n" + summary + "\n", true},
		{"dropped lifecycle", ready + "\n" + strings.Replace(summary, `"lifecycle_dropped":"0"`, `"lifecycle_dropped":"1"`, 1) + "\n", false},
		{"truncated summary", ready + "\n" + summary, false},
		{"missing summary", ready + "\n", false},
		{"wrong command", strings.Replace(ready, `"share"`, `"get"`, 1) + "\n" + summary + "\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "observations.ndjson")
			if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			close(done)
			stream := observationMilestones{path: path, command: "share", done: done}
			defer stream.close()
			var err error
			count := 0
			for err == nil {
				_, err = stream.next(t.Context())
				if err == nil {
					count++
				}
			}
			if errors.Is(err, io.EOF) != test.complete || test.complete && count != 1 {
				t.Fatalf("events=%d completion=%v", count, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stream := observationMilestones{}
	if _, err := stream.next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func TestObservationMilestonesDoNotConfuseStoppedSessionWithFailedInitialJoin(t *testing.T) {
	stream := &observationMilestones{}
	_, relevant, err := stream.derive(observationRecord{Command: "get", Event: "receive_generation_changed", Payload: json.RawMessage(`{}`)})
	if err != nil || relevant {
		t.Fatalf("generation interpretation: relevant=%v error=%v", relevant, err)
	}
	_, relevant, err = stream.derive(observationRecord{Command: "get", Event: "engine_task_observed", Payload: json.RawMessage(`{"stage":"task_settled","outcome":"failed","failure":{"code":"relay_stopped"}}`)})
	if err != nil || relevant {
		t.Fatalf("established session reported as failed initial join: relevant=%v error=%v", relevant, err)
	}
}

type diagnosticMilestonePayload struct {
	Stage   string `json:"stage"`
	Route   string `json:"route"`
	Failure string `json:"failure"`
}
