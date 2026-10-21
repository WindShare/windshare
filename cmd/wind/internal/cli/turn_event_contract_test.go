package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine"
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
			share, receive := newShareProjection(runtime), newGetObservation(runtime)
			observe := func(value nativepeer.Observation) {
				if command == clievent.CommandShare {
					app.observeEngineShare(share, engine.ShareObservation{Native: &value})
				} else {
					value.Subject.Side = nativepeer.SideReceiver
					app.projectReceiveObservation(receive, engine.ReceiveNativeObserved{Value: value})
				}
			}
			for _, milestone := range turnpath.Milestones() {
				observe(turnContractObservation(milestone))
			}
			unavailable := turnContractObservation(turnpath.MilestoneUnavailable)
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
			for _, milestone := range turnpath.Milestones() {
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

func TestTURNRejectionKeepsEvidenceAndDoesNotBecomeCapacityLoss(t *testing.T) {
	var stderr bytes.Buffer
	app := &App{Stderr: &stderr}
	path := filepath.Join(t.TempDir(), "rejection.ndjson")
	runtime, err := app.newCommandRuntime(clievent.CommandShare, testExactTraceOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Close)
	share := newShareProjection(runtime)
	invalid := turnContractObservation(turnpath.Milestone(255))
	app.observeEngineShare(share, engine.ShareObservation{Native: &invalid})
	valid := turnContractObservation(turnpath.MilestoneTransportSelection)
	app.observeEngineShare(share, engine.ShareObservation{Native: &valid})
	runtime.Close()

	var rejectionSeen, validSeen, summarySeen bool
	for _, row := range readTURNContractRows(t, path) {
		switch row.Event {
		case "observer_loss":
			var payload struct {
				Reason, Count string
				Rejection     struct {
					Event       string `json:"source_event"`
					Source      string `json:"source_location"`
					Stage       string `json:"source_stage"`
					Field, Rule string
					Evidence    []clievent.RejectionField
				}
			}
			if err := json.Unmarshal(row.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			rejection := payload.Rejection
			evidence := make(map[string]string)
			for _, field := range rejection.Evidence {
				evidence[field.Field] = field.Value
			}
			if payload.Reason != "event_contract_rejection" || payload.Count != "1" ||
				rejection.Event != "native_connectivity" || rejection.Source != "commandprojection.ProjectNativeObservation" ||
				rejection.Stage != "turn_path" || rejection.Field != "turn.milestone" || rejection.Rule != "known_enum" ||
				evidence["turn.milestone"] != "255" || evidence["peer_path_id"] == "" ||
				evidence["turn.profile"] != "primary" {
				t.Fatalf("rejection cannot explain the source event: %s", row.Payload)
			}
			rejectionSeen = true
		case "native_connectivity":
			validSeen = true
		case "trace_summary":
			var summary struct {
				Incomplete bool
				Rejected   string `json:"contract_rejected"`
				Dropped    string `json:"lifecycle_dropped"`
			}
			if err := json.Unmarshal(row.Payload, &summary); err != nil {
				t.Fatal(err)
			}
			if !summary.Incomplete || summary.Rejected != "1" || summary.Dropped != "0" {
				t.Fatalf("rejection mixed with capacity loss: %s", row.Payload)
			}
			summarySeen = true
		}
	}
	if !rejectionSeen || !validSeen || !summarySeen || strings.Count(stderr.String(), "Trace is incomplete") != 1 ||
		!strings.Contains(stderr.String(), "rejected") {
		t.Fatalf("rejection lost evidence, stopped recording, or lost its warning: %s", stderr.String())
	}
}

func turnContractObservation(milestone turnpath.Milestone) nativepeer.Observation {
	subject := turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}}
	return nativepeer.Observation{
		Subject: nativepeer.Subject{ProtocolSessionID: subject.SessionID, PeerPathID: subject.PathID, Side: nativepeer.SideSender},
		TURN: &turnpath.Event{At: time.Unix(1, 0), Subject: subject, Provider: "test", Account: "owner",
			Profile: "primary", Milestone: milestone, Reason: "contract evidence"},
	}
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
