package turnruntime

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/windshare/windshare/engine/internal/configjson"
)

// Runtime callers can configure accounts without passing through relayconfig.
func TestCredentialOwnersRejectRepeatedMembers(t *testing.T) {
	for _, test := range []struct{ issuer, credentials string }{
		{StaticIssuer, `{"username":"u","password":"a","PASSWORD":"b"}`},
		{CloudflareIssuer, `{"key_id":"a","KEY_ID":"b","api_token":"token","lifetime_seconds":60}`},
		{CoturnIssuer, `{"shared_secret":"a","shared_secret":"a","lifetime_seconds":60}`},
	} {
		settings := ManualSettings{
			Accounts: []AccountSettings{{ID: "account", Provider: test.issuer, Issuer: test.issuer, Credentials: json.RawMessage(test.credentials)}},
			Profiles: []ProfileSettings{{ID: "profile", Account: "account", URLs: []string{"turn:localhost"}}},
		}
		if err := settings.Validate(); !errors.Is(err, ErrConfiguration) || !errors.Is(err, configjson.ErrRepeatedMember) {
			t.Fatalf("%s accepted ambiguous credentials: %v", test.issuer, err)
		}
	}
}
