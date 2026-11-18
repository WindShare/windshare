package relayconfig

import (
	"fmt"
	"slices"
	"testing"

	"github.com/windshare/windshare/engine/relayprofile"
)

func TestNewRelaysUseOnlyAvailableAutomaticSlots(t *testing.T) {
	var c Config
	for i := range relayprofile.MaximumEndpoints + 1 {
		var err error
		c, err = c.MergeRelay(relayprofile.Candidate{PublicURL: fmt.Sprintf("https://relay%d.example", i)})
		if err != nil {
			t.Fatal(err)
		}
	}
	view := c.Preferences("https://receiver.example")
	if !view.AtCapacity || view.Relays[relayprofile.MaximumEndpoints].Enabled {
		t.Fatal(view)
	}
	last := view.Relays[relayprofile.MaximumEndpoints].URL
	if c.SetEnabled(last, true) == nil {
		t.Fatal("enabled a ninth endpoint")
	}
	if err := c.SetEnabled(view.Relays[0].URL, false); err != nil {
		t.Fatal(err)
	}
	if err := c.SetEnabled(last, true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetMode(Selected); err != nil {
		t.Fatal(err)
	}
	active := slices.Clone(c.ActiveRelays)
	c, err := c.MergeRelay(relayprofile.Candidate{PublicURL: "https://paired.example"})
	if err != nil || !slices.Equal(active, c.ActiveRelays) {
		t.Fatal(c.ActiveRelays, err)
	}
}

func TestManualCredentialKeepsDiscoveryAndRediscoveryCanWithdrawIt(t *testing.T) {
	p := relayprofile.Candidate{PublicURL: "https://relay.example", ConsoleURL: "https://console.example", OfferedFrontend: "https://self.example", AccessKey: "old"}
	c := Config{Profiles: []relayprofile.Candidate{p}, FrontURL: "https://saved.example"}
	manual, err := c.MergeRelay(c.CredentialProfile(p.PublicURL, "new"))
	if err != nil {
		t.Fatal(err)
	}
	updated := manual.Profiles[0]
	if updated.AccessKey != "new" || updated.ConsoleURL != p.ConsoleURL || updated.OfferedFrontend != p.OfferedFrontend || len(manual.ActiveRelays) != 0 || manual.FrontURL != c.FrontURL {
		t.Fatal("manual installation changed saved choices")
	}
	rediscovered, err := manual.MergeRelay(relayprofile.Candidate{PublicURL: p.PublicURL})
	if err != nil || rediscovered.Profiles[0].AccessKey != "new" || rediscovered.Profiles[0].ConsoleURL != "" || rediscovered.Profiles[0].OfferedFrontend != "" {
		t.Fatal("stale discovery or missing credential", err)
	}
}
