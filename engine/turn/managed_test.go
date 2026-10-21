package turn

import (
	"context"
	"fmt"
	"testing"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/turn/internal/provider"
)

type managedSource struct{}

func (managedSource) Issue(context.Context, provider.Subject) (provider.Acquisition, error) {
	credential, _ := iceconfig.Fixed("user", "password")
	return provider.Acquisition{Credentials: credential, URLs: []string{"turn:localhost:3478"}}, nil
}
func TestManagedInventoryDoesNotConsumeManualQuotaAndFollowsSelection(t *testing.T) {
	var config accountConfig
	for i := range MaximumAccounts {
		id := fmt.Sprintf("manual%d", i)
		config.Accounts = append(config.Accounts, account{ID: id, Provider: "coturn", Source: managedSource{}})
		config.Profiles = append(config.Profiles, profile{ID: id, AccountID: id, URLs: []string{"turn:localhost:3478"}})
	}
	manager, err := newAccountManager(config)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	services := []managedService{{ID: "cf-selected", RelayURL: "https://selected.example", LocalBudgetBytes: 100, Source: managedSource{}},
		{ID: "cf-inactive", RelayURL: "https://inactive.example", Source: managedSource{}}}
	if err := manager.SetManagedServices(services); err != nil {
		t.Fatal(err)
	}
	selected := manager.Select([]string{"https://selected.example"})
	inventory := selected.Inventory()
	if len(inventory.Profiles) != MaximumProfiles || inventory.Profiles[0].Identity.Profile != "managed.cf-selected" {
		t.Fatal("managed service excluded by manual quota", inventory)
	}
	for _, profile := range inventory.Profiles {
		if profile.Identity.Profile == "managed.cf-inactive" {
			t.Fatal("inactive deployment admitted")
		}
	}
	if len(manager.Select(nil).Inventory().Profiles) != MaximumProfiles {
		t.Fatal("manual accounts depended on relay selection")
	}
	lease, err := selected.Acquire(t.Context(), "managed.cf-selected", turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	lease.Charge(turnpath.Usage{Payload: 40})
	if err := manager.Reconfigure(config); err != nil {
		t.Fatal(err)
	}
	if manager.Snapshot()[MaximumAccounts].ChargedBytes != 40 {
		t.Fatal("manual update reset managed ledger")
	}
	if err := manager.SetManagedServices(services[1:]); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lease.Lost():
	default:
		t.Fatal("removed service kept local authority")
	}
	if err := manager.SetManagedServices(services); err != nil {
		t.Fatal(err)
	}
	if manager.Snapshot()[MaximumAccounts].ChargedBytes != 40 {
		t.Fatal("re-import reset managed budget")
	}
}
