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
		c, err = c.ReplaceIdentity(relayprofile.Candidate{PublicURL: fmt.Sprintf("https://relay%d.example", i)})
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
	c, err := c.ReplaceIdentity(relayprofile.Candidate{PublicURL: "https://paired.example"})
	if err != nil || !slices.Equal(active, c.ActiveRelays) {
		t.Fatal(c.ActiveRelays, err)
	}
}

func TestSelectedModeEnablesNewRelaysWithoutReenablingDisabledOnes(t *testing.T) {
	var c Config
	if err := c.SetMode(Selected); err != nil {
		t.Fatal(err)
	}
	c, err := c.ReplaceIdentity(relayprofile.Candidate{PublicURL: "https://first.example"})
	if err != nil || !slices.Equal(c.ActiveRelays, []string{"https://first.example"}) {
		t.Fatalf("first selected relay = %v, err=%v", c.ActiveRelays, err)
	}
	if err := c.SetEnabled("https://first.example", false); err != nil {
		t.Fatal(err)
	}
	// Re-merging a deliberately disabled profile must keep it disabled.
	c, err = c.ReplaceIdentity(relayprofile.Candidate{PublicURL: "https://first.example", ConsoleURL: "https://console.example"})
	if err != nil || len(c.ActiveRelays) != 0 {
		t.Fatalf("re-merged disabled relay = %v, err=%v", c.ActiveRelays, err)
	}
	// Adding a distinct new relay enables it while preserving the disabled first profile.
	c, err = c.ReplaceIdentity(relayprofile.Candidate{PublicURL: "https://second.example"})
	if err != nil || !slices.Equal(c.ActiveRelays, []string{"https://second.example"}) {
		t.Fatalf("second selected relay = %v, err=%v", c.ActiveRelays, err)
	}
}

func TestManualCredentialKeepsDiscoveryAndRediscoveryCanWithdrawIt(t *testing.T) {
	p := relayprofile.Candidate{PublicURL: "https://relay.example", ConsoleURL: "https://console.example", OfferedFrontend: "https://self.example", Role: relayprofile.Owner, AccessKey: "old"}
	c := Config{Profiles: []relayprofile.Candidate{p}, FrontURL: "https://saved.example"}
	manual, err := c.ReplaceIdentity(c.CredentialProfile(p.PublicURL, "new"))
	if err != nil {
		t.Fatal(err)
	}
	updated := manual.Profiles[0]
	if updated.AccessKey != "new" || updated.Role != relayprofile.Service || updated.ConsoleURL != p.ConsoleURL || updated.OfferedFrontend != p.OfferedFrontend || len(manual.ActiveRelays) != 0 || manual.FrontURL != c.FrontURL {
		t.Fatalf("manual installation changed saved choices or role: %+v", updated)
	}
	rediscovered, err := manual.RefreshMetadata(relayprofile.Candidate{PublicURL: p.PublicURL})
	if err != nil || rediscovered.Profiles[0].AccessKey != "new" || rediscovered.Profiles[0].Role != relayprofile.Service || rediscovered.Profiles[0].ConsoleURL != "" || rediscovered.Profiles[0].OfferedFrontend != "" {
		t.Fatalf("stale discovery, role, or missing credential: %+v err=%v", rediscovered.Profiles, err)
	}
}

func TestRemoveRelayCleansActiveSelection(t *testing.T) {
	c := Config{
		Profiles: []relayprofile.Candidate{
			{PublicURL: "https://one.example", AccessKey: "one-key"},
			{PublicURL: "https://two.example", AccessKey: "two-key"},
		},
		ActiveRelays: []string{"https://one.example", "https://two.example"},
	}
	if err := (PreferenceEdit{Action: "remove", Value: "wss://ONE.EXAMPLE:443/"}).Apply(&c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("config invalid after relay removal: %v", err)
	}
	if len(c.Profiles) != 1 || c.Profiles[0].PublicURL != "https://two.example" {
		t.Fatalf("profiles = %+v", c.Profiles)
	}
	if !slices.Equal(c.ActiveRelays, []string{"https://two.example"}) {
		t.Fatalf("active relays = %v", c.ActiveRelays)
	}
	if err := c.RemoveRelay("https://one.example"); err == nil {
		t.Fatal("removing an unsaved relay succeeded")
	}
}

func TestPreferenceEditUsesCanonicalCommunityNodesAction(t *testing.T) {
	var c Config
	if err := (PreferenceEdit{Action: "community_nodes", Value: "off"}).Apply(&c); err != nil {
		t.Fatalf("community_nodes off failed: %v", err)
	}
	if c.CommunityEnabled() {
		t.Fatal("community_nodes off did not disable community nodes")
	}
	if err := (PreferenceEdit{Action: "community_nodes", Value: "on"}).Apply(&c); err != nil {
		t.Fatalf("community_nodes on failed: %v", err)
	}
	if !c.CommunityEnabled() {
		t.Fatal("community_nodes on did not enable community nodes")
	}
	if err := (PreferenceEdit{Action: "community", Value: "off"}).Apply(&c); err == nil {
		t.Fatal("non-canonical community alias was accepted")
	}
}
