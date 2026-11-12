package engine_test

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

// A parsed capability must reach the same authenticated relay endpoint as the
// sender's configuration, including copied Unicode hosts and explicit hints.
func TestUnicodeRelayConfigurationAndReceiveLinkShareIdentity(t *testing.T) {
	capability, _, key := receiveLinkFixture(t)
	shareBytes, err := base64.RawURLEncoding.DecodeString(capability.ShareID)
	if err != nil {
		t.Fatal(err)
	}
	shareID, err := v2.ShareIDFromBytes(shareBytes)
	if err != nil {
		t.Fatal(err)
	}
	want, err := v2.RelayEndpointForShare("https://xn--mnchen-3ya.de", shareID)
	if err != nil {
		t.Fatal(err)
	}

	for _, relay := range []string{"https://münchen.de", "https://xn--mnchen-3ya.de"} {
		if err := (relayprofile.Candidate{PublicURL: relay}).Validate(); err != nil {
			t.Fatal(err)
		}
		if err := relayconfig.ValidateFrontURL(relay); err != nil {
			t.Fatal(err)
		}
		capability.Relays = []string{relay}
		for _, front := range []string{"https://münchen.de", "https://xn--mnchen-3ya.de"} {
			published, err := capability.URL(front)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(published, "?r=") {
				t.Fatalf("same relay was not implicit: %s", published)
			}
			inputs := []engine.ReceiveLinkInput{
				{URL: published},
				{URL: front + "/" + capability.ShareID, SeparateKey: key},
				{URL: "https://front.example/" + capability.ShareID + "?r=" + url.QueryEscape(relay) + "#" + key},
			}
			for _, input := range inputs {
				resolved, err := engine.ResolveReceiveLink(input)
				if err != nil {
					t.Fatal(err)
				}
				if len(resolved.Relays) != 1 {
					t.Fatalf("relays = %v", resolved.Relays)
				}
				got, err := v2.RelayEndpointForShare(resolved.Relays[0], shareID)
				if err != nil || got != want {
					t.Fatalf("receive relay = %+v, error = %v, want %+v", got, err, want)
				}
			}
		}
	}
}
