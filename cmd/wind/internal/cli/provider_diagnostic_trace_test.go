package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/pionpeer"
	"github.com/windshare/windshare/internal/diaglog"
)

func TestNativeProviderDiagnosticsReachTrace(t *testing.T) {
	target, err := diaglog.ExactFile(filepath.Join(t.TempDir(), "provider.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := diaglog.OpenRun(target, commandGet, diaglog.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recorder.CloseRun() })
	for _, scope := range []string{"ice", "turnc"} {
		for _, severity := range []string{"warning", "error"} {
			diagnostic := &pionpeer.Diagnostic{Scope: scope, Severity: severity, Message: "provider diagnostic detail"}
			source := nativeFactSpec(nativepeer.SideReceiver, "provider_diagnostic")
			source.Subject.AttemptID, source.Subject.AttemptSequence, source.Subject.NetworkGenerationID, source.Subject.ICEProfileID = [16]byte{3}, 4, 5, "ice-0123abcd"
			source.Provider.Diagnostic = diagnostic
			rec := source.DiagnosticRecord()
			diagnostic.Message = "changed producer storage"
			entry := diaglog.RunRecord{Level: "debug", Event: rec.Name(), Payload: rec.Payload()}
			if corr := rec.Correlation(); corr.Present() {
				entry.Correlation = corr
			}
			if !recorder.RecordRun(entry) {
				t.Fatal("provider diagnostic rejected by recorder")
			}
		}
	}
	if status := recorder.CloseRun(); !status.Complete() || status.Dropped != 0 {
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
				Diagnostic      *pionpeer.Diagnostic
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
