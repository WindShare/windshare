package networkcommand

import (
	"testing"

	"github.com/windshare/windshare/internal/platformsetup"
)

type startupFunc func() platformsetup.StartupReport

func (read startupFunc) Startup() platformsetup.StartupReport { return read() }

func TestStartupPublishesOneDecisionWithoutSystemPolicy(t *testing.T) {
	for _, decision := range []platformsetup.StartupDecision{platformsetup.SetupUnchosen, platformsetup.SetupSkipped,
		platformsetup.SetupRecorded, platformsetup.SetupPathChanged, platformsetup.SetupReadFailed, platformsetup.SetupUnsupported} {
		var reports []platformsetup.StartupReport
		ObserveStartup(startupFunc(func() platformsetup.StartupReport { return platformsetup.StartupReport{Decision: decision} }), func(report platformsetup.StartupReport) {
			reports = append(reports, report)
		})
		if len(reports) != 1 {
			t.Fatal("missing or duplicate decision", len(reports))
		}
		if reports[0].Decision != decision || reports[0].Decision.SuggestSetup() != decision.SuggestSetup() {
			t.Fatal(reports[0])
		}
	}
	ObserveStartup(nil, nil)
}
