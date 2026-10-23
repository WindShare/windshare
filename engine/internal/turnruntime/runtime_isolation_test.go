package turnruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestManualBindingRetainsPendingIssuerUntilCredentialsChange(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		name := "preserve"
		if rotate {
			name = "rotate"
		}
		t.Run(name, func(t *testing.T) {
			started, proceed := make(chan context.Context, 1), make(chan struct{})
			client := clientFunc(func(request *http.Request) (*http.Response, error) {
				if strings.HasSuffix(request.URL.Path, "/revoke") {
					if request.Header.Get("Authorization") != "Bearer original-token" {
						t.Error("cleanup used the replacement issuer's authority")
					}
					return reply(http.StatusNoContent, ""), nil
				}
				started <- request.Context()
				<-proceed
				return reply(http.StatusCreated, `{"iceServers":[{"urls":["turn:localhost:3478"],"username":"temporary","credential":"password"}]}`), nil
			})
			settings := Settings{Manual: manualFixture()}
			settings.Manual.Accounts = append(settings.Manual.Accounts, AccountSettings{
				ID: "other", Provider: "standard", Issuer: StaticIssuer,
				Credentials: json.RawMessage(`{"username":"other","password":"password"}`),
			})
			settings.Manual.Accounts[0].Issuer = CloudflareIssuer
			settings.Manual.Accounts[0].Credentials = json.RawMessage(`{"key_id":"0123456789abcdef0123456789abcdef","api_token":"original-token"}`)
			runtime, err := New(settings, Dependencies{Client: client})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := runtime.Close(); err != nil {
					t.Error(err)
				}
			})
			request, finish := pendingAcquisition(t, runtime.accounts, "primary", started, proceed)
			for _, update := range []func(){
				func() { settings.Manual.Accounts[1].LocalBudgetBytes = 200 },
				func() { settings.Manual.Accounts[0].LocalBudgetBytes = 200 },
				func() {
					settings.Manual.Accounts[0].Credentials = json.RawMessage(`{ "lifetime_seconds": 3600, "api_token": "original-token", "key_id": "0123456789abcdef0123456789abcdef" }`)
				},
				func() {
					settings.Manual.Accounts[0].Analytics = &CloudflareAnalytics{AccountID: "fedcba9876543210fedcba9876543210", APIToken: "reporting-token"}
				},
			} {
				update()
				if err := runtime.Configure(settings); err != nil {
					t.Fatal(err)
				}
				if request.Err() != nil {
					t.Fatal("budget, serialization or analytics change invalidated the signing identity")
				}
			}
			if rotate {
				settings.Manual.Accounts[0].Credentials = json.RawMessage(`{"key_id":"0123456789abcdef0123456789abcdef","api_token":"replacement-token"}`)
				if err := runtime.Configure(settings); err != nil {
					t.Fatal(err)
				}
				if !errors.Is(context.Cause(request), turnpath.ErrSuperseded) {
					t.Fatal("credential rotation retained pending authority")
				}
			}
			result := finish()
			if rotate {
				if result.lease != nil || !errors.Is(result.err, turnpath.ErrSuperseded) {
					t.Fatalf("rotated issuer accepted a late grant: %+v", result)
				}
			} else if result.err != nil || result.lease == nil {
				t.Fatalf("unchanged signing identity lost issuance: %+v", result)
			}
		})
	}
}

type managedIssuanceRequest struct {
	path, authorization, leaseID, previousLeaseID string
}

func TestManagedDiscoveryPreservesIndependentIssuanceAndRenewal(t *testing.T) {
	started, proceed := make(chan context.Context, 1), make(chan struct{})
	requests := make(chan managedIssuanceRequest, 4)
	var first sync.Once
	runtime, manager, cache, settings := discoveryFixture(t, func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/api/turn/capability":
			return reply(http.StatusOK, `{"turn":{"service_id":"other-service","enabled":true}}`), nil
		case "/api/turn/revoke":
			return reply(http.StatusOK, `{"state":"revoked"}`), nil
		}
		first.Do(func() {
			started <- request.Context()
			<-proceed
		})
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			return nil, err
		}
		requests <- managedIssuanceRequest{path: request.URL.Path, authorization: request.Header.Get("Authorization"),
			leaseID: body["lease_id"], previousLeaseID: body["previous_lease_id"]}
		data, err := json.Marshal(map[string]any{
			"service_id": body["service_id"], "lease_id": body["lease_id"],
			"session_id": body["session_id"], "path_id": body["path_id"],
			"urls": []string{"turn:localhost:3478"}, "username": "temporary", "password": "password",
			"expires_at": time.Now().Add(time.Hour).Truncate(time.Second).UnixMilli(),
		})
		if err != nil {
			return nil, err
		}
		return reply(http.StatusCreated, string(data)), nil
	})
	request, finish := pendingAcquisition(t, manager, "managed.cf-service", started, proceed)
	other := relayprofile.Candidate{PublicURL: "https://other.example", AccessKey: "other-device"}
	settings.Managed.Relays = append(settings.Managed.Relays, other)
	if err := runtime.Configure(settings); err != nil {
		t.Fatal(err)
	}
	runtime.discovery.Refresh(t.Context(), other)
	if len(manager.Inventory().Profiles) != 2 || request.Err() != nil {
		t.Fatal("discovering another service canceled an existing service's request")
	}
	result := finish()
	if result.err != nil || result.lease == nil {
		t.Fatalf("unaffected managed issuance failed: %+v", result)
	}
	original := <-requests
	settings.Managed.Policies[0].LocalBudgetBytes++
	settings.Managed.Policies = append(settings.Managed.Policies, ManagedPolicy{RelayURL: other.PublicURL, LocalBudgetBytes: 200})
	if err := runtime.Configure(settings); err != nil {
		t.Fatal(err)
	}
	second, err := result.lease.Renew(t.Context(), turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{3}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	renewal := <-requests
	if renewal.path != "/api/turn/renew" || renewal.previousLeaseID != original.leaseID {
		t.Fatal("unrelated discovery or budget settings lost the managed renewal predecessor")
	}
	settings.Managed.Relays[0].AccessKey = "replacement-device"
	if err := cache.Save(t.Context(), settings.Managed.Relays[0], Capability{ServiceID: "cf-service", Enabled: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Configure(settings); err != nil {
		t.Fatal(err)
	}
	third, err := second.Renew(t.Context(), turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{4}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(third.Close)
	replacement := <-requests
	if replacement.path != "/api/turn/issue" || replacement.previousLeaseID != "" || replacement.authorization != "Bearer replacement-device" {
		t.Fatal("rotated device authority reused the previous issuer's renewal token")
	}
}
