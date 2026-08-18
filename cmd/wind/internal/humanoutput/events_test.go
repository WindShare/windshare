package humanoutput

import (
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
