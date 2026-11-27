package relayconfig

import (
	"encoding/json"
	"errors"
	"os"
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

func TestConfigWritesReconcileEligibilityAndReadsStayLockFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := FileStore{Path: path}
	owner := relayprofile.Candidate{
		PublicURL: "https://owner.example",
		AccessKey: "owner-key",
		Role:      relayprofile.Owner,
		Node:      relayprofile.NodeIdentity{Platform: "native", NodeKey: "test-node"},
	}
	readStored := func() Eligibility {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(statefile.StateDirectory(path), statefile.CommunityEligibility))
		if err != nil {
			t.Fatal(err)
		}
		var e Eligibility
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		return e
	}
	pauseOwner := func() {
		t.Helper()
		cfg := emptyConfig()
		cfg.Profiles = []relayprofile.Candidate{owner}
		if err := store.Save(cfg); err != nil {
			t.Fatal(err)
		}
		if err := statefile.Transact(t.Context(), path, func(tx *statefile.Transaction) error {
			e, err := ReconcileEligibility(tx, cfg, map[string]serviceauthority.PublicRelay{owner.Node.NodeKey: serviceauthority.RelayDisabled})
			if err != nil || !e.Paused {
				t.Fatalf("pause reconcile=(%+v, %v)", e, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if show, err := store.TakeCommunityNotice(t.Context()); err != nil || !show {
			t.Fatalf("notice=(%t, %v)", show, err)
		}
	}

	// FileStore.Update must reconcile disk eligibility when removing the sole disabled owner.
	pauseOwner()
	if _, err := store.Update(t.Context(), func(c *Config) error {
		return c.RemoveRelay(owner.PublicURL)
	}); err != nil {
		t.Fatal(err)
	}
	if stored := readStored(); stored.Paused || stored.Notified || len(stored.Disabled) != 0 {
		t.Fatalf("Update left stale disk eligibility: %+v", stored)
	}

	// FileStore.Save must also reconcile disk eligibility on full config replacement.
	pauseOwner()
	if err := store.Save(emptyConfig()); err != nil {
		t.Fatal(err)
	}
	if stored := readStored(); stored.Paused || stored.Notified || len(stored.Disabled) != 0 {
		t.Fatalf("Save left stale disk eligibility: %+v", stored)
	}

	// Manual out-of-band edit of config.json still evaluates accurately on lock-free read.
	pauseOwner()
	rawEmpty, err := Encode(emptyConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rawEmpty, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	_, e, err := store.ReadCommunityState(t.Context())
	if err != nil || e.Paused {
		t.Fatalf("ReadCommunityState after manual edit=(%+v, %v)", e, err)
	}
	if show, err := store.TakeCommunityNotice(t.Context()); err != nil || show {
		t.Fatalf("TakeCommunityNotice after manual edit=(%t, %v)", show, err)
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock-free reads recreated lock file: err=%v", err)
	}
}

func TestCorruptEligibilityDoesNotBrickConfigWritesAndRepairsOnSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := FileStore{Path: path}
	owner := relayprofile.Candidate{
		PublicURL: "https://owner.example",
		AccessKey: "owner-key",
		Role:      relayprofile.Owner,
		Node:      relayprofile.NodeIdentity{Platform: "native", NodeKey: "test-node"},
	}
	cfg := emptyConfig()
	cfg.Profiles = []relayprofile.Candidate{owner}
	cfg.ActiveRelays = []string{owner.PublicURL}
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}

	eligibilityPath := filepath.Join(statefile.StateDirectory(path), statefile.CommunityEligibility)
	corruptEligibility := func() {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(eligibilityPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(eligibilityPath, []byte("{corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// ReadCommunityState must return the valid Config alongside ErrCorruptEligibility.
	corruptEligibility()
	gotCfg, _, err := store.ReadCommunityState(t.Context())
	if !errors.Is(err, ErrCorruptEligibility) || len(gotCfg.OwnedNative()) != 1 {
		t.Fatalf("ReadCommunityState = (owned=%d, err=%v), want owned=1 and ErrCorruptEligibility", len(gotCfg.OwnedNative()), err)
	}

	// FileStore.Update must not fail on corrupt derived state and must repair the file on disk.
	if _, err := store.Update(t.Context(), func(c *Config) error {
		return c.SetMode(Selected)
	}); err != nil {
		t.Fatalf("FileStore.Update blocked by corrupt eligibility: %v", err)
	}
	if _, _, err := store.ReadCommunityState(t.Context()); err != nil {
		t.Fatalf("ReadCommunityState after Update repair failed: %v", err)
	}

	// FileStore.Save must also repair corrupt derived eligibility state.
	corruptEligibility()
	if err := store.Save(emptyConfig()); err != nil {
		t.Fatalf("FileStore.Save blocked by corrupt eligibility: %v", err)
	}
	if _, _, err := store.ReadCommunityState(t.Context()); err != nil {
		t.Fatalf("ReadCommunityState after Save repair failed: %v", err)
	}

	// Corrupting config.json itself must still fail with a non-eligibility error.
	if err := os.WriteFile(path, []byte("{broken-config"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReadCommunityState(t.Context()); err == nil || errors.Is(err, ErrCorruptEligibility) {
		t.Fatalf("corrupt config.json returned %v, want non-eligibility error", err)
	}
}
