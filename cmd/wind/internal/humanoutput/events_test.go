package humanoutput

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
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
	first := peerAttemptSpecForRender(t)
	identity := base64.RawURLEncoding.EncodeToString(first.Attempt.Bytes())
	other := peerAttemptSpecForRender(t)
	other.Attempt, _ = appobs.NewPeerAttemptID(mustID(t, 6))
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, true)
	for index, sequence := range []uint64{1, 1, 9, 10} {
		spec := first
		if index == 1 {
			spec = other
		}
		spec.Sequence = sequence
		if sequence == 9 {
			spec.Stage = appobs.PeerAnswerSent
		}
		if sequence == 10 {
			spec.Stage = appobs.PeerAttemptFailed
			spec.FailedAtStage = appobs.PeerAttemptAdmitted
			spec.FailureScope = appobs.PeerFailureAttempt
			spec.Failure = mustFailure(t, appobs.FailurePeerCanceled)
		}
		event, err := appobs.NewPeerAttemptObserved(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := harness.renderer.Render(event); err != nil {
			t.Fatal(err)
		}
	}
	output := harness.buffer.String()
	if strings.Count(output, "Connection ["+identity+"]:") != 3 ||
		!strings.Contains(output, "Connection ["+base64.RawURLEncoding.EncodeToString(other.Attempt.Bytes())+"]:") ||
		strings.Contains(output, "connection attempt 9") || strings.Contains(output, "connection attempt 10") {
		t.Fatalf("attempt identities changed with event sequence: %s", output)
	}
}

func TestPeerAdmissionReportsRejectionAndDeliveryWithoutClaimingConnection(t *testing.T) {
	for _, test := range []struct {
		name        string
		disposition appobs.PeerAdmissionDisposition
		delivery    appobs.PeerResponseDelivery
		stage       appobs.PeerAttemptStage
		want        string
		warning     bool
	}{
		{"rejected", appobs.PeerAdmissionRejected, appobs.PeerResponseDelivered, appobs.PeerAdmissionResponseSettled, "admission rejected (admission limited). Retry after 1000 ms.", true},
		{"rejection not delivered", appobs.PeerAdmissionRejected, appobs.PeerResponseDeliveryFailed, appobs.PeerAdmissionResponseSettled, "Admission response delivery failed.", true},
		{"acceptance not delivered", appobs.PeerAdmissionAccepted, appobs.PeerResponseDeliveryFailed, appobs.PeerAdmissionResponseSettled, "Admission response delivery failed.", true},
		{"accepted before installation", appobs.PeerAdmissionAccepted, appobs.PeerResponseDelivered, appobs.PeerAdmissionResponseSettled, "admission accepted.", false},
		{"connected", appobs.PeerAdmissionAccepted, appobs.PeerResponseDelivered, appobs.PeerAttemptAdmitted, "connected.", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := peerAttemptSpecForRender(t)
			spec.Sequence, spec.Stage, spec.Phase = 9, test.stage, appobs.PeerPhaseAdmission
			spec.GrantOperation, _ = appobs.NewProtocolOperationID(mustID(t, 7))
			spec.HasGrantOperation = true
			spec.Lane, _ = appobs.NewLaneIdentity(1, 2)
			spec.HasLane = true
			spec.AdmissionDisposition, spec.ResponseDelivery = test.disposition, test.delivery
			if spec.Stage == appobs.PeerAttemptAdmitted {
				spec.Route = appobs.LaneRouteDirect
			}
			if test.disposition == appobs.PeerAdmissionRejected {
				spec.RejectionCode = appobs.PeerLaneRejectAdmissionLimited
				spec.RejectionRetryAfterMillis = 1000
			}
			event, err := appobs.NewPeerAttemptObserved(spec)
			if err != nil {
				t.Fatal(err)
			}
			line := formatPeerAttempt(event, SelectSymbols(false))
			text := lineText(line)
			if !strings.Contains(text, test.want) || strings.Contains(text, "admission-response-settled") {
				t.Fatalf("admission result = %q", text)
			}
			if test.stage != appobs.PeerAttemptAdmitted && strings.Contains(text, "connected.") {
				t.Fatalf("uninstalled admission claimed a connection: %q", text)
			}
			if test.warning && (!strings.HasPrefix(text, "! ") || line.Spans()[0].Style != terminalcanvas.StyleWarning) {
				t.Fatalf("admission failure lost warning presentation: %#v", line.Spans())
			}
		})
	}
}

func peerAttemptSpecForRender(t *testing.T) appobs.PeerAttemptSpec {
	t.Helper()
	session, _ := appobs.NewProtocolSessionID(mustID(t, 3))
	path, _ := appobs.NewPeerPathID(mustID(t, 4))
	attempt, _ := appobs.NewPeerAttemptID(mustID(t, 5))
	return appobs.PeerAttemptSpec{
		Workflow: appobs.WorkflowShare, Session: session, PeerPath: path, Attempt: attempt,
		Sequence: 1, Stage: appobs.PeerAttemptStarted,
	}
}

func TestPeerAttemptNamesOnlyAnAdmittedRoute(t *testing.T) {
	for _, test := range []struct {
		route appobs.LaneRoute
		want  string
	}{{0, "Connection ["}, {appobs.LaneRouteDirect, "Direct connection ["}, {appobs.LaneRouteTURN, "TURN relay connection ["}} {
		spec := peerAttemptSpecForRender(t)
		if test.route != 0 {
			spec.Stage, spec.Phase, spec.Route = appobs.PeerAttemptAdmitted, appobs.PeerPhaseAdmission, test.route
			spec.GrantOperation, _ = appobs.NewProtocolOperationID(mustID(t, 7))
			spec.HasGrantOperation = true
			spec.Lane, _ = appobs.NewLaneIdentity(1, 2)
			spec.HasLane = true
			spec.AdmissionDisposition, spec.ResponseDelivery = appobs.PeerAdmissionAccepted, appobs.PeerResponseDelivered
		}
		event, err := appobs.NewPeerAttemptObserved(spec)
		if err != nil {
			t.Fatal(err)
		}
		line := lineText(formatPeerAttempt(event, SelectSymbols(false)))
		if !strings.Contains(line, test.want) || test.route != appobs.LaneRouteDirect && strings.Contains(line, "Direct") {
			t.Fatalf("route %d misrepresented: %s", test.route, line)
		}
	}
}
