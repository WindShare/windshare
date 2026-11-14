package networkruntime

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/relayprofile"
)

func fixtureRuntime(t *testing.T) *Runtime {
	t.Helper()
	data, err := os.ReadFile("../../../testdata/network/directory.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Now     time.Time
		Roots   []struct{ ID, Key string }
		Entries []struct{ Envelope json.RawMessage }
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	key, _ := base64.RawURLEncoding.DecodeString(fixture.Roots[0].Key)
	r, err := New(nil, func() time.Time { return fixture.Now })
	if err != nil {
		t.Fatal(err)
	}
	r.directory, err = networkdirectory.New(networkdirectory.Config{Network: "test", Roots: []nodedirectory.Root{{ID: fixture.Roots[0].ID, Key: ed25519.PublicKey(key)}},
		Embedded: fixture.Entries[0].Envelope, Now: r.now})
	if err != nil {
		t.Fatal(err)
	}
	r.config.LinkHost, r.config.BackupEntry = "https://front.example", "https://backup.example"
	t.Cleanup(r.Close)
	return r
}

func TestPlanningPreservesExplicitIntentAndFrontendAuthority(t *testing.T) {
	r := fixtureRuntime(t)
	candidate := relayprofile.Candidate{PublicURL: "https://own.example"}
	for _, test := range []struct {
		intent relayprofile.Intent
		count  int
	}{
		{relayprofile.Plan([]relayprofile.Candidate{candidate}), 2},
		{relayprofile.OnlyConfigured([]relayprofile.Candidate{candidate}), 1},
		{relayprofile.UseExplicit([]relayprofile.Candidate{candidate}), 1},
	} {
		selected, err := r.Plan(context.Background(), test.intent, "https://explicit.example")
		if err != nil || len(selected.Relays) != test.count || selected.Frontend != "https://explicit.example" {
			t.Fatal(selected, err)
		}
	}
	r.linkUnreachable = true
	if got, _ := r.frontend(""); got != r.config.LinkHost {
		t.Fatal("unverified backup selected")
	}
	r.backupReachable = true
	if got, _ := r.frontend(""); got != r.config.BackupEntry {
		t.Fatal(got)
	}
	if _, err := r.Plan(context.Background(), relayprofile.OnlyConfigured(nil), ""); !errors.Is(err, relayprofile.ErrCandidatesRequired) || errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := r.Plan(context.Background(), relayprofile.UseExplicit([]relayprofile.Candidate{{PublicURL: "bad"}}), ""); err == nil {
		t.Fatal("invalid explicit relay")
	}
	if _, err := r.Plan(context.Background(), relayprofile.Plan(nil), "invalid"); err == nil {
		t.Fatal("invalid explicit frontend")
	}
}

func TestAliasResolutionReturnsKnownPathsBeforeRefresh(t *testing.T) {
	r := fixtureRuntime(t)
	known, expand, err := r.Resolve(link.Link{Relays: []string{"https://own.example"}, NodeAliases: []string{"jp1", "unknown"}})
	if err != nil || len(known) != 2 || expand == nil {
		t.Fatal(known, err)
	}
	if _, err := expand(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	known, expand, err = r.Resolve(link.Link{Relays: []string{"https://jp1.example"}, NodeAliases: []string{"jp1"}})
	if err != nil || len(known) != 1 || expand != nil {
		t.Fatal(known, err)
	}
	if _, _, err := r.Resolve(link.Link{NodeAliases: []string{"BAD"}}); err == nil {
		t.Fatal("invalid alias")
	}
	if _, _, err := r.Resolve(link.Link{NodeAliases: make([]string, 9)}); err == nil {
		t.Fatal("unbounded aliases")
	}
	if _, err := uniqueEndpoints([]string{"https://a.example", "invalid"}); err == nil {
		t.Fatal("invalid endpoint")
	}
}

func TestLibraryRuntimeHasNoImplicitInfrastructure(t *testing.T) {
	r, err := New(nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Plan(context.Background(), relayprofile.Plan(nil), ""); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if failure := r.Unavailability(); failure == nil || failure.Reason != networkdirectory.Unprovisioned {
		t.Fatal("unprovisioned runtime was reported as an outage", failure)
	}
	if _, _, err := r.Resolve(link.Link{NodeAliases: []string{"jp1"}}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	known, expand, err := r.Resolve(link.Link{Relays: []string{"https://explicit.example"}, NodeAliases: []string{"jp1"}})
	if err != nil || len(known) != 1 || expand != nil {
		t.Fatal(known, err)
	}
}

type brokenCache struct {
	networkdirectory.MemoryCache
	err error
}

func (c *brokenCache) Load(context.Context) (networkdirectory.Record, error) {
	return networkdirectory.Record{}, c.err
}

func TestUnreadableTrustStatePreservesExplicitRelays(t *testing.T) {
	cause := errors.New("trust state unavailable")
	r, err := New(&networkconfig.Config{Directory: networkdirectory.Config{
		Network: "test", Cache: &brokenCache{err: cause},
	}}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	candidate := relayprofile.Candidate{PublicURL: "https://own.example"}
	for _, intent := range []relayprofile.Intent{relayprofile.Plan([]relayprofile.Candidate{candidate}), relayprofile.UseExplicit([]relayprofile.Candidate{candidate})} {
		selected, err := r.Plan(context.Background(), intent, "https://front.example")
		if err != nil || len(selected.Relays) != 1 || selected.Relays[0] != candidate {
			t.Fatal(selected, err)
		}
	}
	if _, err := r.Plan(context.Background(), relayprofile.Plan(nil), ""); !errors.Is(err, ErrUnavailable) || !errors.Is(err, cause) {
		t.Fatal("directory must fail closed without resetting trust", err)
	}
	if failure := r.Unavailability(); failure == nil || failure.Reason != networkdirectory.TrustStateUnreadable || !failure.RetryAt.IsZero() {
		t.Fatal("local trust failure was reported as a temporary outage", failure)
	}
}
