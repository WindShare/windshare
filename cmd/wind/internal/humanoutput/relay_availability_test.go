package humanoutput

import (
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
)

func TestDefaultRelayOutputDistinguishesJoinAvailabilityAndShowsRecovery(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, false)
	authority, _ := clievent.NewRelayAuthority(clievent.RelayWSS, "relay.example", 443)
	for _, state := range []struct {
		available, total, terminal uint32
		ever                       bool
	}{{0, 2, 0, false}, {1, 2, 0, true}, {0, 2, 0, true}, {0, 2, 2, true}} {
		event, _ := clievent.NewRelayAvailability(state.available, state.total, state.terminal, state.ever)
		if err := harness.renderer.Render(event); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range []clievent.RelayRecoveryState{clievent.RelayRecoveryWaiting, clievent.RelayRecoverySucceeded} {
		event, _ := clievent.NewRelayRecoveryObservation(clievent.CommandShare, authority, 70, state, clievent.Failure{}, clievent.RelayRecoveryDetails{Generation: 2, Slow: true, Resume: true, NextDelay: time.Second})
		if err := harness.renderer.Render(event); err != nil {
			t.Fatal(err)
		}
	}
	output := harness.buffer.String()
	for _, expected := range []string{"before publishing the link", "1 of 2 relays", "Healthy direct transfers can continue", "all relays need attention", "retrying automatically", "Relay connection restored: relay.example:443"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("missing %q: %s", expected, output)
		}
	}
	if strings.Contains(output, "attempt 70") {
		t.Fatalf("routine output leaked retry details: %s", output)
	}
}
