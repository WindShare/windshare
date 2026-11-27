package networkruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/connectivity/publicdial"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/networkendpoint"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/community"
	"github.com/windshare/windshare/engine/internal/statefile"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

func ownerPublicKey(seedByte byte) string {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seedByte}, ed25519.SeedSize))
	pub := key.Public().(ed25519.PublicKey)
	return base64.RawURLEncoding.EncodeToString(pub)
}

func waitForVerifiedOrigin(t *testing.T, r *Runtime, want string) {
	t.Helper()
	scope := r.scope(want)
	deadline := time.After(5 * time.Second)
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

func TestOwnedRelayObservationRunsConcurrentlyWithoutBlockingCommunityRefresh(t *testing.T) {
	const (
		initialFront  = "https://initial.example"
		updatedFront  = "https://updated.example"
		overrideFront = "https://override.example"
	)
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	ownerAEntered, ownerBEntered := make(chan struct{}), make(chan struct{})
	releaseOwners := make(chan struct{})
	var ownerAHits, ownerBHits atomic.Int32
	newBlockingOwner := func(entered chan struct{}, hits *atomic.Int32) *httptest.Server {
		var once sync.Once
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			once.Do(func() { close(entered) })
			select {
			case <-r.Context().Done():
			case <-releaseOwners:
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	ownerA := newBlockingOwner(ownerAEntered, &ownerAHits)
	ownerB := newBlockingOwner(ownerBEntered, &ownerBHits)

	var live json.RawMessage
	peerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/discovery/v1/peers":
			_ = json.NewEncoder(w).Encode([]json.RawMessage{})
		case "/.well-known/windshare":
			_, _ = w.Write(live)
		case "/v2/ws":
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
	identity := "wss://concurrent-peer.example.com:" + port + "/v2/ws"
	gossip := makeCommunityPeerEnvelope(t, 71, identity, "concurrent-peer.example.com", serviceauthority.GossipDomain, now, 24*time.Hour, false)
	live = makeCommunityPeerEnvelope(t, 71, identity, "concurrent-peer.example.com", serviceauthority.DescriptorDomain, now, 5*time.Minute, false)

	settingsPath := filepath.Join(t.TempDir(), "config.json")
	store := relayconfig.FileStore{Path: settingsPath}
	if err := store.Save(relayconfig.Config{
		Profiles: []relayprofile.Candidate{
			{PublicURL: ownerA.URL, Role: relayprofile.Owner, AccessKey: "k1", Node: relayprofile.NodeIdentity{Platform: "native", NodeKey: ownerPublicKey(1)}},
			{PublicURL: ownerB.URL, Role: relayprofile.Owner, AccessKey: "k2", Node: relayprofile.NodeIdentity{Platform: "native", NodeKey: ownerPublicKey(2)}},
		},
	}); err != nil {
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
	closed := false
	t.Cleanup(func() {
		if !closed {
			close(releaseOwners)
			r.Close()
		}
	})
	r.config = networkconfig.Config{
		LinkHost:              initialFront,
		CommunitySettingsPath: settingsPath,
	}
	local := netip.MustParseAddr("127.0.0.1")
	r.community = community.New(settingsPath, publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: peerServer.Client().Transport.(*http.Transport).TLSClientConfig,
	}, r.now, diagnostics.Sink{})
	r.startCommunity()

	// Both owned relays must be probed concurrently before either completes.
	for name, ch := range map[string]chan struct{}{"ownerA": ownerAEntered, "ownerB": ownerBEntered} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for concurrent probe of %s", name)
		}
	}

	// Persisted eligibility allows community use, so startup Refresh and
	// settings/origin updates must warm immediately while owned probes stay blocked.
	waitForVerifiedOrigin(t, r, initialFront)

	if _, err := store.Update(t.Context(), func(c *relayconfig.Config) error {
		c.FrontURL = updatedFront
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.RelaySettingsChanged(t.Context())
	waitForVerifiedOrigin(t, r, updatedFront)

	r.noteOrigin(overrideFront)
	waitForVerifiedOrigin(t, r, overrideFront)
	if ownerAHits.Load() != 1 || ownerBHits.Load() != 1 {
		t.Fatalf("unexpected concurrent owner probes while first batch still running: a=%d b=%d", ownerAHits.Load(), ownerBHits.Load())
	}

	// Closing Runtime must cancel in-flight owner probes promptly without waiting for ownerObserveTimeout.
	closeStart := time.Now()
	r.Close()
	closed = true
	if elapsed := time.Since(closeStart); elapsed > 2*time.Second {
		t.Fatalf("Runtime.Close waited on blocked owner probes: %v", elapsed)
	}
}

func TestPausedEligibilityWaitsForConcurrentOwnerObservationBeforeRefreshing(t *testing.T) {
	const front = "https://resumed.example"
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	ownerAEntered, ownerBEntered := make(chan struct{}), make(chan struct{})
	releaseOwnerA := make(chan struct{})
	var onceA, onceB sync.Once
	ownerA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		onceA.Do(func() { close(ownerAEntered) })
		select {
		case <-r.Context().Done():
			return
		case <-releaseOwnerA:
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ownerA.Close()

	var ownerBLive json.RawMessage
	ownerB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		onceB.Do(func() { close(ownerBEntered) })
		if r.URL.Path != "/.well-known/windshare" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(ownerBLive)
	}))
	defer ownerB.Close()

	epB, err := networkendpoint.NormalizeRelay(ownerB.URL)
	if err != nil {
		t.Fatal(err)
	}
	ownerBLive = makeCommunityPeerEnvelope(t, 12, epB.IdentityURL, "127.0.0.1", serviceauthority.DescriptorDomain, now, 5*time.Minute, false)

	var peerLive json.RawMessage
	var peerHits atomic.Int32
	peerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerHits.Add(1)
		switch r.URL.Path {
		case "/discovery/v1/peers":
			_ = json.NewEncoder(w).Encode([]json.RawMessage{})
		case "/.well-known/windshare":
			_, _ = w.Write(peerLive)
		case "/v2/ws":
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
	identity := "wss://paused-peer.example.com:" + port + "/v2/ws"
	gossip := makeCommunityPeerEnvelope(t, 72, identity, "paused-peer.example.com", serviceauthority.GossipDomain, now, 24*time.Hour, false)
	peerLive = makeCommunityPeerEnvelope(t, 72, identity, "paused-peer.example.com", serviceauthority.DescriptorDomain, now, 5*time.Minute, false)

	keyA, keyB := ownerPublicKey(11), ownerPublicKey(12)
	settingsPath := filepath.Join(t.TempDir(), "config.json")
	if err := statefile.Transact(t.Context(), settingsPath, func(tx *statefile.Transaction) error {
		cfg := relayconfig.Config{
			Profiles: []relayprofile.Candidate{
				{PublicURL: ownerA.URL, Role: relayprofile.Owner, AccessKey: "k1", Node: relayprofile.NodeIdentity{Platform: "native", NodeKey: keyA}},
				{PublicURL: ownerB.URL, Role: relayprofile.Owner, AccessKey: "k2", Node: relayprofile.NodeIdentity{Platform: "native", NodeKey: keyB}},
			},
		}
		data, err := relayconfig.Encode(cfg)
		if err != nil {
			return err
		}
		if err := tx.Put(statefile.Settings, data); err != nil {
			return err
		}
		elig, _ := json.Marshal(relayconfig.Eligibility{
			Disabled: map[string]bool{keyA: true, keyB: true},
			Paused:   true,
			Notified: true,
		})
		return tx.Put(statefile.CommunityEligibility, elig)
	}); err != nil {
		t.Fatal(err)
	}
	rawPeers, err := json.Marshal([]json.RawMessage{gossip})
	if err != nil {
		t.Fatal(err)
	}
	if err := statefile.Write(t.Context(), filepath.Join(statefile.StateDirectory(settingsPath), "community-peers.json"), rawPeers); err != nil {
		t.Fatal(err)
	}

	stream, err := diagnostics.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	r, err := New(nil, func() time.Time { return now }, "", stream.Sink(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	r.config = networkconfig.Config{
		LinkHost:              front,
		CommunitySettingsPath: settingsPath,
	}
	local := netip.MustParseAddr("127.0.0.1")
	r.community = community.New(settingsPath, publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: peerServer.Client().Transport.(*http.Transport).TLSClientConfig,
	}, r.now, stream.Sink())
	r.startCommunity()

	// Both owned relays are probed concurrently, while paused persisted
	// eligibility must prevent any speculative community Refresh.
	for name, ch := range map[string]chan struct{}{"ownerA": ownerAEntered, "ownerB": ownerBEntered} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for concurrent probe of %s", name)
		}
	}
	if got := peerHits.Load(); got != 0 {
		t.Fatalf("paused eligibility triggered speculative community Refresh (%d hits)", got)
	}

	// Releasing offline ownerA lets the batch finish with ownerB's contributing
	// state, resuming eligibility and immediately kicking community Refresh.
	close(releaseOwnerA)
	waitForVerifiedOrigin(t, r, front)

	resumedEvent := false
	for !resumedEvent {
		select {
		case rec := <-stream.Records():
			if rec.Name() == "community_eligibility_changed" && !rec.Payload().Get("paused").Bool() {
				if !rec.ObservedAt().Equal(now) {
					t.Fatalf("community_eligibility_changed timestamp = %v, want %v", rec.ObservedAt(), now)
				}
				resumedEvent = true
			}
		default:
			t.Fatal("missing community_eligibility_changed(paused=false) diagnostic")
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

func signOwnerDescriptor(t *testing.T, seedByte byte, domain, identity string, state serviceauthority.PublicRelay, authority map[string]any, now time.Time) []byte {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seedByte}, ed25519.SeedSize))
	pub := priv.Public().(ed25519.PublicKey)
	digest := sha256.Sum256(append([]byte("windshare/node-identity/v1\x00"), pub...))
	enc := base64.RawURLEncoding.EncodeToString
	lifetime := 5 * time.Minute
	if domain == serviceauthority.GossipDomain {
		lifetime = 24 * time.Hour
	}
	payload, err := json.Marshal(map[string]any{
		"version":        2,
		"platform":       "native",
		"node_id":        enc(digest[:]),
		"node_key":       enc(pub),
		"relay_endpoint": identity,
		"services":       map[string]bool{"relay": true, "stun": false},
		"public_relay":   state,
		"authority":      authority,
		"issued_at":      now.Unix(),
		"expires_at":     now.Add(lifetime).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, append([]byte(domain), payload...))
	raw, err := json.Marshal(map[string]any{
		"version":    2,
		"descriptor": enc(payload),
		"signature":  enc(sig),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestObserveOwnerValidatesHTTPSRedirectsKeysEndpointsAndAuthority(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	const (
		publicURL = "https://owner.example/mount"
		identity  = "wss://owner.example/mount/v2/ws"
		wantURL   = "https://owner.example/.well-known/windshare"
	)
	profile := relayprofile.Candidate{
		PublicURL: publicURL,
		Role:      relayprofile.Owner,
		AccessKey: "owner-key",
		Node:      relayprofile.NodeIdentity{Platform: "native", NodeKey: ownerPublicKey(31)},
	}

	var calls atomic.Int32
	var seenURL string
	var status int
	var headers http.Header
	var body []byte

	r, err := New(nil, func() time.Time { return now }, "", diagnostics.Sink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	r.httpTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		seenURL = req.URL.String()
		return &http.Response{
			StatusCode: status,
			Header:     headers,
			Body:       io.NopCloser(bytes.NewReader(body)),
		}, nil
	})

	// Valid HTTPS self-hosted descriptors map wss -> https and report exact PublicRelay state.
	for _, want := range []serviceauthority.PublicRelay{serviceauthority.RelayDisabled, serviceauthority.RelayAnonymous} {
		status, headers = http.StatusOK, make(http.Header)
		body = signOwnerDescriptor(t, 31, serviceauthority.DescriptorDomain, identity, want, nil, now)
		got, ok := r.observeOwner(profile)
		if !ok || got != want || seenURL != wantURL {
			t.Fatalf("valid observeOwner(%v) = (%v, %v, url=%q), want (%v, true, %q)", want, got, ok, seenURL, want, wantURL)
		}
	}

	// Redirects must never be followed.
	calls.Store(0)
	status = http.StatusFound
	headers = http.Header{"Location": []string{"https://redirected.example/.well-known/windshare"}}
	body = nil
	if _, ok := r.observeOwner(profile); ok || calls.Load() != 1 {
		t.Fatalf("redirect observeOwner: ok=%v calls=%d, want (false, 1)", ok, calls.Load())
	}

	// Non-200, oversized body, wrong key, gossip domain, endpoint mismatch, and official authority fail closed.
	authority := map[string]any{"url": "https://api.example", "issuer": ownerPublicKey(99)}
	rejected := []struct {
		name   string
		status int
		body   []byte
	}{
		{name: "non-200", status: http.StatusServiceUnavailable, body: nil},
		{name: "oversized body", status: http.StatusOK, body: bytes.Repeat([]byte("x"), serviceauthority.MaximumDescriptorBytes+1)},
		{name: "wrong signing key", status: http.StatusOK, body: signOwnerDescriptor(t, 32, serviceauthority.DescriptorDomain, identity, serviceauthority.RelayAnonymous, nil, now)},
		{name: "gossip domain", status: http.StatusOK, body: signOwnerDescriptor(t, 31, serviceauthority.GossipDomain, identity, serviceauthority.RelayAnonymous, nil, now)},
		{name: "endpoint mismatch", status: http.StatusOK, body: signOwnerDescriptor(t, 31, serviceauthority.DescriptorDomain, "wss://other.example/v2/ws", serviceauthority.RelayAnonymous, nil, now)},
		{name: "authority-managed node", status: http.StatusOK, body: signOwnerDescriptor(t, 31, serviceauthority.DescriptorDomain, identity, serviceauthority.RelayAnonymous, authority, now)},
	}
	for _, tc := range rejected {
		status, headers, body = tc.status, make(http.Header), tc.body
		if got, ok := r.observeOwner(profile); ok {
			t.Errorf("%s: observeOwner accepted invalid response (state=%v)", tc.name, got)
		}
	}
}
