package relayconfig

import (
	"testing"

	"github.com/windshare/windshare/engine/internal/turnruntime"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/turn"
)

func TestTURNProjectionDoesNotDependOnUnrelatedApplicationSettings(t *testing.T) {
	config := Config{
		Profiles: []relayprofile.Candidate{{PublicURL: "https://relay.example", AccessKey: "key"}},
		FrontURL: "not-a-frontend", ActiveRelays: []string{"https://not-saved.example"},
		ManagedTURNPolicies: []turn.ManagedPolicy{{RelayURL: "https://relay.example", LocalBudgetBytes: 100}},
	}
	if err := config.Validate(); err == nil {
		t.Fatal("invalid application settings accepted")
	}
	settings := config.TURNSettings()
	runtime, err := turnruntime.New(settings, turn.Dependencies{})
	if err != nil {
		t.Fatal("unrelated application settings blocked TURN configuration", err)
	}
	defer runtime.Close()
	settings.Managed.Relays[0].AccessKey = "changed"
	settings.Managed.Policies[0].LocalBudgetBytes = 1
	if config.Profiles[0].AccessKey != "key" || config.ManagedTURNPolicies[0].LocalBudgetBytes != 100 {
		t.Fatal("TURN projection shared mutable storage with application settings")
	}
}
