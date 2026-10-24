package eventproject

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/pionpeer"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestNativeProviderDiagnosticMessageBoundaries(t *testing.T) {
	for _, message := range []string{
		"", "ordinary warning", "invalid \xff text",
		strings.Repeat("界", appobs.NativeProviderDiagnosticMessageLimit),
	} {
		event, err := ProjectNativeObservation(appobs.WorkflowReceive, nativepeer.Observation{
			Provider: &pionpeer.Event{Milestone: "provider_diagnostic",
				Diagnostic: &pionpeer.Diagnostic{Scope: "ice", Severity: "warning", Message: message}},
		})
		if err != nil {
			t.Fatalf("log text caused observation loss: %v", err)
		}
		got := event.Facts().Diagnostic.Message
		if !utf8.ValidString(got) || len(got) > appobs.NativeProviderDiagnosticMessageLimit {
			t.Fatal("provider diagnostic escaped its bounded UTF-8 contract")
		}
		if utf8.ValidString(message) && len(message) <= appobs.NativeProviderDiagnosticMessageLimit && got != message {
			t.Fatal("bounded provider message changed")
		}
		if len(message) > appobs.NativeProviderDiagnosticMessageLimit && !strings.HasSuffix(got, "…") {
			t.Fatal("truncated provider diagnostic was not marked")
		}
	}
}
