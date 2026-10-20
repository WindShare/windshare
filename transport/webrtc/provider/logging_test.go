package provider

import (
	"io"
	"testing"

	"github.com/pion/logging"
)

func TestProviderWarningsRetainAttemptCorrelationWithoutTracingRoutinePackets(t *testing.T) {
	var events []Event
	connection := &Connection{request: AttemptConfig{
		ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2}, AttemptID: [16]byte{3},
		NetworkGenerationID: 4, ICEProfileID: "test-profile",
		Observe: func(event Event) { events = append(events, event) },
	}}
	base := logging.NewDefaultLoggerFactory()
	base.Writer = io.Discard
	factory := providerLoggerFactory{LoggerFactory: base, observe: connection.observe}
	turn := factory.NewLogger("turnc")
	turn.Debug("routine packet")
	turn.Warn("binding rejected")
	turn.Warnf("binding %d rejected", 7)
	ice := factory.NewLogger("ice")
	ice.Error("allocation failed")
	ice.Errorf("server %s unavailable", "test")
	factory.NewLogger("dtls").Warn("not an ICE diagnostic")
	if len(events) != 4 {
		t.Fatalf("unexpected diagnostics: %+v", events)
	}
	for i, event := range events {
		if event.ProtocolSessionID != [16]byte{1} || event.PeerPathID != [16]byte{2} ||
			event.AttemptID != [16]byte{3} || event.NetworkGenerationID != 4 ||
			event.ICEProfileID != "test-profile" || event.At.IsZero() {
			t.Fatalf("uncorrelated provider failure: %+v", event)
		}
		milestone := "turnc_warning"
		if i >= 2 {
			milestone = "ice_error"
		}
		if event.Milestone != milestone || event.State == "" {
			t.Fatalf("failure scope lost: %+v", event)
		}
	}
}
