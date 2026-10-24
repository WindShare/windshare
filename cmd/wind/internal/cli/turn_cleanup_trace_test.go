package cli

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		spec.TURN = &appobs.NativeTURNFacts{Provider: "test", Account: "owner", Profile: "primary", Cleanup: outcome, Reason: outcome.Reason(), Resources: &appobs.NativeTURNResources{OpenedSockets: 2, TransportSent: 100, TransportReceived: 50}}
		event, err := appobs.NewNativeConnectivityObserved(spec)
		if err != nil {
			t.Fatal(err)
		}
		if !recorder.Record(event) {
			t.Fatal("cleanup event rejected by recorder")
		}
		invalid := event.Facts()
		invalid.TURN.Cleanup = 0
		if _, err := appobs.NewNativeConnectivityObserved(invalid); err == nil {
			t.Fatal("untyped cleanup accepted")
		}
		invalid = event.Facts()
		invalid.TURN.Reason = "invented"
		if _, err := appobs.NewNativeConnectivityObserved(invalid); err == nil {
			t.Fatal("cleanup outcome/reason mismatch accepted")
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
		binding *appobs.NativeTURNBinding
		route   *appobs.NativeTURNRoute
	}{
		{"turn_binding", &appobs.NativeTURNBinding{Status: "mismatch", Code: 400, Channel: 0x4000, Allocation: netip.MustParseAddrPort("127.0.0.1:10"), Peer: netip.MustParseAddrPort("127.0.0.1:20")}, nil},
		{"turn_route", nil, &appobs.NativeTURNRoute{Server: "turn:localhost:3478", Outcome: "started"}},
	} {
		spec := nativeFactSpec(appobs.WorkflowShare, evidence.kind)
		spec.TURN = &appobs.NativeTURNFacts{Provider: "test", Account: "owner", Profile: "primary", Resources: &appobs.NativeTURNResources{ActiveSockets: 1}, Binding: evidence.binding, Route: evidence.route}
		event, err := appobs.NewNativeConnectivityObserved(spec)
		if err != nil || !recorder.Record(event) {
			t.Fatalf("recovery evidence rejected: %v", err)
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
