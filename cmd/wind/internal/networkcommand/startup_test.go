package networkcommand

import (
	"testing"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/internal/platformsetup"
)

type startupFunc func() platformsetup.StartupReport

func (read startupFunc) Startup() platformsetup.StartupReport { return read() }

type eventChannel chan clievent.Event

func (events eventChannel) Observe(event clievent.Event) bool { events <- event; return true }

func TestStartupPublishesOneDecisionWithoutSystemPolicy(t *testing.T) {
	for _, decision := range []platformsetup.StartupDecision{platformsetup.SetupUnchosen, platformsetup.SetupSkipped,
		platformsetup.SetupRecorded, platformsetup.SetupPathChanged, platformsetup.SetupReadFailed, platformsetup.SetupUnsupported} {
		events := make(eventChannel, 2)
		ObserveStartup(startupFunc(func() platformsetup.StartupReport { return platformsetup.StartupReport{Decision: decision} }), events, clievent.CommandShare)
		if len(events) != 1 {
			t.Fatal("missing or duplicate decision", len(events))
		}
		event := (<-events).(clievent.NetworkSetupObserved)
		if event.Decision() != decision.String() || event.SuggestSetup() != decision.SuggestSetup() {
			t.Fatal(event)
		}
	}
	ObserveStartup(nil, nil, clievent.CommandGet)
}
