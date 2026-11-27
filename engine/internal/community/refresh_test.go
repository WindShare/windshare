package community

import (
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
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/connectivity/publicdial"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/serviceauthority"
)

func envelope(t *testing.T, endpoint, domain string, now time.Time, lifetime time.Duration) json.RawMessage {
	t.Helper()
	return envelopeWithRelay(t, endpoint, domain, now, lifetime, serviceauthority.RelayAnonymous)
}

func envelopeWithRelay(t *testing.T, endpoint, domain string, now time.Time, lifetime time.Duration, state serviceauthority.PublicRelay) json.RawMessage {
	t.Helper()
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	public := key.Public().(ed25519.PublicKey)
	digest := sha256.Sum256(append([]byte("windshare/node-identity/v1\x00"), public...))
	encode := base64.RawURLEncoding.EncodeToString
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"version": 2, "platform": "native", "node_id": encode(digest[:]), "node_key": encode(public), "relay_endpoint": endpoint,
		"services": map[string]bool{"relay": true, "stun": true}, "stun": []string{"stun:" + u.Hostname() + ":3478", "stun:elsewhere.example:3478"},
		"public_relay": state, "issued_at": now.Unix(), "expires_at": now.Add(lifetime).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(key, append([]byte(domain), payload...))
	raw, err := json.Marshal(map[string]any{"version": 2, "descriptor": encode(payload), "signature": encode(signature)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestGossipCannotReplayAsLive(t *testing.T) {
	now := time.Now()
	const endpoint = "wss://relay.example/v2/ws"
	live := envelope(t, endpoint, serviceauthority.DescriptorDomain, now, 5*time.Minute)
	// Identical lifetimes isolate the domain check from the lifetime check.
	short := envelope(t, endpoint, serviceauthority.GossipDomain, now, 5*time.Minute)
	if _, err := serviceauthority.InspectDescriptor(short, now); !errors.Is(err, serviceauthority.ErrSignature) {
		t.Fatal(err)
	}
	if _, err := serviceauthority.InspectGossip(live, now); !errors.Is(err, serviceauthority.ErrSignature) {
		t.Fatal(err)
	}
	long := envelope(t, endpoint, serviceauthority.GossipDomain, now, 24*time.Hour)
	if _, err := serviceauthority.InspectGossip(long, now.Add(23*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := serviceauthority.InspectGossip(long, now.Add(24*time.Hour)); !errors.Is(err, serviceauthority.ErrExpired) {
		t.Fatal(err)
	}
	oversized := envelope(t, endpoint, serviceauthority.GossipDomain, now, 25*time.Hour)
	if _, err := serviceauthority.InspectGossip(oversized, now); !errors.Is(err, serviceauthority.ErrFormat) {
		t.Fatal(err)
	}
}

func TestPullWarmRestartAndOriginPolicy(t *testing.T) {
	now := time.Now()
	const front = "https://share.example"
	var pulls, upgrades atomic.Int32
	var gossip, live json.RawMessage
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case exchangePath:
			pulls.Add(1)
			if r.Method != http.MethodGet {
				t.Errorf("native client attempted %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode([]json.RawMessage{gossip})
		case "/.well-known/windshare":
			_, _ = w.Write(live)
		case "/v2/ws":
			if r.Header.Get("Origin") != front || r.Header.Get("Authorization") != "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"share.example"}})
			if err != nil {
				return
			}
			upgrades.Add(1)
			_ = ws.CloseNow()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	base := "https://example.com:" + port
	identity := "wss://example.com:" + port + "/v2/ws"
	gossip = envelope(t, identity, serviceauthority.GossipDomain, now, 24*time.Hour)
	live = envelope(t, identity, serviceauthority.DescriptorDomain, now, 5*time.Minute)
	local := netip.MustParseAddr("127.0.0.1")
	path := filepath.Join(t.TempDir(), "config.json")
	open := func() *Pool {
		p := New(path, publicdial.Policy{Permit: func(a netip.Addr) bool { return a == local }, Lookup: func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil }},
			func() time.Time { return now }, diagnostics.Sink{})
		p.http.Transport.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		return p
	}
	scope := Scope{Origin: front, Generation: 1}
	pool := open()
	pool.Refresh(t.Context(), scope, []string{base})
	peers := pool.Candidates(scope)
	if len(peers) != 1 || !peers[0].Verified || pulls.Load() != 1 || upgrades.Load() != 1 {
		t.Fatalf("peers=%v pulls=%d upgrades=%d", peers, pulls.Load(), upgrades.Load())
	}
	stun := pool.STUN(scope)
	if len(stun) != 1 || stun[0].URL != "stun:127.0.0.1:3478" || stun[0].FailureDomain != "127.0.0.0/24" {
		t.Fatal(stun)
	}
	other := Scope{Origin: "https://other.example", Generation: 1}
	if err := pool.Verify(t.Context(), other, identity); err == nil || pool.Candidates(other)[0].Verified {
		t.Fatal("a rejected browser origin was admitted", err)
	}
	pool.Close()
	restored := open()
	defer restored.Close()
	peers = restored.Candidates(scope)
	if len(peers) != 1 || peers[0].Verified {
		t.Fatal("restart must retain the signed record and recheck reachability", peers)
	}
	restored.Refresh(t.Context(), scope, nil)
	if !restored.Candidates(scope)[0].Verified || pulls.Load() != 2 {
		t.Fatal("failed to recover from cache without seeds")
	}
}

func TestDisabledContributionExcludedFromPoolAndLiveSTUN(t *testing.T) {
	now := time.Now()
	const front = "https://share.example"
	var live json.RawMessage
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/windshare":
			_, _ = w.Write(live)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	identity := "wss://example.com:" + port + "/v2/ws"
	local := netip.MustParseAddr("127.0.0.1")
	pool := New("", publicdial.Policy{
		Permit: func(a netip.Addr) bool { return a == local },
		Lookup: func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
	}, func() time.Time { return now }, diagnostics.Sink{})
	defer pool.Close()
	pool.http.Transport.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()

	// Disabled gossip descriptor with STUN enabled is rejected immediately.
	disabledGossip := envelopeWithRelay(t, identity, serviceauthority.GossipDomain, now, 24*time.Hour, serviceauthority.RelayDisabled)
	if pool.accept(disabledGossip, "seed") || len(pool.records()) != 0 {
		t.Fatal("disabled descriptor must not enter community pool")
	}

	// If a previously cached STUN-capable node turns off contribution, live verification refuses it.
	stunOnlyGossip := envelopeWithRelay(t, identity, serviceauthority.GossipDomain, now, 24*time.Hour, serviceauthority.RelayUnavailable)
	if !pool.accept(stunOnlyGossip, "seed") {
		t.Fatal("expected STUN-only community peer to be accepted")
	}
	live = envelopeWithRelay(t, identity, serviceauthority.DescriptorDomain, now, 5*time.Minute, serviceauthority.RelayDisabled)
	scope := Scope{Origin: front, Generation: 1}
	if err := pool.check(t.Context(), scope, identity, 0); err == nil {
		t.Fatal("live disabled node must not be admitted for STUN")
	}
	if stun := pool.STUN(scope); len(stun) != 0 {
		t.Fatalf("expected no STUN endpoints for disabled node, got %v", stun)
	}

	// A newer same-key Disabled gossip record evicts the stale cached entry.
	newerDisabled := envelopeWithRelay(t, identity, serviceauthority.GossipDomain, now.Add(time.Minute), 24*time.Hour, serviceauthority.RelayDisabled)
	if pool.accept(newerDisabled, "seed") || len(pool.records()) != 0 {
		t.Fatal("newer disabled record should evict prior cached entry")
	}
}
