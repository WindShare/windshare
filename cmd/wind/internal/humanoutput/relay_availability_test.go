package humanoutput

import (
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/terminalcanvas"
	"github.com/windshare/windshare/engine"
)

func TestDefaultRelayOutputDistinguishesJoinAvailabilityAndShowsRecovery(t *testing.T) {
	harness := newRenderHarness(t, terminalcanvas.Capabilities{}, false)
	origin := engine.RelayOrigin{Scheme: "wss", Host: "relay.example", Port: 443}
	for _, state := range []engine.RelayAvailability{
		{Available: 0, Total: 2, Terminal: 0, EverReady: false},
		{Available: 1, Total: 2, Terminal: 0, EverReady: true},
		{Available: 0, Total: 2, Terminal: 0, EverReady: true},
		{Available: 0, Total: 2, Terminal: 2, EverReady: true},
	} {
		harness.renderer.RenderRelayAvailability(state)
	}
	for _, state := range []engine.RelayRecoveryState{engine.RelayRecoveryWaiting, engine.RelayRecoverySucceeded} {
		harness.renderer.RenderRelayRecovery(engine.RelayRecovery{
			Origin: origin, Attempt: 70, State: state,
			Generation: 2, Slow: true, Resume: true, NextDelay: time.Second,
		})
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
