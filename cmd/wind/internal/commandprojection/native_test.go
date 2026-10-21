package commandprojection

import (
	"strings"
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/turnpath"
)

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
			source := turnpath.Event{Provider: "test", Account: "owner", Profile: "primary", Milestone: turnpath.MilestoneTransportSelection}
			test.mutate(&source)
			_, err := ProjectNativeObservation(clievent.CommandShare, nativepeer.Observation{
				Subject: nativepeer.Subject{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2}, Side: nativepeer.SideSender},
				TURN:    &source,
			})
			if err == nil {
				t.Fatal("invalid observation accepted")
			}
			rejection := ProjectionRejection(err)
			if !rejection.Valid() || rejection.Field != test.field || rejection.Rule != test.rule ||
				rejection.Session == "" || rejection.Source != "commandprojection.ProjectNativeObservation" {
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
