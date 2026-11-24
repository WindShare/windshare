package humanoutput

import (
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/engine"
)

func TestSharingSubjectUsesOnlySelectedRootFacts(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		subject SharingSubject
		want    string
	}{
		{"file", SharingSubject{Kind: SharingFile, Name: "photo.jpg", FileBytes: 8_200_000}, "Sharing: photo.jpg (file, 8.2 MB)"},
		{"directory", SharingSubject{Kind: SharingDirectory, Name: "photos"}, "Sharing: photos/ (directory)"},
		{"multiple", SharingSubject{Kind: SharingMultiple, SelectedItems: 3}, "Sharing: 3 selected items"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := lineText(formatSharingSubject(test.subject, SelectSymbols(false))); !strings.Contains(got, test.want) {
				t.Fatalf("subject line = %q, want substring %q", got, test.want)
			}
		})
	}
}

func TestFallbackUsesUserFacingPathNames(t *testing.T) {
	t.Parallel()
	event := engine.Fallback{
		From:    engine.TransportWebRTC,
		To:      engine.TransportRelay,
		Failure: engine.Failure{Code: engine.FailureDirectUnavailable},
	}
	if got := lineText(formatFallback(event, SelectSymbols(false))); !strings.Contains(got, "WebRTC path unavailable; using Relay") {
		t.Fatalf("fallback line = %q", got)
	}
}

func TestFailedRelayRecoveryRemainsAWarningWhenRedirected(t *testing.T) {
	origin := engine.RelayOrigin{Scheme: "wss", Host: "relay.example", Port: 443}
	failure := engine.Failure{Code: engine.FailureRelayUnavailable}
	event := engine.RelayRecovery{
		Origin:  origin,
		Attempt: 2,
		State:   engine.RelayRecoveryFailed,
		Failure: &failure,
	}
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, false)
	harness.renderer.RenderRelayRecovery(event)
	if output := harness.buffer.String(); !strings.Contains(output, "Relay recovery attempt 2 failed") {
		t.Fatalf("redirected recovery warning = %q", output)
	}
}

func TestPeerAttemptIdentitySurvivesEventSequenceAndInterleaving(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, true)
	for _, id := range []string{"first", "second", "first", "first"} {
		record := diagnostics.New("peer_attempt", diagnostics.Fields{"stage": diagnostics.Text("started")}).
			Correlate(diagnostics.Fields{"peer_attempt_id": diagnostics.Text(id)})
		harness.renderer.RenderDiagnostic(record)
	}
	output := harness.buffer.String()
	if strings.Count(output, "Connection [first]:") != 3 || strings.Count(output, "Connection [second]:") != 1 {
		t.Fatalf("lost attempt ownership: %s", output)
	}
}

func TestPeerAdmissionReportsRejectionAndDeliveryWithoutClaimingConnection(t *testing.T) {
	tests := []struct {
		disposition, delivery, stage, want string
		warning                            bool
	}{
		{"rejected", "delivered", "admission-response-settled", "admission rejected (admission limited). Retry after 1000 ms.", true},
		{"rejected", "failed", "admission-response-settled", "Admission response delivery failed.", true},
		{"accepted", "failed", "admission-response-settled", "Admission response delivery failed.", true},
		{"accepted", "delivered", "admission-response-settled", "admission accepted.", false},
		{"accepted", "delivered", "admitted", "connected.", false},
	}
	for _, test := range tests {
		record := diagnostics.New("peer_attempt", diagnostics.Fields{"stage": diagnostics.Text(test.stage),
			"admission": diagnostics.Object(diagnostics.Fields{"disposition": diagnostics.Text(test.disposition), "response_delivery": diagnostics.Text(test.delivery)}),
			"rejection": diagnostics.Object(diagnostics.Fields{"code": diagnostics.Text("admission-limited"), "retry_after_ms": diagnostics.Uint(1000)})})
		line := formatPeerAttempt(record, SelectSymbols(false))
		text := lineText(line)
		if !strings.Contains(text, test.want) || test.stage != "admitted" && strings.Contains(text, "connected.") {
			t.Errorf("misleading admission: %s", text)
		}
		if test.warning && line.Spans()[0].Style != terminalcanvas.StyleWarning {
			t.Errorf("missing warning: %s", text)
		}
	}
}

func TestPeerAttemptNamesOnlyAnAdmittedRoute(t *testing.T) {
	for _, test := range []struct{ route, want string }{{"", "Connection ["}, {"direct", "Direct connection ["}, {"turn", "TURN relay connection ["}} {
		record := diagnostics.New("peer_attempt", diagnostics.Fields{"stage": diagnostics.Text("admitted"), "route": diagnostics.Text(test.route)})
		line := lineText(formatPeerAttempt(record, SelectSymbols(false)))
		if !strings.Contains(line, test.want) {
			t.Errorf("route=%s: %s", test.route, line)
		}
	}
}

func TestUnknownDiagnosticsRemainQuietAndProviderTextCannotControlTerminal(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, true)
	harness.renderer.RenderDiagnostic(diagnostics.New("new.event", nil))
	if harness.buffer.Len() != 0 {
		t.Fatal("unknown diagnostics reached ordinary display")
	}
	record := diagnostics.New("peer_attempt", diagnostics.Fields{"failure": diagnostics.Object(diagnostics.Fields{"message": diagnostics.Text("bad\x1b[2J\r\nline")})})
	harness.renderer.RenderDiagnostic(record)
	if strings.Contains(harness.buffer.String(), "\x1b[2J") || strings.Contains(harness.buffer.String(), "\r") {
		t.Fatal("provider text controlled terminal")
	}
}
