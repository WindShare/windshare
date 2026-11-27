package community

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
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
	return envelopeWithSeed(t, 0, endpoint, domain, now, lifetime, state)
}

func envelopeWithSeed(t *testing.T, seedByte byte, endpoint, domain string, now time.Time, lifetime time.Duration, state serviceauthority.PublicRelay) json.RawMessage {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seedByte}, ed25519.SeedSize))
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
		return New(path, publicdial.Policy{
			Permit:          func(a netip.Addr) bool { return a == local },
			Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
			TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig,
		}, func() time.Time { return now }, diagnostics.Sink{})
	}
	scope := Scope{Origin: front, Generation: 1}
	pool := open()
	pool.Refresh(t.Context(), scope, Seeds{Active: []string{base}})
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
	restored.Refresh(t.Context(), scope, Seeds{})
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
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig,
	}, func() time.Time { return now }, diagnostics.Sink{})
	defer pool.Close()

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

func TestSubpathMountedCommunityRelayVerifiesAtOriginRootAndMountWebSocket(t *testing.T) {
	now := time.Now()
	const front = "https://share.example"
	var gossip, live json.RawMessage
	var upgrades atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case exchangePath:
			_ = json.NewEncoder(w).Encode([]json.RawMessage{gossip})
		case "/.well-known/windshare":
			_, _ = w.Write(live)
		case "/mount/v2/ws":
			if r.Header.Get("Origin") != front {
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
	seed := "https://example.com:" + port
	identity := "wss://example.com:" + port + "/mount/v2/ws"
	gossip = envelope(t, identity, serviceauthority.GossipDomain, now, 24*time.Hour)
	live = envelope(t, identity, serviceauthority.DescriptorDomain, now, 5*time.Minute)
	local := netip.MustParseAddr("127.0.0.1")
	pool := New("", publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig,
	}, func() time.Time { return now }, diagnostics.Sink{})
	defer pool.Close()

	scope := Scope{Origin: front, Generation: 1}
	pool.Refresh(t.Context(), scope, Seeds{Active: []string{seed}})
	peers := pool.Candidates(scope)
	if len(peers) != 1 || !peers[0].Verified || peers[0].URL != seed+"/mount" || upgrades.Load() != 1 {
		t.Fatalf("subpath relay failed discovery/verification: peers=%v upgrades=%d", peers, upgrades.Load())
	}
}

func testPolicy(server *httptest.Server) publicdial.Policy {
	local := netip.MustParseAddr("127.0.0.1")
	return publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig,
	}
}

func TestCancelledCallerDoesNotAbortSharedRefresh(t *testing.T) {
	now := time.Now()
	const front = "https://share.example"
	var gossip, live json.RawMessage
	var pulls atomic.Int32
	pullEntered, releasePull := make(chan struct{}), make(chan struct{})
	var pullOnce sync.Once
	var pullAborted atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case exchangePath:
			pulls.Add(1)
			pullOnce.Do(func() { close(pullEntered) })
			select {
			case <-r.Context().Done():
				pullAborted.Store(true)
				return
			case <-releasePull:
			}
			_ = json.NewEncoder(w).Encode([]json.RawMessage{gossip})
		case "/.well-known/windshare":
			_, _ = w.Write(live)
		case "/v2/ws":
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"share.example"}})
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
	seed := "https://example.com:" + port
	identity := "wss://example.com:" + port + "/v2/ws"
	gossip = envelope(t, identity, serviceauthority.GossipDomain, now, 24*time.Hour)
	live = envelope(t, identity, serviceauthority.DescriptorDomain, now, 5*time.Minute)

	path := filepath.Join(t.TempDir(), "config.json")
	pool := New(path, testPolicy(server), func() time.Time { return now }, diagnostics.Sink{})
	defer pool.Close()

	scope := Scope{Origin: front, Generation: 1}
	ctx1, cancel1 := context.WithCancel(t.Context())
	done1 := make(chan struct{})
	go func() {
		pool.Refresh(ctx1, scope, Seeds{Active: []string{seed}})
		close(done1)
	}()
	<-pullEntered

	// Coalesce a second caller before cancelling the first caller.
	done2 := make(chan struct{})
	go func() {
		pool.Refresh(t.Context(), scope, Seeds{Active: []string{seed}})
		close(done2)
	}()
	cancel1()
	<-done1

	// Releasing the seed pull after caller 1 exited must still complete the
	// shared refresh, write the cache file, and warm the peer for caller 2.
	close(releasePull)
	<-done2

	if pullAborted.Load() {
		t.Fatal("cancelling caller 1 aborted the shared seed pull")
	}
	if got := pulls.Load(); got != 1 {
		t.Fatalf("expected coalesced callers to share 1 seed pull, got %d", got)
	}
	peers := pool.Candidates(scope)
	if len(peers) != 1 || !peers[0].Verified {
		t.Fatalf("expected coalesced refresh to populate and verify peer, got %+v", peers)
	}
	if _, err := os.Stat(pool.path); err != nil {
		t.Fatalf("expected community cache file to be persisted: %v", err)
	}
}

func TestCancelledCallerDoesNotAbortSharedVerifyOrQueuedProbe(t *testing.T) {
	now := time.Now()
	const front = "https://share.example"
	var live json.RawMessage
	var probes atomic.Int32
	probeEntered, releaseProbe := make(chan struct{}), make(chan struct{})
	var probeOnce sync.Once
	var probeAborted atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/windshare":
			probes.Add(1)
			probeOnce.Do(func() { close(probeEntered) })
			select {
			case <-r.Context().Done():
				probeAborted.Store(true)
				return
			case <-releaseProbe:
			}
			_, _ = w.Write(live)
		case "/v2/ws":
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"share.example"}})
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
	gossip := envelope(t, identity, serviceauthority.GossipDomain, now, 24*time.Hour)
	live = envelope(t, identity, serviceauthority.DescriptorDomain, now, 5*time.Minute)

	pool := New("", testPolicy(server), func() time.Time { return now }, diagnostics.Sink{})
	defer pool.Close()
	if !pool.accept(gossip, "seed") {
		t.Fatal("expected peer to be accepted")
	}

	scope := Scope{Origin: front, Generation: 1}
	ctx1, cancel1 := context.WithCancel(t.Context())
	err1 := make(chan error, 1)
	go func() { err1 <- pool.Verify(ctx1, scope, identity) }()
	<-probeEntered

	cancel1()
	if err := <-err1; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller 1 error = %v, want context.Canceled", err)
	}

	// A second live caller joining the still-running probe must succeed.
	err2 := make(chan error, 1)
	go func() { err2 <- pool.Verify(t.Context(), scope, identity) }()
	close(releaseProbe)
	if err := <-err2; err != nil {
		t.Fatalf("coalesced caller 2 failed: %v", err)
	}
	if probeAborted.Load() {
		t.Fatal("cancelling caller 1 aborted the shared probe")
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("expected coalesced callers to share 1 probe, got %d", got)
	}
	if peers := pool.Candidates(scope); len(peers) != 1 || !peers[0].Verified {
		t.Fatalf("expected peer to be verified after shared probe, got %+v", peers)
	}

	// Verify that cancelling caller 1 while queued behind saturated probe slots
	// also leaves the shared probe intact for caller 2.
	releaseBlockers := make(chan struct{})
	enteredBlockers := make(chan struct{}, fanout)
	newBlocker := func() *httptest.Server {
		return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			enteredBlockers <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-releaseBlockers:
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
	}
	blockerA, blockerB := newBlocker(), newBlocker()
	defer blockerA.Close()
	defer blockerB.Close()

	var liveC json.RawMessage
	serverC := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/windshare":
			_, _ = w.Write(liveC)
		case "/v2/ws":
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"share.example"}})
			if err != nil {
				return
			}
			_ = ws.CloseNow()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer serverC.Close()

	_, portA, _ := net.SplitHostPort(strings.TrimPrefix(blockerA.URL, "https://"))
	_, portB, _ := net.SplitHostPort(strings.TrimPrefix(blockerB.URL, "https://"))
	_, portC, _ := net.SplitHostPort(strings.TrimPrefix(serverC.URL, "https://"))
	idA := "wss://a.example:" + portA + "/v2/ws"
	idB := "wss://b.example:" + portB + "/v2/ws"
	idC := "wss://c.example:" + portC + "/v2/ws"
	liveC = envelope(t, idC, serviceauthority.DescriptorDomain, now, 5*time.Minute)

	tlsConfig := serverC.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConfig.InsecureSkipVerify = true
	local := netip.MustParseAddr("127.0.0.1")
	queuedPool := New("", publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: tlsConfig,
	}, func() time.Time { return now }, diagnostics.Sink{})
	defer queuedPool.Close()

	for _, id := range []string{idA, idB, idC} {
		if !queuedPool.accept(envelope(t, id, serviceauthority.GossipDomain, now, 24*time.Hour), "seed") {
			t.Fatalf("failed to accept %s", id)
		}
	}

	var blockerWG sync.WaitGroup
	for _, id := range []string{idA, idB} {
		blockerWG.Go(func() { _ = queuedPool.Verify(t.Context(), scope, id) })
	}
	for range fanout {
		<-enteredBlockers
	}

	qCtx1, qCancel1 := context.WithCancel(t.Context())
	qErr1 := make(chan error, 1)
	go func() { qErr1 <- queuedPool.Verify(qCtx1, scope, idC) }()
	for {
		queuedPool.mu.Lock()
		queued := queuedPool.inflight[probeKey{scope, idC}] != nil
		queuedPool.mu.Unlock()
		if queued {
			break
		}
		runtime.Gosched()
	}

	qErr2 := make(chan error, 1)
	go func() { qErr2 <- queuedPool.Verify(t.Context(), scope, idC) }()
	qCancel1()
	if err := <-qErr1; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued caller 1 error = %v, want context.Canceled", err)
	}
	close(releaseBlockers)
	if err := <-qErr2; err != nil {
		t.Fatalf("queued caller 2 failed after caller 1 cancelled: %v", err)
	}
	blockerWG.Wait()
}

func TestRefreshRewarmsWhenNewerScopeArrivesMidCycle(t *testing.T) {
	now := time.Now()
	const frontA, frontB = "https://a.example", "https://b.example"
	var gossip, live json.RawMessage
	warmEntered, releaseWarm := make(chan struct{}), make(chan struct{})
	var warmOnce sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case exchangePath:
			_ = json.NewEncoder(w).Encode([]json.RawMessage{gossip})
		case "/.well-known/windshare":
			warmOnce.Do(func() {
				close(warmEntered)
				<-releaseWarm
			})
			_, _ = w.Write(live)
		case "/v2/ws":
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"a.example", "b.example"}})
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
	seed := "https://example.com:" + port
	identity := "wss://example.com:" + port + "/v2/ws"
	gossip = envelope(t, identity, serviceauthority.GossipDomain, now, 24*time.Hour)
	live = envelope(t, identity, serviceauthority.DescriptorDomain, now, 5*time.Minute)

	pool := New("", testPolicy(server), func() time.Time { return now }, diagnostics.Sink{})
	defer pool.Close()

	scope1 := Scope{Origin: frontA, Generation: 1}
	scope2 := Scope{Origin: frontB, Generation: 2}
	staleScope := Scope{Origin: frontA, Generation: 1}

	ctx1, cancel1 := context.WithCancel(t.Context())
	done1 := make(chan struct{})
	go func() {
		pool.Refresh(ctx1, scope1, Seeds{Active: []string{seed}})
		close(done1)
	}()
	<-warmEntered

	done2 := make(chan struct{})
	go func() {
		pool.Refresh(t.Context(), scope2, Seeds{Active: []string{seed}})
		close(done2)
	}()
	for {
		pool.mu.Lock()
		advanced := pool.cycle != nil && pool.cycle.scope == scope2
		pool.mu.Unlock()
		if advanced {
			break
		}
		runtime.Gosched()
	}

	// A delayed caller with an older generation must not roll back scope2.
	staleCtx, cancelStale := context.WithCancel(t.Context())
	doneStale := make(chan struct{})
	go func() {
		pool.Refresh(staleCtx, staleScope, Seeds{Active: []string{seed}})
		close(doneStale)
	}()
	cancelStale()
	<-doneStale

	cancel1()
	<-done1
	close(releaseWarm)
	<-done2

	peers := pool.Candidates(scope2)
	if len(peers) != 1 || !peers[0].Verified {
		t.Fatalf("expected peer to be re-warmed for newer scope %+v, got %+v", scope2, peers)
	}
}

func TestPreCancelledContextAndPoolCloseJoinDetachedWork(t *testing.T) {
	now := time.Now()
	const front = "https://share.example"
	var requests atomic.Int32
	pullEntered, serverCancelled := make(chan struct{}), make(chan struct{})
	var enterOnce, cancelOnce sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		enterOnce.Do(func() { close(pullEntered) })
		<-r.Context().Done()
		cancelOnce.Do(func() { close(serverCancelled) })
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	seed := "https://example.com:" + port
	identity := "wss://example.com:" + port + "/v2/ws"

	pool := New("", testPolicy(server), func() time.Time { return now }, diagnostics.Sink{})
	if !pool.accept(envelope(t, identity, serviceauthority.GossipDomain, now, 24*time.Hour), "seed") {
		t.Fatal("expected peer to be accepted")
	}

	scope := Scope{Origin: front, Generation: 1}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	pool.Refresh(cancelled, scope, Seeds{Active: []string{seed}})
	if err := pool.Verify(cancelled, scope, identity); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled Verify error = %v, want context.Canceled", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("pre-cancelled calls triggered %d HTTP requests, want 0", got)
	}

	callerCtx, callerCancel := context.WithCancel(t.Context())
	callerDone := make(chan struct{})
	go func() {
		pool.Refresh(callerCtx, scope, Seeds{Active: []string{seed}})
		close(callerDone)
	}()
	<-pullEntered
	callerCancel()
	<-callerDone

	// Pool.Close must cancel and join the detached background work promptly.
	pool.Close()
	<-serverCancelled
	if err := pool.Verify(t.Context(), scope, identity); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-Close Verify error = %v, want context.Canceled", err)
	}
}

type fakeCommunityNode struct {
	server   *httptest.Server
	base     string
	identity string
	gossip   json.RawMessage
	live     json.RawMessage
	pulls    atomic.Int32
	probes   atomic.Int32
	alive    atomic.Bool
	sample   func() []json.RawMessage
}

func newFakeCommunityNode(t *testing.T, host string, seedByte byte, front string, now time.Time, alive bool) *fakeCommunityNode {
	t.Helper()
	node := &fakeCommunityNode{}
	node.alive.Store(alive)
	originHost := strings.TrimPrefix(strings.TrimPrefix(front, "https://"), "http://")
	node.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case exchangePath:
			node.pulls.Add(1)
			if !node.alive.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			var entries []json.RawMessage
			if node.sample != nil {
				entries = node.sample()
			} else {
				entries = []json.RawMessage{node.gossip}
			}
			_ = json.NewEncoder(w).Encode(entries)
		case "/.well-known/windshare":
			node.probes.Add(1)
			if !node.alive.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write(node.live)
		case "/v2/ws":
			if !node.alive.Load() || r.Header.Get("Origin") != front {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{originHost}})
			if err != nil {
				return
			}
			_ = ws.CloseNow()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(node.server.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(node.server.URL, "https://"))
	node.base = "https://" + host + ":" + port
	node.identity = "wss://" + host + ":" + port + "/v2/ws"
	node.gossip = envelopeWithSeed(t, seedByte, node.identity, serviceauthority.GossipDomain, now, 24*time.Hour, serviceauthority.RelayAnonymous)
	node.live = envelopeWithSeed(t, seedByte, node.identity, serviceauthority.DescriptorDomain, now, 5*time.Minute, serviceauthority.RelayAnonymous)
	return node
}

func insecureLocalPolicy(sampleServer *httptest.Server) publicdial.Policy {
	local := netip.MustParseAddr("127.0.0.1")
	tlsConfig := sampleServer.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConfig.InsecureSkipVerify = true
	return publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: tlsConfig,
	}
}

func TestRefreshSeedTierOrderDeduplicationAndBoundedAdvancement(t *testing.T) {
	now := time.Now()
	const front = "https://share.example"
	scope := Scope{Origin: front, Generation: 1}

	configured := newFakeCommunityNode(t, "cfg.example", 1, front, now, true)
	active1 := newFakeCommunityNode(t, "act1.example", 2, front, now, true)
	fallback := newFakeCommunityNode(t, "fb.example", 3, front, now, true)

	// 1. Explicit configured seed wins before active and fallback tiers.
	pool := New("", insecureLocalPolicy(configured.server), func() time.Time { return now }, diagnostics.Sink{})
	defer pool.Close()
	pool.Refresh(t.Context(), scope, Seeds{
		Configured: []string{configured.base},
		Active:     []string{active1.base},
		Fallback:   []string{fallback.base},
	})
	if configured.pulls.Load() != 1 || active1.pulls.Load() != 0 || fallback.pulls.Load() != 0 {
		t.Fatalf("configured tier did not short-circuit: cfg=%d active=%d fb=%d",
			configured.pulls.Load(), active1.pulls.Load(), fallback.pulls.Load())
	}

	// 2. Healthy local cache short-circuits before touching any seed tier.
	pool.Refresh(t.Context(), scope, Seeds{
		Configured: []string{fallback.base},
		Active:     []string{active1.base},
		Fallback:   []string{fallback.base},
	})
	if configured.pulls.Load() != 2 || active1.pulls.Load() != 0 || fallback.pulls.Load() != 0 {
		t.Fatalf("healthy cache touched bootstrap tiers: cfg=%d active=%d fb=%d",
			configured.pulls.Load(), active1.pulls.Load(), fallback.pulls.Load())
	}

	// 3. Active tier advances past >=fanout dead seeds within one cycle and stays ahead of Fallback even with non-zero cursor.
	dead1 := newFakeCommunityNode(t, "d1.example", 11, front, now, false)
	dead2 := newFakeCommunityNode(t, "d2.example", 12, front, now, false)
	alive3 := newFakeCommunityNode(t, "a3.example", 13, front, now, true)
	fbUnused := newFakeCommunityNode(t, "fbu.example", 14, front, now, true)

	advPool := New("", insecureLocalPolicy(alive3.server), func() time.Time { return now }, diagnostics.Sink{})
	defer advPool.Close()
	advPool.cursor = 6 // Even after prior cycles moved cursor, Active still runs before Fallback.
	advPool.Refresh(t.Context(), scope, Seeds{
		Active:   []string{dead1.base, dead2.base, alive3.base},
		Fallback: []string{fbUnused.base},
	})
	if dead1.pulls.Load() != 1 || dead2.pulls.Load() != 1 || alive3.pulls.Load() != 1 || fbUnused.pulls.Load() != 0 {
		t.Fatalf("active tier advancement unexpected: d1=%d d2=%d a3=%d fb=%d",
			dead1.pulls.Load(), dead2.pulls.Load(), alive3.pulls.Load(), fbUnused.pulls.Load())
	}
	if peers := advPool.Candidates(scope); len(peers) != 1 || !peers[0].Verified {
		t.Fatalf("expected alive3 peer to be discovered and verified, got %+v", peers)
	}

	// 4. Large dead Active tier caps at maxTierAttempts so Fallback is still reached, and duplicate origins are not re-dialed.
	deadTier := make([]*fakeCommunityNode, maxTierAttempts+2)
	deadURLs := make([]string, len(deadTier))
	for i := range deadTier {
		deadTier[i] = newFakeCommunityNode(t, "dead-tier.example", byte(20+i), front, now, false)
		deadURLs[i] = deadTier[i].base
	}
	fbAlive := newFakeCommunityNode(t, "fb-alive.example", 40, front, now, true)
	capPool := New("", insecureLocalPolicy(fbAlive.server), func() time.Time { return now }, diagnostics.Sink{})
	defer capPool.Close()
	capPool.Refresh(t.Context(), scope, Seeds{
		Active:   deadURLs,
		Fallback: []string{deadURLs[0] + "/", fbAlive.base},
	})
	totalDeadPulls := int32(0)
	for _, n := range deadTier {
		totalDeadPulls += n.pulls.Load()
	}
	if totalDeadPulls != maxTierAttempts || deadTier[0].pulls.Load() != 1 || fbAlive.pulls.Load() != 1 {
		t.Fatalf("tier cap/dedup failed: totalDead=%d firstDead=%d fbAlive=%d",
			totalDeadPulls, deadTier[0].pulls.Load(), fbAlive.pulls.Load())
	}

	// 5. Wall-clock seedPullBudget bounds blackholed seed recovery.
	blackhole := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer blackhole.Close()
	_, bhPort, _ := net.SplitHostPort(strings.TrimPrefix(blackhole.URL, "https://"))
	bhSeed := "https://blackhole.example:" + bhPort
	budgetPool := New("", insecureLocalPolicy(blackhole), func() time.Time { return now }, diagnostics.Sink{})
	defer budgetPool.Close()
	budgetPool.seedPullBudget = 60 * time.Millisecond
	started := time.Now()
	budgetPool.Refresh(t.Context(), scope, Seeds{Active: []string{bhSeed}})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blackholed seed pull ignored seedPullBudget: elapsed=%v", elapsed)
	}
}

func TestStaleCacheRecoveryWarmsNewlyDiscoveredPeersWithoutReprobingDeadOnes(t *testing.T) {
	now := time.Now()
	const front = "https://share.example"
	scope := Scope{Origin: front, Generation: 1}

	// Dead cached peers sort alphabetically ahead of newly discovered healthy peers.
	deadA := newFakeCommunityNode(t, "a-dead.example", 51, front, now, false)
	deadB := newFakeCommunityNode(t, "b-dead.example", 52, front, now, false)
	okC := newFakeCommunityNode(t, "c-ok.example", 53, front, now, true)
	okD := newFakeCommunityNode(t, "d-ok.example", 54, front, now, true)
	seed := newFakeCommunityNode(t, "seed.example", 55, front, now, true)
	seed.sample = func() []json.RawMessage { return []json.RawMessage{okC.gossip, okD.gossip} }

	pool := New("", insecureLocalPolicy(seed.server), func() time.Time { return now }, diagnostics.Sink{})
	defer pool.Close()
	if !pool.accept(deadA.gossip, "cache") || !pool.accept(deadB.gossip, "cache") {
		t.Fatal("failed to seed stale cached peers")
	}

	pool.Refresh(t.Context(), scope, Seeds{Active: []string{seed.base}})
	verified := 0
	for _, c := range pool.Candidates(scope) {
		if c.Verified {
			verified++
		}
	}
	if verified != fanout {
		t.Fatalf("expected %d newly discovered peers verified immediately, got %d (%+v)", fanout, verified, pool.Candidates(scope))
	}
	if len(pool.STUN(scope)) != fanout {
		t.Fatalf("expected %d verified community STUN endpoints, got %+v", fanout, pool.STUN(scope))
	}
	if deadA.probes.Load() != 1 || deadB.probes.Load() != 1 {
		t.Fatalf("dead cached peers were re-probed during follow-up warm: a=%d b=%d", deadA.probes.Load(), deadB.probes.Load())
	}
	if okC.probes.Load() != 1 || okD.probes.Load() != 1 {
		t.Fatalf("newly discovered peers were not warmed exactly once: c=%d d=%d", okC.probes.Load(), okD.probes.Load())
	}

	// Partial healthy cache (1 healthy peer) fills only the remaining fanout-1 slot
	// when pull discovers additional peers, without re-probing the already-verified peer.
	p1 := newFakeCommunityNode(t, "p1.example", 61, front, now, true)
	p2 := newFakeCommunityNode(t, "p2.example", 62, front, now, true)
	p3 := newFakeCommunityNode(t, "p3.example", 63, front, now, true)
	p1.sample = func() []json.RawMessage { return []json.RawMessage{p1.gossip, p2.gossip, p3.gossip} }

	partialPool := New("", insecureLocalPolicy(p1.server), func() time.Time { return now }, diagnostics.Sink{})
	defer partialPool.Close()
	if !partialPool.accept(p1.gossip, "cache") {
		t.Fatal("failed to seed partial cache")
	}
	partialPool.Refresh(t.Context(), scope, Seeds{})
	partialVerified := 0
	for _, c := range partialPool.Candidates(scope) {
		if c.Verified {
			partialVerified++
		}
	}
	if partialVerified != fanout || p1.probes.Load() != 1 || p2.probes.Load()+p3.probes.Load() != 1 {
		t.Fatalf("partial cache follow-up warm mismatch: verified=%d probes=(%d,%d,%d)",
			partialVerified, p1.probes.Load(), p2.probes.Load(), p3.probes.Load())
	}

	// Offline stale cache (initial warm fails AND pull fails) must not run a second warm pass.
	offPeers := make([]*fakeCommunityNode, 4)
	offlinePool := New("", insecureLocalPolicy(p1.server), func() time.Time { return now }, diagnostics.Sink{})
	defer offlinePool.Close()
	for i := range offPeers {
		offPeers[i] = newFakeCommunityNode(t, string(rune('a'+i))+"-off.example", byte(70+i), front, now, false)
		if !offlinePool.accept(offPeers[i].gossip, "cache") {
			t.Fatalf("failed to seed offline peer %d", i)
		}
	}
	offlinePool.Refresh(t.Context(), scope, Seeds{Active: []string{offPeers[0].base}})
	totalProbes := int32(0)
	for _, n := range offPeers {
		totalProbes += n.probes.Load()
	}
	if totalProbes != fanout {
		t.Fatalf("offline stale cache amplified warm probes: got %d, want %d", totalProbes, fanout)
	}
}
