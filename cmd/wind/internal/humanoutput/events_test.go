package humanoutput

import (
	"strings"
	"testing"
	"time"

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

func TestRelayRecoverySeparatesNonTerminalReasonsFromTerminalAdvice(t *testing.T) {
	t.Parallel()
	origin := engine.RelayOrigin{Scheme: "wss", Host: "relay.example", Port: 443}
	for _, tc := range []struct {
		name      string
		failure   engine.Failure
		want      string
		forbidden []string
	}{
		{
			name:      "not found omits link retry advice during auto recovery",
			failure:   engine.Failure{Code: engine.FailureRelayNotFound},
			want:      "The share is not currently available.",
			forbidden: []string{"Retry the original link", "Retry after"},
		},
		{
			name:      "share not ready omits polling retry after during auto recovery",
			failure:   engine.Failure{Code: engine.FailureShareNotReady, RetryAfter: 250 * time.Millisecond},
			want:      "The share is not ready yet.",
			forbidden: []string{"Retry after"},
		},
		{
			name:      "relay unavailable omits server cooldown during auto recovery",
			failure:   engine.Failure{Code: engine.FailureRelayUnavailable, RetryAfter: 30 * time.Second},
			want:      "The relay is unavailable.",
			forbidden: []string{"Retry after"},
		},
		{
			name:      "network unavailable omits manual retry advice during auto recovery",
			failure:   engine.Failure{Code: engine.FailureNetworkUnavailable},
			want:      "The WindShare network is unavailable.",
			forbidden: []string{"Check your connection and retry"},
		},
	} {
		failure := tc.failure
		line := lineText(formatRelayRecovery(engine.RelayRecovery{
			Origin:  origin,
			Attempt: 2,
			State:   engine.RelayRecoveryFailed,
			Failure: &failure,
		}, SelectSymbols(false)))
		if !strings.Contains(line, tc.want) {
			t.Errorf("%s: recovery line = %q, want substring %q", tc.name, line, tc.want)
		}
		for _, bad := range tc.forbidden {
			if strings.Contains(line, bad) {
				t.Errorf("%s: non-terminal recovery line leaked %q: %q", tc.name, bad, line)
			}
		}
	}

	denied := engine.Failure{Code: engine.FailureRelayAccessDenied}
	terminal := lineText(formatRelayRecovery(engine.RelayRecovery{
		Origin:   origin,
		Attempt:  1,
		State:    engine.RelayRecoveryFailed,
		Terminal: true,
		Failure:  &denied,
	}, SelectSymbols(false)))
	if !strings.Contains(terminal, "Check your account or relay authorization.") {
		t.Fatalf("terminal recovery line omitted actionable advice: %q", terminal)
	}

	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, false)
	harness.renderer.RenderWarning(engine.Failure{Code: engine.FailureRelayNotFound, RetryAfter: 5 * time.Second})
	warning := harness.buffer.String()
	if !strings.Contains(warning, "Warning: The share is not currently available.") ||
		strings.Contains(warning, "Retry the original link") || strings.Contains(warning, "Retry after") {
		t.Fatalf("non-terminal warning leaked retry advice: %q", warning)
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
