package clievent

import (
	"testing"

	"github.com/windshare/windshare/internal/platformsetup"
)

func TestNetworkSetupRecommendationFollowsInstallationDecision(t *testing.T) {
	for _, decision := range []platformsetup.StartupDecision{platformsetup.SetupUnchosen, platformsetup.SetupSkipped, platformsetup.SetupRecorded, platformsetup.SetupPathChanged} {
		event, err := NewNetworkSetupObserved(CommandShare, platformsetup.StartupReport{Decision: decision})
		if err != nil || event.SuggestSetup() != decision.SuggestSetup() {
			t.Fatal(event, err)
		}
	}
	if _, err := NewNetworkSetupObserved(0, platformsetup.StartupReport{Decision: platformsetup.SetupSkipped}); err == nil {
		t.Fatal("missing command accepted")
	}
	if (NetworkSetupObserved{}).Validate() == nil {
		t.Fatal("missing decision accepted")
	}
}
