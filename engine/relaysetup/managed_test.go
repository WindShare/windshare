package relaysetup

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turnaccount"
	"github.com/windshare/windshare/engine/turnconfig"
)

func TestOnboardingManagedServicePreservesFullManualConfiguration(t *testing.T) {
	saved := relayconfig.Config{Profiles: []senderrelay.Candidate{{PublicURL: "https://relay.example", AccessKey: "device-key"}},
		ActiveRelays: []string{"https://relay.example"}}
	for i := range turnaccount.MaximumAccounts {
		id := fmt.Sprintf("manual%d", i)
		saved.TURN.Accounts = append(saved.TURN.Accounts, turnconfig.Account{ID: id, Provider: "coturn", Issuer: "static",
			Credentials: json.RawMessage("{\"username\":\"user\",\"password\":\"password\"}")})
		saved.TURN.Profiles = append(saved.TURN.Profiles, turnconfig.Profile{ID: id, Account: id, URLs: []string{"turn:localhost:3478"}})
	}
	revision := saved.SettingsRevision()
	reference := turnconfig.ManagedService{ServiceID: "cf-service", RelayURL: "https://relay.example", Enabled: true}
	installManagedService(&saved, reference.RelayURL, []turnconfig.ManagedService{reference})
	if len(saved.ManagedTURN) != 1 || len(saved.TURN.Accounts) != turnaccount.MaximumAccounts {
		t.Fatal("discovery consumed manual capacity")
	}
	if err := saved.Validate(); err != nil {
		t.Fatal(err)
	}
	if saved.SettingsRevision() != revision {
		t.Fatal("inventory invalidated concurrent enrollment")
	}
	saved.ManagedTURN[0].LocalBudgetBytes = 99
	reference.Enabled = false
	installManagedService(&saved, reference.RelayURL, []turnconfig.ManagedService{reference})
	if saved.ManagedTURN[0].Enabled || saved.ManagedTURN[0].LocalBudgetBytes != 99 {
		t.Fatal("refresh lost local preferences")
	}
}
