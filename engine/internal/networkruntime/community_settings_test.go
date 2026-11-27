package networkruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/connectivity/publicdial"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/community"
	"github.com/windshare/windshare/engine/internal/statefile"
	"github.com/windshare/windshare/engine/networkconfig"
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
