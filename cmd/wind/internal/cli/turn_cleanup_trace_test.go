package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pion/turn/v5"
	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/pionpeer"
	"github.com/windshare/windshare/connectivity/turnroute"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/core/connectionpath"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestTURNCleanupProviderFactsReachCLITraceWithReasonAndResources(t *testing.T) {
	target, err := runtrace.ExactFile(filepath.Join(t.TempDir(), "cleanup.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := runtrace.Open(target, clievent.CommandShare, runtrace.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recorder.Close() })
	for _, outcome := range []connectionpath.CleanupOutcome{connectionpath.CleanupProtocol, connectionpath.CleanupDeadline} {
		spec := nativeFactSpec(appobs.WorkflowShare, outcome.EventKind())
		spec.Provider.TURNService = &iceconfig.Identity{Provider: "test", Account: "owner", Profile: "primary"}
		spec.Provider.TURNCleanup = outcome
		spec.Provider.TURN = &pionpeer.TURNResourceSnapshot{OpenedSockets: 2, TransportSent: 100, TransportReceived: 50}
		if !recorder.Record(clievent.Diagnostic{Owner: clievent.CommandShare, Record: spec.DiagnosticRecord()}) {
			t.Fatal("cleanup source rejected")
		}

	}
	status := recorder.Close()
	if !status.Complete || status.LifecycleDropped != 0 {
		t.Fatalf("trace incomplete: %+v", status)
	}
	body, err := os.ReadFile(recorder.Path())
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(body)), "\n") {
		var wire struct {
			Event       string `json:"event"`
			Correlation struct {
				Session string `json:"protocol_session_id"`
				Path    string `json:"peer_path_id"`
			} `json:"correlation"`
			Payload struct {
				Kind, State string
				TURN        struct {
					Reason    string
					Resources struct {
						Active string `json:"active_sockets"`
						Sent   string `json:"transport_sent_bytes"`
					} `json:"resources"`
				} `json:"turn"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &wire); err != nil {
			t.Fatal(err)
		}
		if wire.Event != "native_connectivity" {
			continue
		}
		if wire.Payload.State != "unknown" || wire.Payload.TURN.Resources.Active != "0" || wire.Payload.TURN.Resources.Sent != "100" ||
			wire.Correlation.Session == "" || wire.Correlation.Path == "" {
			t.Fatalf("lost cleanup facts: %s", line)
		}
		found[wire.Payload.Kind] = wire.Payload.TURN.Reason
	}
	if len(found) != 2 || found[connectionpath.CleanupDeadline.EventKind()] != "protocol_cleanup_deadline" || found[connectionpath.CleanupProtocol.EventKind()] != "" {
		t.Fatalf("cleanup distinction lost: %+v", found)
	}
}

func TestTURNRecoveryEvidenceReachesTrace(t *testing.T) {
	target, err := runtrace.ExactFile(filepath.Join(t.TempDir(), "recovery.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := runtrace.Open(target, clievent.CommandShare, runtrace.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recorder.Close() })
	for _, evidence := range []struct {
		kind    string
		binding *turn.BindingEvent
		route   *turnroute.Event
	}{
		{"turn_binding", &turn.BindingEvent{Status: turn.BindingMismatch, Code: 400, Channel: 0x4000, RelayedAddress: "127.0.0.1:10", PeerAddress: "127.0.0.1:20"}, nil},
		{"turn_route", nil, &turnroute.Event{Server: "turn:localhost:3478", Outcome: "started"}},
	} {
		spec := nativeFactSpec(appobs.WorkflowShare, evidence.kind)
		spec.Provider.TURNService = &iceconfig.Identity{Provider: "test", Account: "owner", Profile: "primary"}
		spec.Provider.TURN = &pionpeer.TURNResourceSnapshot{ActiveSockets: 1}
		spec.Provider.TURNBinding, spec.Provider.TURNRoute = evidence.binding, evidence.route
		if !recorder.Record(clievent.Diagnostic{Owner: clievent.CommandShare, Record: spec.DiagnosticRecord()}) {
			t.Fatal("recovery source rejected")
		}

	}
	recorder.Close()
	body, err := os.ReadFile(recorder.Path())
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(body)), "\n") {
		var record struct {
			Payload struct {
				Kind string
				TURN struct {
					Binding *struct {
						Status, Peer, Allocation string
						Code                     int
					}
					Route *struct{ Server, Local, Outcome string }
				}
			}
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		switch record.Payload.Kind {
		case "turn_binding":
			binding := record.Payload.TURN.Binding
			if binding == nil || binding.Status != "mismatch" || binding.Code != 400 ||
				binding.Peer != "127.0.0.1:20" || binding.Allocation != "127.0.0.1:10" {
				t.Fatalf("binding attribution lost: %s", line)
			}
			found++
		case "turn_route":
			route := record.Payload.TURN.Route
			if route == nil || route.Server != "turn:localhost:3478" || route.Local != "system" || route.Outcome != "started" {
				t.Fatalf("route decision lost: %s", line)
			}
			found++
		}
	}
	if found != 2 {
		t.Fatalf("missing recovery evidence: %d", found)
	}
}
