package relayconfig

import (
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/statefile"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestEligibilityOnlyTracksEnrolledNativeOwnersAndNotifiesOnce(t *testing.T) {
	store := FileStore{Path: filepath.Join(t.TempDir(), "config.json")}
	owner := relayprofile.Candidate{PublicURL: "https://owner.example", AccessKey: "owner-key", Role: relayprofile.Owner, Node: relayprofile.NodeIdentity{Platform: "native", NodeKey: "test-node"}}
	config := emptyConfig()
	set := func(p relayprofile.Candidate) {
		config.Profiles = []relayprofile.Candidate{p}
		if err := store.Save(config); err != nil {
			t.Fatal(err)
		}
	}
	observe := func(state serviceauthority.PublicRelay, paused bool) {
		t.Helper()
		err := statefile.Transact(t.Context(), store.Path, func(tx *statefile.Transaction) error {
			e, err := ReconcileEligibility(tx, config, map[string]serviceauthority.PublicRelay{owner.Node.NodeKey: state})
			if e.Paused != paused {
				t.Fatalf("state %v paused=%t, want %t", state, e.Paused, paused)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	notice := func(want bool) {
		t.Helper()
		got, err := store.TakeCommunityNotice(t.Context())
		if err != nil || got != want {
			t.Fatal(want, got, err)
		}
	}
	for _, role := range []relayprofile.CredentialRole{relayprofile.Service, relayprofile.Account, ""} {
		p := owner
		p.Role = role
		if role == "" {
			p.AccessKey = ""
		}
		set(p)
		observe(serviceauthority.RelayDisabled, false)
	}
	p := owner
	p.Node.Platform = "cloudflare"
	set(p)
	observe(serviceauthority.RelayDisabled, false)
	p = owner
	p.Node.Authority = "issuer"
	set(p)
	observe(serviceauthority.RelayDisabled, false)
	set(owner)
	observe(serviceauthority.RelayDisabled, true)
	notice(true)
	notice(false)
	observe(serviceauthority.RelayUnknown, true)
	notice(false)
	for _, s := range []serviceauthority.PublicRelay{serviceauthority.RelayBudgetExhausted, serviceauthority.RelayUnavailable, serviceauthority.RelayAnonymous} {
		observe(s, false)
		notice(false)
	}
	observe(serviceauthority.RelayDisabled, true)
	notice(true)
}
