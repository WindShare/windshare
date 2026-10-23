package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestNativeProviderDiagnosticsReachTrace(t *testing.T) {
	target, err := runtrace.ExactFile(filepath.Join(t.TempDir(), "provider.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := runtrace.Open(target, clievent.CommandGet, runtrace.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recorder.Close() })
	session, _ := appobs.NewProtocolSessionID(testShareBytes16(1))
	path, _ := appobs.NewPeerPathID(testShareBytes16(2))
	attempt, _ := appobs.NewPeerAttemptID(testShareBytes16(3))
	for _, scope := range []string{"ice", "turnc"} {
		for _, severity := range []string{"warning", "error"} {
			diagnostic := &appobs.NativeProviderDiagnosticFacts{Scope: scope, Severity: severity, Message: "provider diagnostic detail"}
			event, err := clievent.NewNativeConnectivityObserved(appobs.NativeConnectivitySpec{Workflow: appobs.WorkflowReceive, Session: session, Path: path, Attempt: attempt, AttemptSequence: 4, NetworkGeneration: 5, Profile: "ice-0123abcd", Side: "receiver", Kind: "provider_diagnostic", State: "unknown", Diagnostic: diagnostic})
			if err != nil {
				t.Fatal(err)
			}
			diagnostic.Message = "changed producer storage"
			facts := event.Facts()
			facts.Diagnostic.Message = "changed consumer snapshot"
			if !recorder.Record(event) {
				t.Fatal("provider diagnostic rejected by recorder")
			}
		}
	}
	if status := recorder.Close(); !status.Complete || status.LifecycleDropped != 0 {
		t.Fatalf("provider diagnostics lost: %+v", status)
	}
	body, err := os.ReadFile(recorder.Path())
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for line := range strings.SplitSeq(strings.TrimSpace(string(body)), "\n") {
		var record struct {
			Event       string
			Correlation map[string]string
			Payload     struct {
				Kind, State     string
				AttemptSequence string `json:"attempt_sequence"`
				Diagnostic      *appobs.NativeProviderDiagnosticFacts
			}
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Event != "native_connectivity" {
			continue
		}
		d := record.Payload.Diagnostic
		if record.Payload.Kind != "provider_diagnostic" || record.Payload.State != "unknown" ||
			record.Payload.AttemptSequence != "4" || d == nil || d.Message != "provider diagnostic detail" ||
			record.Correlation["protocol_session_id"] == "" || record.Correlation["peer_path_id"] == "" ||
			record.Correlation["peer_attempt_id"] == "" {
			t.Fatalf("provider diagnostic lost attribution or content: %s", line)
		}
		seen[d.Scope+"/"+d.Severity] = true
	}
	for _, key := range []string{"ice/warning", "ice/error", "turnc/warning", "turnc/error"} {
		if !seen[key] {
			t.Errorf("missing provider diagnostic %s", key)
		}
	}
}
