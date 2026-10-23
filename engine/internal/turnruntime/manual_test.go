package turnruntime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/turnpath"
)

func TestSetupIsOfflineAndPreservesIssuerCapabilities(t *testing.T) {
	for _, issuer := range []string{"static", "coturn", "cloudflare"} {
		t.Run(issuer, func(t *testing.T) {
			credentials := map[string]any{
				"static":     StaticCredentials{Username: "user", Password: "password"},
				"coturn":     CoturnCredentials{SharedSecret: "secret", LifetimeSeconds: 60},
				"cloudflare": CloudflareCredentials{KeyID: "key", APIToken: "secret", LifetimeSeconds: 60},
			}[issuer]
			encoded, _ := json.Marshal(credentials)
			config := ManualSettings{Accounts: []AccountSettings{{ID: "account", Provider: issuer, Issuer: issuer, Credentials: encoded}}, Profiles: []ProfileSettings{{ID: "profile", Account: "account", URLs: []string{"turn:localhost"}}}}
			if err := config.Validate(); err != nil {
				t.Fatal(err)
			}
			runtime, err := New(Settings{Manual: config}, Dependencies{Now: func() time.Time { return time.Unix(1700000000, 0) }})
			if err != nil {
				t.Fatal(err)
			}
			manager := runtime.accounts
			if issuer != "cloudflare" {
				lease, err := manager.Acquire(context.Background(), "profile", turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{2}})
				if err != nil {
					t.Fatal(err)
				}
				if lease.Renewable() != (issuer == "coturn") {
					t.Fatal("incorrect capability")
				}
				if lease.Renewable() {
					fresh, err := lease.Renew(context.Background(), turnpath.Subject{SessionID: [16]byte{1}, PathID: [16]byte{3}})
					if err != nil {
						t.Fatal(err)
					}
					if fresh.Relay().Credentials().Username() == lease.Relay().Credentials().Username() {
						t.Fatal("renewal retained old path authority")
					}
					fresh.Close()
				}
				lease.Close()
			}
			if err := runtime.Close(); err != nil {
				t.Fatal(err)
			}
			config.Profiles[0].URLs = nil
			if (config.Validate() == nil) != (issuer == "cloudflare") {
				t.Fatal("missing endpoints accepted without discovery capability")
			}
		})
	}
}
func TestInvalidIssuerSecretsAndProfileReferences(t *testing.T) {
	valid := json.RawMessage(`{"username":"u","password":"p"}`)
	for _, account := range []AccountSettings{
		{ID: "account", Provider: "turn", Issuer: "unknown", Credentials: valid},
		{ID: "account", Provider: "turn", Issuer: "static", Credentials: json.RawMessage(`{"username":"u","password":"p","api_token":"extra"}`)},
		{ID: "account", Provider: "turn", Issuer: "static", Credentials: json.RawMessage(`null`)},
		{ID: "account", Provider: "turn", Issuer: "coturn", Credentials: json.RawMessage(`{"shared_secret":"s","lifetime_seconds":1}`)},
	} {
		config := ManualSettings{Accounts: []AccountSettings{account}, Profiles: []ProfileSettings{{ID: "profile", Account: "account", URLs: []string{"turn:localhost"}}}}
		if config.Validate() == nil {
			t.Fatal("accepted invalid credentials")
		}
	}
	config := ManualSettings{Accounts: []AccountSettings{{ID: "account", Provider: "turn", Issuer: "static", Credentials: valid}}, Profiles: []ProfileSettings{{ID: "p", Account: "missing", URLs: []string{"turn:localhost"}}}}
	if config.Validate() == nil {
		t.Fatal("accepted absent account")
	}
}
