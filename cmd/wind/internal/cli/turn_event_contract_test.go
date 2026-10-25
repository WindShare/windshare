package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/turnpath"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/connectionpath"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestEveryTURNMilestoneReachesCommandTrace(t *testing.T) {
	for _, command := range []clievent.Command{clievent.CommandShare, clievent.CommandGet} {
		name, _ := command.Name()
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			app := &App{Stderr: &stderr}
			path := filepath.Join(t.TempDir(), "turn.ndjson")
			runtime, err := app.newCommandRuntime(command, testExactTraceOptions(path))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(runtime.Close)
			observe := func(value nativepeer.Observation) {
				if command == clievent.CommandGet {
					value.Subject.Side = "receiver"
				}
				runtime.diagnosticSink().Record(value)
			}
			for _, milestone := range connectionpath.Milestones() {
				observe(turnContractObservation(milestone))
			}
			unavailable := turnContractObservation(connectionpath.MilestoneUnavailable)
			unavailable.TURN.Provider, unavailable.TURN.Account = "", ""
			observe(unavailable)
			runtime.Close()

			seen := make(map[string]bool)
			var summarySeen bool
			for _, row := range readTURNContractRows(t, path) {
				switch row.Event {
				case "native_connectivity":
					var payload struct {
						TURN struct{ Milestone, Provider, Account, Profile, Reason string }
					}
					if err := json.Unmarshal(row.Payload, &payload); err != nil {
						t.Fatal(err)
					}
					if payload.TURN.Milestone == "" || payload.TURN.Profile != "primary" ||
						payload.TURN.Reason != "contract evidence" || row.Correlation["protocol_session_id"] == "" ||
						row.Correlation["peer_path_id"] == "" {
						t.Fatalf("TURN facts lost in trace: %s", row.Payload)
					}
					seen[payload.TURN.Milestone] = true
				case "observer_loss":
					t.Fatalf("source-owned milestone was rejected: %s", row.Payload)
				case "trace_summary":
					var summary struct {
						Incomplete bool
						Rejected   string `json:"contract_rejected"`
						Dropped    string `json:"lifecycle_dropped"`
					}
					if err := json.Unmarshal(row.Payload, &summary); err != nil {
						t.Fatal(err)
					}
					if summary.Incomplete || summary.Rejected != "0" || summary.Dropped != "0" {
						t.Fatalf("valid lifecycle made trace incomplete: %s", row.Payload)
					}
					summarySeen = true
				}
			}
			for _, milestone := range connectionpath.Milestones() {
				if !seen[milestone.String()] {
					t.Errorf("milestone missing from trace: %s", milestone)
				}
			}
			if !summarySeen || strings.Contains(stderr.String(), "Trace is incomplete") {
				t.Fatalf("valid TURN lifecycle reported unhealthy trace: %s", stderr.String())
			}
		})
	}
}

func TestNewNativeMilestoneReachesTraceWithoutConsumerRegistration(t *testing.T) {
	var stderr bytes.Buffer
	path := filepath.Join(t.TempDir(), "future.ndjson")
	runtime, err := (&App{Stderr: &stderr}).newCommandRuntime(clievent.CommandShare, testExactTraceOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	runtime.diagnosticSink().Record(nativeFactSpec(appobs.WorkflowShare, "future_source_milestone"))
	runtime.Close()
	seen := false
	for _, row := range readTURNContractRows(t, path) {
		if row.Event == "native_connectivity" {
			var payload struct{ Kind string }
			if err := json.Unmarshal(row.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			seen = payload.Kind == "future_source_milestone"
		}
	}
	if !seen || strings.Contains(stderr.String(), "Trace is incomplete") {
		t.Fatalf("future source record lost or rejected: %s", stderr.String())
	}
}

func turnContractObservation(milestone connectionpath.Milestone) nativepeer.Observation {
	value := nativeFactSpec(appobs.WorkflowShare, "turn_path")
	value.Provider = nil
	value.TURN = &turnpath.Event{At: time.Unix(1, 0), Provider: "test", Account: "owner", Profile: "primary", Milestone: milestone, Reason: "contract evidence"}
	return value
}

type turnContractRow struct {
	Event       string
	Correlation map[string]string
	Payload     json.RawMessage
}

func readTURNContractRows(t *testing.T, path string) []turnContractRow {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []turnContractRow
	for line := range bytes.SplitSeq(bytes.TrimSpace(content), []byte{'\n'}) {
		var row turnContractRow
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}
