package eventproject

import (
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/throughput"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/core/connectionpath"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestParallelismObservationPreservesDecisionAndOwnsFacts(t *testing.T) {
	source := nativepeer.ParallelismFacts{
		At:       time.Unix(42, 0),
		Decision: throughput.Decision{Action: throughput.Explore, Reason: "network_demand", UsefulRate: 12, OutputRate: 11, BaselineUsefulRate: 10, BaselineOutputRate: 9},
		Demand:   throughput.Observation{Paths: 1, QueuedBytes: 32, UnfinishedBytes: 8, CapacityWaitingBytes: 4},
	}
	event, err := ProjectNativeObservation(appobs.WorkflowReceive, nativepeer.Observation{
		Subject:     nativepeer.Subject{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2}, Side: nativepeer.SideReceiver},
		Parallelism: &source,
	})
	if err != nil {
		t.Fatal(err)
	}
	source.Decision.Reason = "mutated"
	facts := event.Facts()
	if facts.Kind != "parallelism" || facts.Parallelism.Reason != "network_demand" || facts.Parallelism.QueuedBytes != 32 || facts.Parallelism.OutputRate != 11 || !facts.Session.Valid() || !facts.Path.Valid() {
		t.Fatalf("decision context lost: %+v", facts)
	}
	facts.Parallelism.QueuedBytes = 999
	if event.Facts().Parallelism.QueuedBytes != 32 {
		t.Fatal("mutable decision facts escaped")
	}
}

func TestTURNRejectionsIdentifyFailedOperands(t *testing.T) {
	for _, test := range []struct {
		name               string
		mutate             func(*turnpath.Event)
		field, rule, value string
	}{
		{"milestone", func(event *turnpath.Event) { event.Milestone = 255 }, "turn.milestone", "known_enum", "255"},
		{"missing service", func(event *turnpath.Event) { event.Account = "" }, "turn.account", "bounded_service_identity", ""},
		{"service syntax", func(event *turnpath.Event) { event.Profile = "invalid/profile" }, "turn.profile", "ascii_service_identity", "invalid/profile"},
		{"oversized reason", func(event *turnpath.Event) { event.Reason = strings.Repeat("x", 4096) }, "turn.reason", "bounded_text", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := turnpath.Event{Provider: "test", Account: "owner", Profile: "primary", Milestone: connectionpath.MilestoneTransportSelection}
			test.mutate(&source)
			_, err := ProjectNativeObservation(appobs.WorkflowShare, nativepeer.Observation{
				Subject: nativepeer.Subject{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2}, Side: nativepeer.SideSender},
				TURN:    &source,
			})
			if err == nil {
				t.Fatal("invalid observation accepted")
			}
			rejection := ProjectionRejection(err)
			if !rejection.Valid() || rejection.Field != test.field || rejection.Rule != test.rule ||
				rejection.Session == "" || rejection.Source != "eventproject.ProjectNativeObservation" {
				t.Fatalf("missing rejection provenance: %+v", rejection)
			}
			found := false
			for _, field := range rejection.Evidence() {
				if field.Field != test.field {
					continue
				}
				found = true
				if test.name == "oversized reason" {
					if !rejection.Truncated() || rejection.OmittedBytes() == 0 || len(field.Value) >= len(source.Reason) {
						t.Fatal("oversized evidence was neither bounded nor marked")
					}
				} else if field.Value != test.value {
					t.Fatalf("source operand changed: %+v", field)
				}
			}
			if !found {
				t.Fatalf("missing failed operand %s", test.field)
			}
		})
	}
}
