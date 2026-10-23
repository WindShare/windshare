package turnruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/turnpath"
)

type pendingResult struct {
	lease *turnpath.Lease
	err   error
}

func pendingAcquisition(t *testing.T, manager *accountManager, profileID string, started <-chan context.Context, proceed chan struct{}) (context.Context, func() pendingResult) {
	t.Helper()
	done := make(chan pendingResult, 1)
	go func() {
		lease, err := manager.Acquire(t.Context(), profileID, turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
		done <- pendingResult{lease: lease, err: err}
	}()
	finish := sync.OnceValue(func() pendingResult {
		close(proceed)
		select {
		case result := <-done:
			return result
		case <-time.After(time.Second):
			t.Fatal("issuance did not finish")
			return pendingResult{}
		}
	})
	t.Cleanup(func() {
		if lease := finish().lease; lease != nil {
			lease.Close()
		}
	})
	select {
	case request := <-started:
		return request, finish
	case <-time.After(time.Second):
		t.Fatal("issuance did not start")
		return nil, finish
	}
}

func TestConfigurationInvalidatesOnlyDependentIssuance(t *testing.T) {
	for _, test := range []struct {
		name       string
		superseded bool
		change     func(*testing.T, *accountManager, *accountConfig)
	}{
		{name: "other_account_budget", change: func(_ *testing.T, _ *accountManager, config *accountConfig) {
			config.Accounts[1].LocalBudgetBytes = 200
		}},
		{name: "own_budget_increase", change: func(_ *testing.T, _ *accountManager, config *accountConfig) {
			config.Accounts[0].LocalBudgetBytes = 200
		}},
		{name: "own_budget_still_available", change: func(_ *testing.T, _ *accountManager, config *accountConfig) {
			config.Accounts[0].LocalBudgetBytes = 60
		}},
		{name: "sibling_profile_endpoints", change: func(_ *testing.T, _ *accountManager, config *accountConfig) {
			config.Profiles[1].URLs = []string{"turn:localhost:3480"}
		}},
		{name: "managed_service_added", change: func(t *testing.T, manager *accountManager, _ *accountConfig) {
			if err := manager.SetManagedServices([]managedService{{
				ID: "discovered", RelayURL: "https://relay.example", Source: &credentialIssuer{source: managedSource{}},
			}}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "issuer_rotation", superseded: true, change: func(_ *testing.T, _ *accountManager, config *accountConfig) {
			config.Accounts[0].Source = &credentialIssuer{source: newVersionedSource("replacement")}
		}},
		{name: "profile_endpoints", superseded: true, change: func(_ *testing.T, _ *accountManager, config *accountConfig) {
			config.Profiles[0].URLs = []string{"turn:localhost:3480"}
		}},
		{name: "account_removal_and_reimport", superseded: true, change: func(t *testing.T, manager *accountManager, _ *accountConfig) {
			reconfigure(t, manager, accountConfig{})
		}},
		{name: "profile_removal_and_reimport", superseded: true, change: func(t *testing.T, manager *accountManager, config *accountConfig) {
			removed := cloneConfig(*config)
			removed.Profiles = removed.Profiles[1:]
			reconfigure(t, manager, removed)
		}},
		{name: "budget_revoked_then_restored", superseded: true, change: func(t *testing.T, manager *accountManager, config *accountConfig) {
			exhausted := cloneConfig(*config)
			exhausted.Accounts[0].LocalBudgetBytes = 30
			reconfigure(t, manager, exhausted)
			config.Accounts[0].LocalBudgetBytes = 200
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &lateSource{versionedSource: newVersionedSource("original"),
				started: make(chan context.Context, 1), proceed: make(chan struct{})}
			config := configurationFor(source.versionedSource, 100)
			config.Accounts = append(config.Accounts, account{ID: "other", Provider: "test",
				LocalBudgetBytes: 100, Source: &credentialIssuer{source: managedSource{}}})
			config.Profiles = append(config.Profiles, profile{ID: "sibling", AccountID: "owner", URLs: []string{"turn:localhost:3479"}})
			manager := newConfiguredManager(t, config)
			first := acquire(t, manager, "primary", 1)
			first.Charge(turnpath.Usage{TransportSent: 40})
			first.Close()
			awaitRevocation(t, source.versionedSource, "original")
			config.Accounts[0].Source = &credentialIssuer{source: source}
			reconfigure(t, manager, config)
			request, finish := pendingAcquisition(t, manager, "primary", source.started, source.proceed)
			test.change(t, manager, &config)
			reconfigure(t, manager, config)
			if test.superseded != errors.Is(context.Cause(request), turnpath.ErrSuperseded) {
				t.Fatalf("unexpected request cancellation: %v", context.Cause(request))
			}
			result := finish()
			if test.superseded {
				if result.lease != nil || !errors.Is(result.err, turnpath.ErrSuperseded) {
					t.Fatalf("late authority survived invalidation: %+v", result)
				}
				awaitRevocation(t, source.versionedSource, "original")
			} else if result.err != nil || result.lease == nil || result.lease.Loss() != nil {
				t.Fatalf("unaffected issuance was lost: %+v", result)
			}
			if manager.Snapshot()[0].ChargedBytes != 40 {
				t.Fatal("configuration change reset account usage")
			}
		})
	}
}

func TestRenewalRetainsIssuerAcrossBudgetAndProfileEdits(t *testing.T) {
	source := newVersionedSource("original")
	config := configurationFor(source, 100)
	manager := newConfiguredManager(t, config)
	first := acquire(t, manager, "primary", 1)
	config.Accounts[0].LocalBudgetBytes = 200
	config.Profiles[0].URLs = []string{"turn:localhost:3479"}
	reconfigure(t, manager, config)
	second, err := first.Renew(t.Context(), turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	source.mu.Lock()
	renewed := source.renewed
	source.mu.Unlock()
	if renewed != 1 || second.Relay().URLs()[0] != "turn:localhost:3479" {
		t.Fatal("unchanged issuer lost renewal ownership or ignored new endpoints")
	}
	reconfigure(t, manager, accountConfig{})
	reconfigure(t, manager, config)
	third, err := second.Renew(t.Context(), turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{3}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(third.Close)
	source.mu.Lock()
	renewed = source.renewed
	source.mu.Unlock()
	if renewed != 1 {
		t.Fatal("revoked predecessor's renewal material was reused after reimport")
	}
}
