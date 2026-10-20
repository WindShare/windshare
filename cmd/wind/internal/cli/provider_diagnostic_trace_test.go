package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/transport/webrtc/provider"
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
	subject := nativepeer.Subject{
		ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2}, AttemptID: [16]byte{3},
		AttemptSequence: 4, NetworkGenerationID: 5, ICEProfileID: "ice-0123abcd", Side: nativepeer.SideReceiver,
	}
	for _, scope := range []string{"ice", "turnc"} {
		for _, severity := range []string{"warning", "error"} {
			diagnostic := &provider.Diagnostic{Scope: scope, Severity: severity, Message: "provider diagnostic detail"}
			event, err := projectNativeObservation(clievent.CommandGet, nativepeer.Observation{
				Subject: subject, Provider: &provider.Event{Milestone: "provider_diagnostic", Diagnostic: diagnostic},
			})
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
				Diagnostic      *clievent.NativeProviderDiagnosticFacts
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

func TestNativeProviderDiagnosticMessageBoundaries(t *testing.T) {
	for _, message := range []string{
		"", "ordinary warning", "invalid \xff text",
		strings.Repeat("界", clievent.NativeProviderDiagnosticMessageLimit),
	} {
		event, err := projectNativeObservation(clievent.CommandGet, nativepeer.Observation{
			Provider: &provider.Event{Milestone: "provider_diagnostic",
				Diagnostic: &provider.Diagnostic{Scope: "ice", Severity: "warning", Message: message}},
		})
		if err != nil {
			t.Fatalf("log text caused observation loss: %v", err)
		}
		got := event.Facts().Diagnostic.Message
		if !utf8.ValidString(got) || len(got) > clievent.NativeProviderDiagnosticMessageLimit {
			t.Fatal("provider diagnostic escaped its bounded UTF-8 contract")
		}
		if utf8.ValidString(message) && len(message) <= clievent.NativeProviderDiagnosticMessageLimit && got != message {
			t.Fatal("bounded provider message changed")
		}
		if len(message) > clievent.NativeProviderDiagnosticMessageLimit && !strings.HasSuffix(got, "…") {
			t.Fatal("truncated provider diagnostic was not marked")
		}
	}
}
