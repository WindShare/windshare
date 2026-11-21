package networkruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/core/diagnostics"
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
	r, err := New(nil, func() time.Time { return fixture.Now }, "", diagnostics.Sink{})
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
		if err != nil || len(selected.Relays) != test.count || selected.Frontend != "https://explicit.example" || selected.AlternateFrontend != "" {
			t.Fatal(selected, err)
		}
	}
	if selected, err := r.Plan(context.Background(), relayprofile.Plan(nil), ""); err != nil || selected.Frontend != r.config.LinkHost || selected.AlternateFrontend != r.config.BackupEntry {
		t.Fatalf("default built-in frontend = (%q, %q), err = %v", selected.Frontend, selected.AlternateFrontend, err)
	}
	r.linkUnreachable = true
	if got := r.frontend(""); got.primary != r.config.LinkHost || got.alternate != r.config.BackupEntry {
		t.Fatal("unverified backup selected", got)
	}
	r.backupReachable = true
	if got := r.frontend(""); got.primary != r.config.BackupEntry || got.alternate != r.config.LinkHost {
		t.Fatal(got)
	}
	if selected, err := r.Plan(context.Background(), relayprofile.Plan(nil), ""); err != nil || selected.Frontend != r.config.BackupEntry || selected.AlternateFrontend != r.config.LinkHost {
		t.Fatalf("fallback built-in frontend = (%q, %q), err = %v", selected.Frontend, selected.AlternateFrontend, err)
	}
	r.config.BackupEntry = "https://FRONT.example/"
	r.linkUnreachable, r.backupReachable = false, false
	if got := r.frontend(""); got.primary != r.config.LinkHost || got.alternate != "" {
		t.Fatalf("equivalent backup host exposed duplicate alternate: %+v", got)
	}
	r.config.BackupEntry = ""
	if got := r.frontend(""); got.primary != r.config.LinkHost || got.alternate != "" {
		t.Fatalf("missing backup host exposed alternate: %+v", got)
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

func TestPublicationKeepsPendingHintsAndOmitsRejectedRegistrations(t *testing.T) {
	selection := Selection{useAliases: true, Plan: relayplan.Plan{
		Configured: []relayplan.Candidate{{URL: "ready"}, {URL: "rejected"}, {URL: "pending"}},
		Network:    []relayplan.Candidate{{URL: "failed-node", Alias: "one"}, {URL: "pending-node", Alias: "two"}},
	}}
	capability := link.Link{Relays: []string{"ready", "rejected", "pending", "failed-node", "pending-node"}}
	selection.PublishRelayHints(&capability, []string{"rejected", "failed-node"})
	if !slices.Equal(capability.Relays, []string{"ready", "pending"}) || !slices.Equal(capability.NodeAliases, []string{"two"}) {
		t.Fatal(capability.Relays, capability.NodeAliases)
	}
	selection.useAliases = false
	custom := link.Link{Relays: []string{"ready", "pending-node"}}
	selection.PublishRelayHints(&custom, nil)
	if !slices.Contains(custom.Relays, "pending-node") || len(custom.NodeAliases) != 0 {
		t.Fatal("custom frontend lost the explicit network endpoint", custom)
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
	r, err := New(nil, time.Now, "", diagnostics.Sink{})
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
	if err != nil || len(known) != 1 || expand == nil {
		t.Fatal(known, err)
	}
	if _, err := expand(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
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
	}}, time.Now, "", diagnostics.Sink{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	candidate := relayprofile.Candidate{PublicURL: "https://own.example"}
	for _, intent := range []relayprofile.Intent{relayprofile.Plan([]relayprofile.Candidate{candidate}), relayprofile.UseExplicit([]relayprofile.Candidate{candidate})} {
		selected, err := r.Plan(context.Background(), intent, "https://front.example")
		if err != nil || len(selected.Relays) != 1 || selected.Relays[0] != candidate.Registration() {
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

func TestEngineDiagnosticsCaptureFrontendHealthAndDirectoryInitFailure(t *testing.T) {
	var slogOutput bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&slogOutput, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	linkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer linkServer.Close()
	backupServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backupServer.Close()

	stream, err := diagnostics.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := diagnostics.Scope{EngineID: "engine-net"}
	cause := errors.New("corrupt trust store")
	r, err := New(&networkconfig.Config{
		Directory:   networkdirectory.Config{Network: "test", Cache: &brokenCache{err: cause}},
		LinkHost:    linkServer.URL,
		BackupEntry: backupServer.URL,
	}, time.Now, "", stream.Sink().Bind(scope))
	if err != nil {
		t.Fatal(err)
	}
	var sawInitFailure, sawFrontendHealth bool
	deadline := time.After(5 * time.Second)
	for !(sawInitFailure && sawFrontendHealth) {
		select {
		case record := <-stream.Records():
			if record.Scope() != scope {
				t.Fatalf("unexpected scope: %+v", record.Scope())
			}
			switch record.Name() {
			case "network_directory_initialization_failed":
				if record.Payload().Get("reason").Text() == string(networkdirectory.TrustStateUnreadable) &&
					record.Payload().Get("network").Text() == "test" {
					sawInitFailure = true
				}
			case "network_frontend_health":
				if record.Payload().Get("link_host").Text() == linkServer.URL &&
					!record.Payload().Get("reachable").Bool() &&
					record.Payload().Get("backup_entry").Text() == backupServer.URL &&
					record.Payload().Get("backup_reachable").Bool() &&
					record.Payload().Get("network_generation").Uint() >= 1 {
					sawFrontendHealth = true
				}
			}
		case <-deadline:
			r.Close()
			t.Fatalf("timed out waiting for engine diagnostics: init=%t frontend=%t", sawInitFailure, sawFrontendHealth)
		}
	}
	r.Close()
	stream.Close()
	if slogOutput.Len() != 0 {
		t.Fatalf("networkruntime bypassed diagnostics sink via slog: %s", slogOutput.String())
	}
}
