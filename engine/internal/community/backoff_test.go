package community

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
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/connectivity/publicdial"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/serviceauthority"
)

func signedEnvelope(t *testing.T, seedByte byte, endpoint, domain string, now time.Time, lifetime time.Duration, state serviceauthority.PublicRelay, capBS *uint64) json.RawMessage {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seedByte}, ed25519.SeedSize))
	public := key.Public().(ed25519.PublicKey)
	digest := sha256.Sum256(append([]byte("windshare/node-identity/v1\x00"), public...))
	encode := base64.RawURLEncoding.EncodeToString
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"version":        2,
		"platform":       "native",
		"node_id":        encode(digest[:]),
		"node_key":       encode(public),
		"relay_endpoint": endpoint,
		"services":       map[string]bool{"relay": true, "stun": true},
		"stun":           []string{"stun:" + u.Hostname() + ":3478"},
		"public_relay":   state,
		"issued_at":      now.Unix(),
		"expires_at":     now.Add(lifetime).Unix(),
	}
	if capBS != nil {
		doc["public_capacity_bytes_per_second"] = *capBS
	}
	payload, err := json.Marshal(doc)
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

func TestOrigin403BackoffIsScopedEscalatesAndRecovers(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const originA, originB = "https://a.example", "https://b.example"
	var allowB atomic.Bool
	var liveRequests, wsRequests atomic.Int32
	var identity string
	var liveDoc atomic.Value
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/windshare":
			liveRequests.Add(1)
			_, _ = w.Write(liveDoc.Load().(json.RawMessage))
		case "/v2/ws":
			wsRequests.Add(1)
			origin := r.Header.Get("Origin")
			if origin != originA && !(origin == originB && allowB.Load()) {
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
	identity = "wss://peer.example.com:" + port + "/v2/ws"
	liveDoc.Store(signedEnvelope(t, 1, identity, serviceauthority.DescriptorDomain, now, 5*time.Minute, serviceauthority.RelayAnonymous, nil))

	stream, err := diagnostics.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	local := netip.MustParseAddr("127.0.0.1")
	pool := New("", publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig,
	}, func() time.Time { return now }, stream.Sink())
	defer pool.Close()

	if !pool.accept(signedEnvelope(t, 1, identity, serviceauthority.GossipDomain, now, 24*time.Hour, serviceauthority.RelayAnonymous, nil), "seed") {
		t.Fatal("failed to accept peer")
	}

	scopeA := Scope{Origin: originA, Generation: 1}
	scopeB := Scope{Origin: originB, Generation: 1}

	if err := pool.Verify(t.Context(), scopeB, identity); err == nil {
		t.Fatal("expected HTTP 403 verification failure")
	}
	if got := pool.Candidates(scopeB); len(got) != 0 {
		t.Fatalf("403-rejected peer still offered for scopeB: %+v", got)
	}
	// Concurrent/repeated Verify during rejected backoff fails fast with zero extra HTTP dials.
	beforeLive, beforeWS := liveRequests.Load(), wsRequests.Load()
	if err := pool.Verify(t.Context(), scopeB, identity); err == nil {
		t.Fatal("expected fast-fail during rejected backoff")
	}
	if liveRequests.Load() != beforeLive || wsRequests.Load() != beforeWS {
		t.Fatalf("backed-off Verify dialed network: live=%d->%d ws=%d->%d", beforeLive, liveRequests.Load(), beforeWS, wsRequests.Load())
	}

	// Re-gossiping a fresher same-key descriptor must NOT wipe active backoff.
	if !pool.accept(signedEnvelope(t, 1, identity, serviceauthority.GossipDomain, now.Add(time.Second), 24*time.Hour, serviceauthority.RelayAnonymous, nil), "seed") {
		t.Fatal("expected fresher same-key gossip to be accepted")
	}
	if got := pool.Candidates(scopeB); len(got) != 0 {
		t.Fatalf("same-key gossip refresh bypassed rejected backoff: %+v", got)
	}

	// Origin A must remain completely unaffected and verify cleanly.
	if got := pool.Candidates(scopeA); len(got) != 1 || got[0].ProbeFailed {
		t.Fatalf("scopeA poisoned by scopeB 403: %+v", got)
	}
	if err := pool.Verify(t.Context(), scopeA, identity); err != nil {
		t.Fatalf("scopeA Verify failed: %v", err)
	}
	if got := pool.Candidates(scopeA); len(got) != 1 || !got[0].Verified {
		t.Fatalf("scopeA not verified: %+v", got)
	}

	// After 1m rejected backoff expires (within 3m decay), peer returns with ProbeFailed=true
	// and a second 403 escalates backoff to 2m.
	now = now.Add(time.Minute)
	liveDoc.Store(signedEnvelope(t, 1, identity, serviceauthority.DescriptorDomain, now, 5*time.Minute, serviceauthority.RelayAnonymous, nil))
	if got := pool.Candidates(scopeB); len(got) != 1 || !got[0].ProbeFailed || !got[0].CooldownUntil.IsZero() {
		t.Fatalf("post-backoff decayed candidate = %+v", got)
	}
	if err := pool.Verify(t.Context(), scopeB, identity); err == nil {
		t.Fatal("expected second 403")
	}
	now = now.Add(90 * time.Second)
	if got := pool.Candidates(scopeB); len(got) != 0 {
		t.Fatalf("second 403 did not escalate to 2m backoff: %+v", got)
	}

	// Network generation change immediately clears old-generation rejection.
	scopeBGen2 := Scope{Origin: originB, Generation: 2}
	if got := pool.Candidates(scopeBGen2); len(got) != 1 || got[0].ProbeFailed {
		t.Fatalf("new network generation inherited old backoff: %+v", got)
	}
	allowB.Store(true)
	liveDoc.Store(signedEnvelope(t, 1, identity, serviceauthority.DescriptorDomain, now, 5*time.Minute, serviceauthority.RelayAnonymous, nil))
	if err := pool.Verify(t.Context(), scopeBGen2, identity); err != nil {
		t.Fatalf("expected recovery after origin allowed: %v", err)
	}
	if got := pool.Candidates(scopeBGen2); len(got) != 1 || !got[0].Verified || got[0].ProbeFailed {
		t.Fatalf("recovered candidate = %+v", got)
	}

	// Verify structured failure diagnostic was emitted for 403.
	sawOriginForbidden := false
	for {
		select {
		case rec := <-stream.Records():
			if rec.Name() == "community_probe_failed" && rec.Payload().Get("reason").Text() == "origin_forbidden" &&
				rec.Payload().Get("kind").Text() == "rejected" && rec.Payload().Get("backoff_ms").Uint() == uint64(time.Minute/time.Millisecond) {
				sawOriginForbidden = true
			}
		default:
			if !sawOriginForbidden {
				t.Fatal("missing community_probe_failed diagnostic for origin_forbidden")
			}
			return
		}
	}
}

func TestTransientFailureBackoffWarmSkipAndCallerCancelImmunity(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const origin = "https://share.example"
	var badHits, goodHits atomic.Int32
	var badFail atomic.Bool
	badFail.Store(true)
	blockBad := make(chan struct{})
	badEntered := make(chan struct{}, 1)

	var goodLive atomic.Value
	var badLive atomic.Value
	muxSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isBad := strings.HasPrefix(r.Host, "a-bad.example.com")
		switch r.URL.Path {
		case "/.well-known/windshare":
			if isBad {
				badHits.Add(1)
				select {
				case badEntered <- struct{}{}:
				default:
				}
				select {
				case <-blockBad:
				case <-r.Context().Done():
					return
				}
				if badFail.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write(badLive.Load().(json.RawMessage))
				return
			}
			goodHits.Add(1)
			_, _ = w.Write(goodLive.Load().(json.RawMessage))
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
	defer muxSrv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(muxSrv.URL, "https://"))
	badID := "wss://a-bad.example.com:" + port + "/v2/ws"
	goodID := "wss://b-good.example.com:" + port + "/v2/ws"
	badLive.Store(signedEnvelope(t, 10, badID, serviceauthority.DescriptorDomain, now, 5*time.Minute, serviceauthority.RelayAnonymous, nil))
	goodLive.Store(signedEnvelope(t, 11, goodID, serviceauthority.DescriptorDomain, now, 5*time.Minute, serviceauthority.RelayAnonymous, nil))
	badGossip := signedEnvelope(t, 10, badID, serviceauthority.GossipDomain, now, 24*time.Hour, serviceauthority.RelayAnonymous, nil)
	goodGossip := signedEnvelope(t, 11, goodID, serviceauthority.GossipDomain, now, 24*time.Hour, serviceauthority.RelayAnonymous, nil)

	local := netip.MustParseAddr("127.0.0.1")
	pool := New("", publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: muxSrv.Client().Transport.(*http.Transport).TLSClientConfig,
	}, func() time.Time { return now }, diagnostics.Sink{})
	defer pool.Close()

	if !pool.accept(badGossip, "seed") || !pool.accept(goodGossip, "seed") {
		t.Fatal("failed to accept peers")
	}
	scope := Scope{Origin: origin, Generation: 1}

	// 1. Pool shutdown cancelling p.ctx mid-probe must NOT record backoff or emit failure traces,
	//    and caller cancellation unblocks without poisoning peer state.
	shutdownStream, err := diagnostics.NewStream(8, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownStream.Close()
	shutdownPool := New("", publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: muxSrv.Client().Transport.(*http.Transport).TLSClientConfig,
	}, func() time.Time { return now }, shutdownStream.Sink())
	if !shutdownPool.accept(badGossip, "seed") {
		t.Fatal("failed to accept peer")
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- shutdownPool.Verify(t.Context(), scope, badID) }()
	select {
	case <-badEntered:
		shutdownPool.Close()
		if err := <-shutdownDone; err == nil {
			t.Fatal("expected closed pool Verify to return error")
		}
	case err := <-shutdownDone:
		shutdownPool.Close()
		t.Fatalf("Verify returned before reaching server: %v", err)
	}
	for _, c := range shutdownPool.Candidates(scope) {
		if c.Identity == badID && (!c.CooldownUntil.IsZero() || c.ProbeFailed) {
			t.Fatalf("pool shutdown poisoned peer state: %+v", c)
		}
	}
	for drained := false; !drained; {
		select {
		case rec := <-shutdownStream.Records():
			if rec.Name() == "community_probe_failed" {
				t.Fatalf("pool shutdown emitted community_probe_failed: %+v", rec)
			}
		default:
			drained = true
		}
	}

	// 2. Real transient 503 failure records 15s CooldownUntil + ProbeFailed=true.
	close(blockBad)
	if err := pool.Verify(t.Context(), scope, badID); err == nil {
		t.Fatal("expected 503 Verify failure")
	}
	var badCandFound bool
	for _, c := range pool.Candidates(scope) {
		if c.Identity == badID {
			badCandFound = true
			if !c.ProbeFailed || !c.CooldownUntil.Equal(now.Add(15*time.Second)) {
				t.Fatalf("transient backoff candidate = %+v, want CooldownUntil=%v ProbeFailed=true", c, now.Add(15*time.Second))
			}
		}
	}
	if !badCandFound {
		t.Fatal("transiently failed peer must remain in Candidates with CooldownUntil for last-resort fallback")
	}

	// 3. Background warm() skips backed-off a-bad and warms b-good instead.
	beforeBad := badHits.Load()
	pool.warm(t.Context(), scope, 0, fanout, nil)
	if badHits.Load() != beforeBad || goodHits.Load() == 0 {
		t.Fatalf("warm() did not skip backed-off peer: bad=%d->%d good=%d", beforeBad, badHits.Load(), goodHits.Load())
	}

	// 4. Explicit Verify on a transiently failed peer still probes (so last-resort retry works when peer recovers).
	badFail.Store(false)
	if err := pool.Verify(t.Context(), scope, badID); err != nil {
		t.Fatalf("last-resort Verify on recovered transient peer failed: %v", err)
	}
	for _, c := range pool.Candidates(scope) {
		if c.Identity == badID && (!c.Verified || c.ProbeFailed || !c.CooldownUntil.IsZero()) {
			t.Fatalf("recovered peer retained failure state: %+v", c)
		}
	}

	// 5. After cooldown expires without recovery, ProbeFailed persists for probeFailureDecay then decays.
	badFail.Store(true)
	now = now.Add(6 * time.Minute) // expire previous live observation
	if err := pool.Verify(t.Context(), scope, badID); err == nil {
		t.Fatal("expected transient failure")
	}
	now = now.Add(20 * time.Second) // past 15s cooldown, inside 3m decay
	for _, c := range pool.Candidates(scope) {
		if c.Identity == badID && (!c.ProbeFailed || !c.CooldownUntil.IsZero()) {
			t.Fatalf("post-cooldown candidate = %+v, want ProbeFailed=true CooldownUntil=0", c)
		}
	}
	now = now.Add(probeFailureDecay) // past decay window
	for _, c := range pool.Candidates(scope) {
		if c.Identity == badID && (c.ProbeFailed || !c.CooldownUntil.IsZero()) {
			t.Fatalf("decayed candidate still marked failed: %+v", c)
		}
	}
}

func TestLiveDescriptorSuppressionAndEviction(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const originA, originB = "https://a.example", "https://b.example"
	var liveDoc atomic.Value
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/windshare":
			hits.Add(1)
			_, _ = w.Write(liveDoc.Load().(json.RawMessage))
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
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	identity := "wss://peer.example.com:" + port + "/v2/ws"

	stream, err := diagnostics.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	local := netip.MustParseAddr("127.0.0.1")
	pool := New("", publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig,
	}, func() time.Time { return now }, stream.Sink())
	defer pool.Close()

	if !pool.accept(signedEnvelope(t, 20, identity, serviceauthority.GossipDomain, now, 24*time.Hour, serviceauthority.RelayAnonymous, nil), "seed") {
		t.Fatal("failed to accept peer")
	}

	// 1. Live signed BudgetExhausted suppresses relay across ALL origins while retaining the peer record.
	liveDoc.Store(signedEnvelope(t, 20, identity, serviceauthority.DescriptorDomain, now, 5*time.Minute, serviceauthority.RelayBudgetExhausted, nil))
	scopeA := Scope{Origin: originA, Generation: 1}
	scopeB := Scope{Origin: originB, Generation: 1}
	if err := pool.Verify(t.Context(), scopeA, identity); err == nil {
		t.Fatal("expected Verify to fail for live budget_exhausted")
	}
	if len(pool.records()) != 1 {
		t.Fatalf("budget_exhausted peer must be retained in cache for recovery, got %d records", len(pool.records()))
	}
	if len(pool.Candidates(scopeA)) != 0 || len(pool.Candidates(scopeB)) != 0 {
		t.Fatalf("live budget_exhausted peer still offered as relay candidate: A=%v B=%v", pool.Candidates(scopeA), pool.Candidates(scopeB))
	}
	before := hits.Load()
	if err := pool.Verify(t.Context(), scopeB, identity); err == nil || hits.Load() != before {
		t.Fatalf("suppressed peer did not fast-fail across origins: err=%v hits=%d->%d", err, before, hits.Load())
	}

	// Once the 5m live TTL expires and the node recovers to RelayAnonymous, candidacy resumes.
	now = now.Add(5*time.Minute + time.Second)
	liveDoc.Store(signedEnvelope(t, 20, identity, serviceauthority.DescriptorDomain, now, 5*time.Minute, serviceauthority.RelayAnonymous, nil))
	if len(pool.Candidates(scopeA)) != 1 {
		t.Fatalf("expected peer to return after live suppression TTL expired, got %+v", pool.Candidates(scopeA))
	}
	if err := pool.Verify(t.Context(), scopeA, identity); err != nil {
		t.Fatalf("expected peer to verify after budget recovery: %v", err)
	}

	// 2. Live signed RelayDisabled immediately evicts the peer from both relay and STUN.
	now = now.Add(6 * time.Minute)
	liveDoc.Store(signedEnvelope(t, 20, identity, serviceauthority.DescriptorDomain, now, 5*time.Minute, serviceauthority.RelayDisabled, nil))
	if err := pool.Verify(t.Context(), scopeA, identity); err == nil {
		t.Fatal("expected Verify to fail for live disabled")
	}
	if len(pool.records()) != 0 || len(pool.Candidates(scopeA)) != 0 || len(pool.STUN(scopeA.Generation)) != 0 {
		t.Fatalf("live disabled peer was not evicted: records=%d candidates=%v stun=%v", len(pool.records()), pool.Candidates(scopeA), pool.STUN(scopeA.Generation))
	}

	sawBudget, sawEvicted := false, false
	for {
		select {
		case rec := <-stream.Records():
			if rec.Name() == "community_probe_failed" && rec.Payload().Get("reason").Text() == "live_budget_exhausted" {
				sawBudget = true
			}
			if rec.Name() == "community_peer_evicted" && rec.Payload().Get("reason").Text() == "live_disabled" {
				sawEvicted = true
			}
		default:
			if !sawBudget || !sawEvicted {
				t.Fatalf("missing expected traces: sawBudget=%t sawEvicted=%t", sawBudget, sawEvicted)
			}
			return
		}
	}
}
