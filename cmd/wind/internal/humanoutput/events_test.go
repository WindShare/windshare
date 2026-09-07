package humanoutput

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
)

func TestSharingSubjectUsesOnlySelectedRootFacts(t *testing.T) {
	t.Parallel()
	file, err := clievent.NewFileSubject(clievent.NewDisplayName("photo.jpg"), 8_200_000)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := clievent.NewDirectorySubject(clievent.NewDisplayName("photos"))
	if err != nil {
		t.Fatal(err)
	}
	multiple, err := clievent.NewMultipleSubject(3)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		subject clievent.SharingSubject
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
		clievent.CommandGet, clievent.TransportWebRTC, clievent.TransportRelay,
		mustFailure(t, clievent.FailurePeerNegotiation),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := lineText(formatFallback(event, SelectSymbols(false))); !strings.Contains(got, "Direct path unavailable; using Relay") {
		t.Fatalf("fallback line = %q", got)
	}
}

func TestFailedRelayRecoveryRemainsAWarningWhenRedirected(t *testing.T) {
	authority, err := clievent.NewRelayAuthority(clievent.RelayWSS, "relay.example", 443)
	if err != nil {
		t.Fatal(err)
	}
	event, err := clievent.NewRelayRecovering(
		clievent.CommandGet, authority, 2, clievent.RelayRecoveryFailed,
		mustFailure(t, clievent.FailureRelayTransport),
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
	other.Attempt, _ = clievent.NewPeerAttemptID(mustID(t, 6))
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, true)
	for index, sequence := range []uint64{1, 1, 9, 10} {
		spec := first
		if index == 1 {
			spec = other
		}
		spec.Sequence = sequence
		if sequence == 9 {
			spec.Stage = clievent.PeerAnswerSent
		}
		if sequence == 10 {
			spec.Stage = clievent.PeerAttemptFailed
			spec.FailedAtStage = clievent.PeerAttemptAdmitted
			spec.FailureScope = clievent.PeerFailureAttempt
			spec.Failure = mustFailure(t, clievent.FailurePeerCanceled)
		}
		event, err := clievent.NewPeerAttemptObserved(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := harness.renderer.Render(event); err != nil {
			t.Fatal(err)
		}
	}
	output := harness.buffer.String()
	if strings.Count(output, "Direct connection ["+identity+"]:") != 3 ||
		!strings.Contains(output, "Direct connection ["+base64.RawURLEncoding.EncodeToString(other.Attempt.Bytes())+"]:") ||
		strings.Contains(output, "connection attempt 9") || strings.Contains(output, "connection attempt 10") {
		t.Fatalf("attempt identities changed with event sequence: %s", output)
	}
}

func TestPeerAdmissionReportsRejectionAndDeliveryWithoutClaimingConnection(t *testing.T) {
	for _, test := range []struct {
		name        string
		disposition clievent.PeerAdmissionDisposition
		delivery    clievent.PeerResponseDelivery
		stage       clievent.PeerAttemptStage
		want        string
		warning     bool
	}{
		{"rejected", clievent.PeerAdmissionRejected, clievent.PeerResponseDelivered, clievent.PeerAdmissionResponseSettled, "admission rejected (admission limited). Retry after 1000 ms.", true},
		{"rejection not delivered", clievent.PeerAdmissionRejected, clievent.PeerResponseDeliveryFailed, clievent.PeerAdmissionResponseSettled, "Admission response delivery failed.", true},
		{"acceptance not delivered", clievent.PeerAdmissionAccepted, clievent.PeerResponseDeliveryFailed, clievent.PeerAdmissionResponseSettled, "Admission response delivery failed.", true},
		{"accepted before installation", clievent.PeerAdmissionAccepted, clievent.PeerResponseDelivered, clievent.PeerAdmissionResponseSettled, "admission accepted.", false},
		{"connected", clievent.PeerAdmissionAccepted, clievent.PeerResponseDelivered, clievent.PeerAttemptAdmitted, "connected.", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := peerAttemptSpecForRender(t)
			spec.Sequence, spec.Stage, spec.Phase = 9, test.stage, clievent.PeerPhaseAdmission
			spec.GrantOperation, _ = clievent.NewProtocolOperationID(mustID(t, 7))
			spec.HasGrantOperation = true
			spec.Lane, _ = clievent.NewLaneIdentity(1, 2)
			spec.HasLane = true
			spec.AdmissionDisposition, spec.ResponseDelivery = test.disposition, test.delivery
			if test.disposition == clievent.PeerAdmissionRejected {
				spec.RejectionCode = clievent.PeerLaneRejectAdmissionLimited
				spec.RejectionRetryAfterMillis = 1000
			}
			event, err := clievent.NewPeerAttemptObserved(spec)
			if err != nil {
				t.Fatal(err)
			}
			line := formatPeerAttempt(event, SelectSymbols(false))
			text := lineText(line)
			if !strings.Contains(text, test.want) || strings.Contains(text, "admission-response-settled") {
				t.Fatalf("admission result = %q", text)
			}
			if test.stage != clievent.PeerAttemptAdmitted && strings.Contains(text, "connected.") {
				t.Fatalf("uninstalled admission claimed a connection: %q", text)
			}
			if test.warning && (!strings.HasPrefix(text, "! ") || line.Spans()[0].Style != terminalcanvas.StyleWarning) {
				t.Fatalf("admission failure lost warning presentation: %#v", line.Spans())
			}
		})
	}
}

func peerAttemptSpecForRender(t *testing.T) clievent.PeerAttemptSpec {
	t.Helper()
	session, _ := clievent.NewProtocolSessionID(mustID(t, 3))
	path, _ := clievent.NewPeerPathID(mustID(t, 4))
	attempt, _ := clievent.NewPeerAttemptID(mustID(t, 5))
	return clievent.PeerAttemptSpec{
		Command: clievent.CommandShare, Session: session, PeerPath: path, Attempt: attempt,
		Sequence: 1, Stage: clievent.PeerAttemptStarted,
	}
}
