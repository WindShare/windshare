package networkruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/account"
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
	r, err := New(nil, func() time.Time { return fixture.Now }, "", diagnostics.Sink{}, nil)
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
	r, err := New(nil, time.Now, "", diagnostics.Sink{}, nil)
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
	}}, time.Now, "", diagnostics.Sink{}, nil)
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
	}, time.Now, "", stream.Sink().Bind(scope), nil)
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

func TestEngineDiagnosticsCaptureAccountIssuanceWithoutSlog(t *testing.T) {
	var slogOutput bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&slogOutput, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	data, err := os.ReadFile("../../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Now     int64
		Root    string
		Session map[string]any
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	root, err := serviceauthority.PublicKey(fixture.Root)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Secret string `json:"client_secret"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		reply := make(map[string]any, len(fixture.Session))
		maps.Copy(reply, fixture.Session)
		if body.Secret != "" {
			reply["credential_secret"] = body.Secret
		}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	defer server.Close()

	stream, err := diagnostics.NewStream(32, nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := diagnostics.Scope{EngineID: "engine-acct"}
	r, err := New(&networkconfig.Config{
		Directory: networkdirectory.Config{Network: "test"},
		Authority: &account.Config{
			URL:   server.URL,
			Root:  root,
			Store: &account.MemoryStore{},
		},
	}, func() time.Time { return time.Unix(fixture.Now, 0) }, "", stream.Sink().Bind(scope), nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := r.Accounts().RefreshOnce(t.Context()); err != nil || outcome != account.RefreshSucceeded {
		r.Close()
		t.Fatalf("refresh = %s, %v", outcome, err)
	}
	r.Close()
	stream.Close()

	var sawState, sawIssued bool
	for record := range stream.Records() {
		if record.Scope() != scope {
			t.Fatalf("unexpected scope: %+v", record.Scope())
		}
		switch record.Name() {
		case "account_state_changed":
			sawState = true
		case "account_ticket_issued":
			sawIssued = true
		}
	}
	if !sawState || !sawIssued {
		t.Fatalf("missing account diagnostics in engine stream: state=%t issued=%t", sawState, sawIssued)
	}
	if slogOutput.Len() != 0 {
		t.Fatalf("networkruntime account service bypassed diagnostics sink via slog: %s", slogOutput.String())
	}
}

type fakeNetworkSource struct {
	mu    sync.Mutex
	state networkstate.State
}

func (s *fakeNetworkSource) Snapshot(context.Context) (networkstate.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, nil
}

func (s *fakeNetworkSource) setIP(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = networkstate.State{Addresses: []networkstate.Address{{IP: netip.MustParseAddr(ip), InterfaceIndex: 1}}}
}

func TestNetworkChangeResetsDirectoryBackoffForExplicitAliasResolve(t *testing.T) {
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
	var clock atomic.Int64
	clock.Store(fixture.Now.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }

	source := &fakeNetworkSource{}
	source.setIP("192.0.2.1")
	watcher := networkstate.NewWatcher(networkstate.WatcherConfig{
		Source:   source,
		Debounce: networkstate.DefaultDebounce,
		Interval: time.Hour,
		Now:      now,
	})
	defer watcher.Close()

	var online atomic.Bool
	r, err := New(&networkconfig.Config{
		Directory: networkdirectory.Config{
			Network: "test",
			Roots:   []nodedirectory.Root{{ID: fixture.Roots[0].ID, Key: ed25519.PublicKey(key)}},
			Mirrors: []string{"https://mirror.example/directory"},
			Fetch: func(context.Context, string) ([]byte, error) {
				if !online.Load() {
					return nil, errors.New("offline")
				}
				return fixture.Entries[0].Envelope, nil
			},
		},
	}, now, "", diagnostics.Sink{}, watcher)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Seed failure backoff via an explicit alias refresh while offline.
	_, expand, err := r.Resolve(link.Link{NodeAliases: []string{"jp1"}})
	if err != nil || expand == nil {
		t.Fatalf("Resolve = (expand=%t, err=%v)", expand != nil, err)
	}
	var unavailable *networkdirectory.UnavailableError
	if _, err := expand(t.Context()); !errors.As(err, &unavailable) || unavailable.RetryAt.IsZero() {
		t.Fatalf("expected offline cooldown error, got %v", err)
	}

	// Initial baseline observation must not reset directory backoff.
	if _, err := watcher.Snapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if r.directory.RetryAt().IsZero() {
		t.Fatal("initial network poll unexpectedly cleared directory backoff")
	}

	// Switching networks after debounce clears directory backoff via the shared watcher.
	online.Store(true)
	source.setIP("192.0.2.2")
	_, _, _ = watcher.Step(t.Context())
	clock.Store(now().Add(networkstate.DefaultDebounce).UnixNano())
	_, _, _ = watcher.Step(t.Context())
	deadline := time.Now().Add(time.Second)
	for !r.directory.RetryAt().IsZero() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !r.directory.RetryAt().IsZero() {
		t.Fatal("network change did not clear directory backoff")
	}

	known, expand, err := r.Resolve(link.Link{NodeAliases: []string{"jp1"}})
	if err != nil {
		t.Fatalf("Resolve after network change failed: %v", err)
	}
	if len(known) == 0 && expand != nil {
		known, err = expand(t.Context())
		if err != nil {
			t.Fatalf("alias expand after network change failed: %v", err)
		}
	}
	if !slices.Equal(known, []string{"https://jp1.example"}) {
		t.Fatalf("resolved endpoints = %v, want [https://jp1.example]", known)
	}
}
