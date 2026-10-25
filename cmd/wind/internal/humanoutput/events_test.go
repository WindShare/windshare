package humanoutput

import (
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/core/diagnostics"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestSharingSubjectUsesOnlySelectedRootFacts(t *testing.T) {
	t.Parallel()
	file, err := appobs.NewFileSubject(appobs.NewDisplayName("photo.jpg"), 8_200_000)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := appobs.NewDirectorySubject(appobs.NewDisplayName("photos"))
	if err != nil {
		t.Fatal(err)
	}
	multiple, err := appobs.NewMultipleSubject(3)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		subject appobs.SharingSubject
		want    string
	}{
		{"file", file, "Sharing: photo.jpg (file, 8.2 MB)"},
		{"directory", directory, "Sharing: photos/ (directory)"},
		{"multiple", multiple, "Sharing: 3 selected items"},
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
	event, err := clievent.NewFallback(
		clievent.CommandGet, appobs.TransportWebRTC, appobs.TransportRelay,
		mustFailure(t, appobs.FailurePeerNegotiation),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := lineText(formatFallback(event, SelectSymbols(false))); !strings.Contains(got, "WebRTC path unavailable; using Relay") {
		t.Fatalf("fallback line = %q", got)
	}
}

func TestFailedRelayRecoveryRemainsAWarningWhenRedirected(t *testing.T) {
	authority, err := appobs.NewRelayAuthority(appobs.RelayWSS, "relay.example", 443)
	if err != nil {
		t.Fatal(err)
	}
	event, err := clievent.NewRelayRecovering(
		clievent.CommandGet, authority, 2, appobs.RelayRecoveryFailed,
		mustFailure(t, appobs.FailureRelayTransport),
	)
	if err != nil {
		t.Fatal(err)
	}
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, false)
	if err := harness.renderer.Render(event); err != nil {
		t.Fatal(err)
	}
	if output := harness.buffer.String(); !strings.Contains(output, "Relay recovery attempt 2 failed") {
		t.Fatalf("redirected recovery warning = %q", output)
	}
}

func TestPeerAttemptIdentitySurvivesEventSequenceAndInterleaving(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, true)
	for _, id := range []string{"first", "second", "first", "first"} {
		event := diagnosticEvent("peer_attempt", diagnostics.Fields{"stage": diagnostics.Text("started")})
		event.Record = event.Record.Correlate(diagnostics.Fields{"peer_attempt_id": diagnostics.Text(id)})
		if err := harness.renderer.Render(event); err != nil {
			t.Fatal(err)
		}
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
	if err := harness.renderer.Render(diagnosticEvent("new.event", nil)); err != nil {
		t.Fatal(err)
	}
	if harness.buffer.Len() != 0 {
		t.Fatal("unknown diagnostics reached ordinary display")
	}
	event := diagnosticEvent("peer_attempt", diagnostics.Fields{"failure": diagnostics.Object(diagnostics.Fields{"message": diagnostics.Text("bad\x1b[2J\r\nline")})})
	if err := harness.renderer.Render(event); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(harness.buffer.String(), "\x1b[2J") || strings.Contains(harness.buffer.String(), "\r") {
		t.Fatal("provider text controlled terminal")
	}
}
