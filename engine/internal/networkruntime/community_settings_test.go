package networkruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/connectivity/icepolicy"
	"github.com/windshare/windshare/connectivity/publicdial"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/community"
	"github.com/windshare/windshare/engine/internal/statefile"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestRelaySettingsChangedResetsOriginWakesDiscoveryAndDeliversNotice(t *testing.T) {
	const oldFront = "https://old-front.example"
	const newFront = "https://new-front.example"
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	var live json.RawMessage
	originSeen := make(chan string, 4)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/discovery/v1/peers":
			_ = json.NewEncoder(w).Encode([]json.RawMessage{})
		case "/.well-known/windshare":
			_, _ = w.Write(live)
		case "/v2/ws":
			origin := r.Header.Get("Origin")
			select {
			case originSeen <- origin:
			default:
			}
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
			if err != nil {
				return
			}
			_ = ws.CloseNow()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	identity := "wss://example.com:" + port + "/v2/ws"
	makeEnvelope := func(domain string, lifetime time.Duration) json.RawMessage {
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{47}, ed25519.SeedSize))
		pub := key.Public().(ed25519.PublicKey)
		digest := sha256.Sum256(append([]byte("windshare/node-identity/v1\x00"), pub...))
		enc := base64.RawURLEncoding.EncodeToString
		payload, err := json.Marshal(map[string]any{
			"version": 2, "platform": "native", "node_id": enc(digest[:]), "node_key": enc(pub),
			"relay_endpoint": identity, "services": map[string]bool{"relay": true, "stun": false},
			"public_relay": serviceauthority.RelayAnonymous,
			"issued_at":    now.Unix(), "expires_at": now.Add(lifetime).Unix(),
		})
		if err != nil {
			t.Fatal(err)
		}
		sig := ed25519.Sign(key, append([]byte(domain), payload...))
		raw, err := json.Marshal(map[string]any{"version": 2, "descriptor": enc(payload), "signature": enc(sig)})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	gossip := makeEnvelope(serviceauthority.GossipDomain, 24*time.Hour)
	live = makeEnvelope(serviceauthority.DescriptorDomain, 5*time.Minute)

	settingsPath := filepath.Join(t.TempDir(), "config.json")
	store := relayconfig.FileStore{Path: settingsPath}
	communityOff := false
	if err := store.Save(relayconfig.Config{Profiles: []relayprofile.Candidate{}, CommunityNodes: &communityOff}); err != nil {
		t.Fatal(err)
	}
	rawPeers, err := json.Marshal([]json.RawMessage{gossip})
	if err != nil {
		t.Fatal(err)
	}
	if err := statefile.Write(t.Context(), filepath.Join(statefile.StateDirectory(settingsPath), "community-peers.json"), rawPeers); err != nil {
		t.Fatal(err)
	}

	var notices atomic.Int32
	r, err := New(nil, func() time.Time { return now }, "", diagnostics.Sink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.config = networkconfig.Config{
		LinkHost:              oldFront,
		CommunitySettingsPath: settingsPath,
		CommunityPaused:       func() { notices.Add(1) },
	}
	local := netip.MustParseAddr("127.0.0.1")
	r.community = community.New(settingsPath, publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig,
	}, r.now, diagnostics.Sink{})
	r.startCommunity()
	var closed bool
	t.Cleanup(func() {
		if !closed {
			r.Close()
		}
	})
	// Simulate an earlier share having cached the old frontend origin.
	r.noteOrigin(oldFront)

	// Enable community nodes and switch front_url, then kick RelaySettingsChanged.
	if _, err := store.Update(t.Context(), func(c *relayconfig.Config) error {
		enabled := true
		c.CommunityNodes = &enabled
		c.FrontURL = newFront
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.RelaySettingsChanged(t.Context())

	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-originSeen:
			if got == newFront {
				goto warmed
			}
		case <-deadline:
			t.Fatalf("RelaySettingsChanged did not warm updated front_url %q", newFront)
		}
	}
warmed:
	// Verify pending eligibility notice fires immediately on RelaySettingsChanged,
	// and never fires after Close.
	const disabledKey = "disabled-owner-key"
	if err := statefile.Transact(t.Context(), settingsPath, func(tx *statefile.Transaction) error {
		cfg := relayconfig.Config{
			Profiles: []relayprofile.Candidate{
				{PublicURL: "https://owned.example", Role: relayprofile.Owner, AccessKey: "k", Node: relayprofile.NodeIdentity{Platform: "native", NodeKey: disabledKey}},
			},
			ActiveRelays: []string{"https://owned.example"},
		}
		data, err := relayconfig.Encode(cfg)
		if err != nil {
			return err
		}
		if err := tx.Put(statefile.Settings, data); err != nil {
			return err
		}
		_, err = relayconfig.ReconcileEligibility(tx, cfg, map[string]serviceauthority.PublicRelay{disabledKey: serviceauthority.RelayDisabled})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r.RelaySettingsChanged(t.Context())
	if notices.Load() != 1 {
		t.Fatalf("notices after RelaySettingsChanged = %d, want 1", notices.Load())
	}

	// Reset Notified=false on disk, close runtime, and confirm post-Close call is a no-op.
	if err := statefile.Transact(t.Context(), settingsPath, func(tx *statefile.Transaction) error {
		data, _ := json.Marshal(relayconfig.Eligibility{Disabled: map[string]bool{disabledKey: true}, Paused: true, Notified: false})
		return tx.Put(statefile.CommunityEligibility, data)
	}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	closed = true
	r.RelaySettingsChanged(context.Background())
	if notices.Load() != 1 {
		t.Fatalf("post-Close RelaySettingsChanged delivered notice: %d", notices.Load())
	}
}

func makeCommunityPeerEnvelope(t *testing.T, seedByte byte, identity, host, domain string, now time.Time, lifetime time.Duration, withSTUN bool) json.RawMessage {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seedByte}, ed25519.SeedSize))
	pub := key.Public().(ed25519.PublicKey)
	digest := sha256.Sum256(append([]byte("windshare/node-identity/v1\x00"), pub...))
	enc := base64.RawURLEncoding.EncodeToString
	doc := map[string]any{
		"version":        2,
		"platform":       "native",
		"node_id":        enc(digest[:]),
		"node_key":       enc(pub),
		"relay_endpoint": identity,
		"services":       map[string]bool{"relay": true, "stun": withSTUN},
		"public_relay":   serviceauthority.RelayAnonymous,
		"issued_at":      now.Unix(),
		"expires_at":     now.Add(lifetime).Unix(),
	}
	if withSTUN {
		doc["stun"] = []string{"stun:" + host + ":3478"}
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(key, append([]byte(domain), payload...))
	raw, err := json.Marshal(map[string]any{"version": 2, "descriptor": enc(payload), "signature": enc(sig)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestICEPoolRetainsCommunitySTUNAcrossFrontURLOverridesAndRespectsOptOut(t *testing.T) {
	const officialFront = "https://official.example"
	const overrideFront = "https://override.example"
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	var live json.RawMessage
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/windshare":
			_, _ = w.Write(live)
		case "/v2/ws":
			if r.Header.Get("Origin") != overrideFront {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			_ = ws.CloseNow()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	identity := "wss://peer.example.com:" + port + "/v2/ws"
	gossip := makeCommunityPeerEnvelope(t, 61, identity, "peer.example.com", serviceauthority.GossipDomain, now, 24*time.Hour, true)
	live = makeCommunityPeerEnvelope(t, 61, identity, "peer.example.com", serviceauthority.DescriptorDomain, now, 5*time.Minute, true)

	settingsPath := filepath.Join(t.TempDir(), "config.json")
	store := relayconfig.FileStore{Path: settingsPath}
	if err := store.Save(relayconfig.Config{Profiles: []relayprofile.Candidate{}}); err != nil {
		t.Fatal(err)
	}
	rawPeers, err := json.Marshal([]json.RawMessage{gossip})
	if err != nil {
		t.Fatal(err)
	}
	if err := statefile.Write(t.Context(), filepath.Join(statefile.StateDirectory(settingsPath), "community-peers.json"), rawPeers); err != nil {
		t.Fatal(err)
	}

	r, err := New(nil, func() time.Time { return now }, "", diagnostics.Sink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	r.config = networkconfig.Config{LinkHost: officialFront, CommunitySettingsPath: settingsPath}
	local := netip.MustParseAddr("127.0.0.1")
	r.community = community.New(settingsPath, publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig,
	}, r.now, diagnostics.Sink{})

	sel, err := r.Plan(t.Context(), relayprofile.Plan(nil), overrideFront)
	if err != nil || len(sel.Relays) != 1 || sel.Relays[0].Community == nil {
		t.Fatalf("override Plan = (%+v, %v)", sel, err)
	}
	if err := sel.Relays[0].Community.Check(t.Context()); err != nil {
		t.Fatalf("override Check failed: %v", err)
	}

	selectCommunityFallback := func() []string {
		return icepolicy.SelectAttemptProfile(r.ICEPool(), icepolicy.SelectionRequest{
			NetworkGenerationID: "1",
			WaveID:              "wave",
			Now:                 now,
			IPv4:                true,
			UsedEndpointIDs:     []string{"shipped-google-stun", "public-cloudflare-stun", "public-twilio-stun"},
		}, icepolicy.GenerationFacts{}).URLs()
	}

	// Primary profile still picks reviewed STUN ahead of community STUN.
	primary := icepolicy.SelectAttemptProfile(r.ICEPool(), icepolicy.SelectionRequest{
		NetworkGenerationID: "1", WaveID: "wave", Now: now, IPv4: true,
	}, icepolicy.GenerationFacts{}).URLs()
	if len(primary) != 2 || primary[0] == "stun:127.0.0.1:3478" || primary[1] == "stun:127.0.0.1:3478" {
		t.Fatalf("community STUN displaced reviewed STUN: %v", primary)
	}
	// Fallback wave receives the community STUN verified under --front-url override.
	if got := selectCommunityFallback(); len(got) != 1 || got[0] != "stun:127.0.0.1:3478" {
		t.Fatalf("ICEPool dropped community STUN after --front-url override: %v", got)
	}

	// Disabling community_nodes removes community STUN from ICEPool immediately.
	if _, err := store.Update(t.Context(), func(c *relayconfig.Config) error {
		off := false
		c.CommunityNodes = &off
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := selectCommunityFallback(); len(got) != 0 {
		t.Fatalf("disabled community_nodes still exposed community STUN: %v", got)
	}

	// Re-enabling and planning with the default frontend clears the explicit override.
	if _, err := store.Update(t.Context(), func(c *relayconfig.Config) error {
		on := true
		c.CommunityNodes = &on
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Plan(t.Context(), relayprofile.Plan(nil), ""); err != nil {
		t.Fatalf("default Plan failed: %v", err)
	}
	r.mu.Lock()
	originAfterDefault := r.communityOrigin
	r.mu.Unlock()
	if originAfterDefault != "" {
		t.Fatalf("default Plan retained explicit communityOrigin %q", originAfterDefault)
	}
}

func TestUnavailabilityUsesSavedFrontURLForCustomFrontendBuilds(t *testing.T) {
	const officialFront = "https://official.example"
	const customFront = "https://custom.example"
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	var live json.RawMessage
	var allowCustom atomic.Bool
	allowCustom.Store(true)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/windshare":
			_, _ = w.Write(live)
		case "/v2/ws":
			if r.Header.Get("Origin") == customFront && allowCustom.Load() {
				ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
				if err != nil {
					return
				}
				_ = ws.CloseNow()
				return
			}
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	identity := "wss://custom-peer.example.com:" + port + "/v2/ws"
	gossip := makeCommunityPeerEnvelope(t, 62, identity, "custom-peer.example.com", serviceauthority.GossipDomain, now, 24*time.Hour, false)
	live = makeCommunityPeerEnvelope(t, 62, identity, "custom-peer.example.com", serviceauthority.DescriptorDomain, now, 5*time.Minute, false)

	settingsPath := filepath.Join(t.TempDir(), "config.json")
	store := relayconfig.FileStore{Path: settingsPath}
	if err := store.Save(relayconfig.Config{Profiles: []relayprofile.Candidate{}}); err != nil {
		t.Fatal(err)
	}
	rawPeers, err := json.Marshal([]json.RawMessage{gossip})
	if err != nil {
		t.Fatal(err)
	}
	if err := statefile.Write(t.Context(), filepath.Join(statefile.StateDirectory(settingsPath), "community-peers.json"), rawPeers); err != nil {
		t.Fatal(err)
	}

	r, err := New(nil, func() time.Time { return now }, "", diagnostics.Sink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	// Custom build with no built-in LinkHost or BackupEntry.
	r.config = networkconfig.Config{CommunitySettingsPath: settingsPath}
	local := netip.MustParseAddr("127.0.0.1")
	r.community = community.New(settingsPath, publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig,
	}, r.now, diagnostics.Sink{})

	// Without a saved front_url or built-in frontend, bare build stays Unprovisioned.
	if failure := r.Unavailability(); failure == nil || failure.Reason != networkdirectory.Unprovisioned {
		t.Fatalf("bare custom build Unavailability = %v, want Unprovisioned", failure)
	}

	// Saving settings.FrontURL makes Unavailability see the community pool without creating a lock file.
	if _, err := store.Update(t.Context(), func(c *relayconfig.Config) error {
		c.FrontURL = customFront
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(settingsPath + ".lock"); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if failure := r.Unavailability(); failure != nil {
		t.Fatalf("custom-frontend build Unavailability = %v, want nil", failure)
	}
	if _, err := os.Stat(settingsPath + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Unavailability created lock file: err=%v", err)
	}

	// With a built-in LinkHost that the peer rejects (403), Unavailability still follows settings.FrontURL.
	r.config.LinkHost = officialFront
	if err := r.community.Verify(t.Context(), r.scope(officialFront), identity); err == nil {
		t.Fatal("expected officialFront origin rejection")
	}
	if failure := r.Unavailability(); failure != nil {
		t.Fatalf("Unavailability used rejected LinkHost instead of settings.FrontURL: %v", failure)
	}

	// Conversely, if settings.FrontURL itself is rejected, Unavailability must not falsely succeed via LinkHost.
	allowCustom.Store(false)
	if err := r.community.Verify(t.Context(), r.scope(customFront), identity); err == nil {
		t.Fatal("expected customFront origin rejection")
	}
	if failure := r.Unavailability(); failure == nil || failure.Reason != networkdirectory.Unprovisioned {
		t.Fatalf("Unavailability ignored rejection on settings.FrontURL: %v", failure)
	}
}

func TestCheckFrontendsFailoverRewarmsCommunityEvenAfterDefaultShare(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	var linkHealthy atomic.Bool
	linkHealthy.Store(true)
	linkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if linkHealthy.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer linkServer.Close()
	backupServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backupServer.Close()

	var live json.RawMessage
	var wsHits atomic.Int32
	originSeen := make(chan string, 16)
	peerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/discovery/v1/peers":
			_ = json.NewEncoder(w).Encode([]json.RawMessage{})
		case "/.well-known/windshare":
			_, _ = w.Write(live)
		case "/v2/ws":
			wsHits.Add(1)
			originSeen <- r.Header.Get("Origin")
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			_ = ws.CloseNow()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer peerServer.Close()

	_, port, _ := net.SplitHostPort(strings.TrimPrefix(peerServer.URL, "https://"))
	identity := "wss://failover-peer.example.com:" + port + "/v2/ws"
	gossip := makeCommunityPeerEnvelope(t, 63, identity, "failover-peer.example.com", serviceauthority.GossipDomain, now, 24*time.Hour, false)
	live = makeCommunityPeerEnvelope(t, 63, identity, "failover-peer.example.com", serviceauthority.DescriptorDomain, now, 5*time.Minute, false)

	settingsPath := filepath.Join(t.TempDir(), "config.json")
	store := relayconfig.FileStore{Path: settingsPath}
	if err := store.Save(relayconfig.Config{Profiles: []relayprofile.Candidate{}}); err != nil {
		t.Fatal(err)
	}
	rawPeers, err := json.Marshal([]json.RawMessage{gossip})
	if err != nil {
		t.Fatal(err)
	}
	if err := statefile.Write(t.Context(), filepath.Join(statefile.StateDirectory(settingsPath), "community-peers.json"), rawPeers); err != nil {
		t.Fatal(err)
	}

	r, err := New(nil, func() time.Time { return now }, "", diagnostics.Sink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	r.config = networkconfig.Config{
		LinkHost:              linkServer.URL,
		BackupEntry:           backupServer.URL,
		CommunitySettingsPath: settingsPath,
	}
	local := netip.MustParseAddr("127.0.0.1")
	r.community = community.New(settingsPath, publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: peerServer.Client().Transport.(*http.Transport).TLSClientConfig,
	}, r.now, diagnostics.Sink{})
	r.startCommunity()

	waitVerified := func(want string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case got := <-originSeen:
				if got == want {
					goto checkState
				}
			case <-deadline:
				t.Fatalf("timed out waiting for community warm on origin %q", want)
			}
		}
	checkState:
		scope := r.scope(want)
		for {
			if peers := r.community.Candidates(scope); len(peers) == 1 && peers[0].Verified {
				return
			}
			select {
			case <-deadline:
				t.Fatalf("timed out waiting for verified community candidate on origin %q", want)
			default:
				time.Sleep(time.Millisecond)
			}
		}
	}

	// 1. Initial warm verifies LinkHost; an ordinary default share must not latch communityOrigin.
	waitVerified(linkServer.URL)
	sel1, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil || sel1.Frontend != linkServer.URL || len(sel1.Plan.Network) != 1 || !sel1.Plan.Network[0].Verified {
		t.Fatalf("initial default Plan = (%+v, %v)", sel1, err)
	}
	r.mu.Lock()
	latched := r.communityOrigin
	r.mu.Unlock()
	if latched != "" {
		t.Fatalf("default Plan latched communityOrigin = %q, want empty", latched)
	}

	// 2. Unchanged checkFrontends does not trigger redundant community kicks.
	beforeHits := wsHits.Load()
	r.checkFrontends()
	if len(r.communityKick) != 0 || wsHits.Load() != beforeHits {
		t.Fatalf("unchanged checkFrontends triggered re-warm: kick=%d hits=%d->%d", len(r.communityKick), beforeHits, wsHits.Load())
	}

	// 3. Failing over LinkHost -> BackupEntry kicks and re-warms BackupEntry immediately.
	linkHealthy.Store(false)
	r.checkFrontends()
	waitVerified(backupServer.URL)
	sel2, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil || sel2.Frontend != backupServer.URL || len(sel2.Plan.Network) != 1 || !sel2.Plan.Network[0].Verified {
		t.Fatalf("post-failover Plan = (%+v, %v)", sel2, err)
	}

	// 4. Recovering BackupEntry -> LinkHost kicks and re-warms LinkHost immediately.
	linkHealthy.Store(true)
	r.checkFrontends()
	waitVerified(linkServer.URL)

	// 5. Built-in failover must not clobber an explicit per-share frontend override.
	const explicitFront = "https://explicit.example"
	if _, err := r.Plan(t.Context(), relayprofile.Plan(nil), explicitFront); err != nil {
		t.Fatalf("explicit Plan failed: %v", err)
	}
	waitVerified(explicitFront)
	linkHealthy.Store(false)
	r.checkFrontends()
	r.mu.Lock()
	explicitStillSet := r.communityOrigin
	r.mu.Unlock()
	if explicitStillSet != explicitFront {
		t.Fatalf("built-in failover clobbered explicit communityOrigin = %q, want %q", explicitStillSet, explicitFront)
	}
}

func TestCommunityEligibilityChangedEmitsAcrossEnrollmentAndRemoval(t *testing.T) {
	current := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	var nowUnix atomic.Int64
	nowUnix.Store(current.Unix())
	nowFn := func() time.Time { return time.Unix(nowUnix.Load(), 0).UTC() }

	settingsPath := filepath.Join(t.TempDir(), "config.json")
	store := relayconfig.FileStore{Path: settingsPath}
	stream, err := diagnostics.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	r, err := New(nil, nowFn, "", stream.Sink(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	r.config = networkconfig.Config{CommunitySettingsPath: settingsPath}
	r.startCommunity()

	drainEligibility := func() []diagnostics.Record {
		var out []diagnostics.Record
		for {
			select {
			case rec := <-stream.Records():
				if rec.Name() == "community_eligibility_changed" {
					out = append(out, rec)
				}
			default:
				return out
			}
		}
	}

	// Cold start establishes baseline without emitting a spurious transition.
	if got := drainEligibility(); len(got) != 0 {
		t.Fatalf("unexpected startup community_eligibility_changed events: %v", got)
	}

	// Initial owner enrollment with RelayDisabled transitions eligibility false -> true.
	enrollAt := current.Add(10 * time.Second)
	nowUnix.Store(enrollAt.Unix())
	const ownerKey = "enrolled-disabled-owner-key"
	if err := statefile.Transact(t.Context(), settingsPath, func(tx *statefile.Transaction) error {
		cfg := relayconfig.Config{
			Profiles: []relayprofile.Candidate{
				{PublicURL: "https://owned.example", Role: relayprofile.Owner, AccessKey: "k", Node: relayprofile.NodeIdentity{Platform: "native", NodeKey: ownerKey}},
			},
			ActiveRelays: []string{"https://owned.example"},
		}
		data, err := relayconfig.Encode(cfg)
		if err != nil {
			return err
		}
		if err := tx.Put(statefile.Settings, data); err != nil {
			return err
		}
		_, err = relayconfig.ReconcileEligibility(tx, cfg, map[string]serviceauthority.PublicRelay{ownerKey: serviceauthority.RelayDisabled})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r.RelaySettingsChanged(t.Context())
	events := drainEligibility()
	if len(events) != 1 || !events[0].Payload().Get("paused").Bool() || !events[0].ObservedAt().Equal(enrollAt) {
		t.Fatalf("enrollment events = %+v, want 1 paused=true at %v", events, enrollAt)
	}

	// Unchanged settings notifications must not emit duplicates.
	r.RelaySettingsChanged(t.Context())
	if got := drainEligibility(); len(got) != 0 {
		t.Fatalf("unchanged RelaySettingsChanged emitted duplicate events: %v", got)
	}

	// Removing the disabled owned relay transitions eligibility true -> false.
	removeAt := current.Add(20 * time.Second)
	nowUnix.Store(removeAt.Unix())
	if _, err := store.Update(t.Context(), func(c *relayconfig.Config) error {
		return c.RemoveRelay("https://owned.example")
	}); err != nil {
		t.Fatal(err)
	}
	r.RelaySettingsChanged(t.Context())
	events = drainEligibility()
	if len(events) != 1 || events[0].Payload().Get("paused").Bool() || !events[0].ObservedAt().Equal(removeAt) {
		t.Fatalf("removal events = %+v, want 1 paused=false at %v", events, removeAt)
	}
}
