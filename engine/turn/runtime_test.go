package turn

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/windshare/windshare/connectivity/turnpath"
)

func manualFixture() ManualSettings {
	return ManualSettings{
		Accounts: []AccountSettings{{ID: "owner", Provider: "standard", Issuer: StaticIssuer,
			LocalBudgetBytes: 100, Credentials: json.RawMessage(`{"username":"old","password":"password"}`)}},
		Profiles: []ProfileSettings{{ID: "primary", Account: "owner", URLs: []string{"turn:localhost:3478"}}},
	}
}

func TestConfigureValidatesWholeSettingsBeforePublishingOneRevision(t *testing.T) {
	runtime, manager, _, settings := discoveryFixture(t, func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	settings.Manual = manualFixture()
	if err := runtime.Configure(settings); err != nil {
		t.Fatal(err)
	}
	selection, release := runtime.ForShare(t.Context(), settings.Managed.Relays)
	defer release()
	old, err := selection.Acquire(t.Context(), "primary", turnpath.Subject{})
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	old.Charge(turnpath.Usage{TransportSent: 40})
	before := selection.Inventory()
	for _, invalid := range []func(*Settings){
		func(next *Settings) {
			next.Manual.Accounts[0].LocalBudgetBytes = 1
			next.Managed.Policies[0].RelayURL = "https://unknown.example"
		},
		func(next *Settings) {
			next.Managed.Policies[0].LocalBudgetBytes = 1
			next.Manual.Profiles[0].Account = "missing"
		},
	} {
		next := settings.Clone()
		invalid(&next)
		if err := runtime.Configure(next); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("invalid settings accepted: %v", err)
		}
		status := runtime.Status()
		if len(status) != 2 || status[0].LocalBudgetBytes != 100 || status[0].ChargedBytes != 40 ||
			status[1].LocalBudgetBytes != 1234 || old.Loss() != nil {
			t.Fatalf("rejected settings partially changed live authority: %+v", status)
		}
		if selection.Inventory().Revision != before.Revision {
			t.Fatal("rejected settings invalidated pending issuance")
		}
	}
	next := settings.Clone()
	next.Manual.Accounts[0].LocalBudgetBytes = 200
	next.Manual.Accounts[0].Credentials = json.RawMessage(`{"username":"new","password":"password"}`)
	next.Managed.Policies[0].LocalBudgetBytes = 5678
	if err := runtime.Configure(next); err != nil {
		t.Fatal(err)
	}
	status := runtime.Status()
	if selection.Inventory().Revision != before.Revision+1 || status[0].LocalBudgetBytes != 200 ||
		status[0].ChargedBytes != 40 || status[1].LocalBudgetBytes != 5678 || old.Loss() != nil {
		t.Fatalf("settings were not published together with retained usage: %+v", status)
	}
	if old.Relay().Credentials().Username() != "old" {
		t.Fatal("settings replacement mutated an existing grant")
	}
	fresh, err := selection.Acquire(t.Context(), "primary", turnpath.Subject{})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if fresh.Relay().Credentials().Username() != "new" {
		t.Fatal("new acquisition did not use the published settings")
	}
	revision := manager.Inventory().Revision
	if err := runtime.Configure(next.Clone()); err != nil {
		t.Fatal(err)
	}
	if manager.Inventory().Revision != revision {
		t.Fatal("reapplying identical settings interrupted active authority")
	}
}

func TestConfigureRecognizesAccessKeyOnlyChangesAndOwnsManagedInputs(t *testing.T) {
	issuedWith := make(chan string, 1)
	runtime, _, cache, settings := discoveryFixture(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/api/turn/capability" {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		if request.URL.Path == "/api/turn/revoke" {
			return reply(http.StatusOK, `{"state":"canceled"}`), nil
		}
		issuedWith <- request.Header.Get("Authorization")
		return reply(http.StatusForbidden, "{}"), nil
	})
	settings.Managed.Relays[0].AccessKey = "replacement-device"
	if err := cache.Save(t.Context(), settings.Managed.Relays[0], Capability{ServiceID: "cf-service", Enabled: true}, runtime.dependencies.Now()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Configure(settings); err != nil {
		t.Fatal(err)
	}
	selection, release := runtime.ForShare(t.Context(), settings.Managed.Relays)
	defer release()
	settings.Managed.Relays[0].AccessKey = "caller-mutation"
	settings.Managed.Policies[0].LocalBudgetBytes = 1
	_, err := selection.Acquire(t.Context(), "managed.cf-service", turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
	if err == nil {
		t.Fatal("provider failure ignored")
	}
	if header := <-issuedWith; header != "Bearer replacement-device" {
		t.Fatalf("access-key rotation was ignored or caller mutation leaked: %s", header)
	}
	if runtime.Status()[0].LocalBudgetBytes != 1234 {
		t.Fatal("caller mutation changed the configured budget")
	}
}
