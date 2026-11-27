package networkruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/connectivity/publicdial"
	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/community"
	"github.com/windshare/windshare/engine/internal/statefile"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
)

type cachedCommunityPeer struct {
	host       string
	seedByte   byte
	capacityBS uint64
}

func writeCachedCommunityPool(t *testing.T, r *Runtime, peers ...cachedCommunityPeer) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	data, err := os.ReadFile("../../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct{ Descriptor struct{ Descriptor string } }
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	basePayload, err := enc.DecodeString(fixture.Descriptor.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	envelopes := make([]any, 0, len(peers))
	for _, peer := range peers {
		var d map[string]any
		if err := json.Unmarshal(basePayload, &d); err != nil {
			t.Fatal(err)
		}
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{peer.seedByte}, 32))
		pub := key.Public().(ed25519.PublicKey)
		digest := sha256.Sum256(append([]byte("windshare/node-identity/v1\x00"), pub...))
		d["node_key"] = enc.EncodeToString(pub)
		d["node_id"] = enc.EncodeToString(digest[:])
		d["relay_endpoint"] = "wss://" + peer.host + "/v2/ws"
		d["authority"], d["platform"], d["issued_at"], d["expires_at"] = nil, "native", r.now().Unix(), r.now().Add(24*time.Hour).Unix()
		if peer.capacityBS > 0 {
			d["public_capacity_bytes_per_second"] = peer.capacityBS
		}
		payload, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		sig := ed25519.Sign(key, append([]byte(serviceauthority.GossipDomain), payload...))
		envelopes = append(envelopes, map[string]any{"version": 2, "descriptor": enc.EncodeToString(payload), "signature": enc.EncodeToString(sig)})
	}
	raw, err := json.Marshal(envelopes)
	if err != nil {
		t.Fatal(err)
	}
	if err := statefile.Write(t.Context(), filepath.Join(statefile.StateDirectory(p), "community-peers.json"), raw); err != nil {
		t.Fatal(err)
	}
	r.community = community.New(p, publicdial.Policy{}, r.now, diagnostics.Sink{})
	r.config.CommunitySettingsPath = p
	return p
}

func TestCachedCommunityPlanWithoutDirectoryDoesNotLeakTicketsOrColdHints(t *testing.T) {
	r := fixtureRuntime(t)
	const peer = "https://cached.example"
	writeCachedCommunityPool(t, r, cachedCommunityPeer{host: "cached.example", seedByte: 43})
	// A missing trust store has the same discovery boundary as an expired snapshot.
	r.directory.Close()
	r.directory = nil
	selection, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil || r.Unavailability() != nil || len(selection.Relays) != 1 {
		t.Fatal(selection, err, r.Unavailability())
	}
	reg := selection.Relays[0]
	if selection.Plan.Network[0].Source != relayplan.Community || reg.Authorization != nil || reg.Community == nil || reg.Community.Origin != r.config.LinkHost {
		t.Fatal(reg, selection.Plan)
	}
	capability := link.Link{}
	selection.PublishRelayHints(&capability, []string{peer})
	if len(capability.Relays) != 0 {
		t.Fatal("unverified community hint", capability)
	}
	reg.Community.Ready.Store(true)
	selection.PublishRelayHints(&capability, []string{peer})
	if !slices.Equal(capability.Relays, []string{peer}) {
		t.Fatal(capability)
	}
	if _, err := r.Plan(t.Context(), relayprofile.Plan(nil).WithoutCommunity(), ""); err == nil {
		t.Fatal("opt-out included community candidate")
	}
}

func TestCommunityObservationDiagnosticsAndFirstFailureDemotion(t *testing.T) {
	r := fixtureRuntime(t)
	writeCachedCommunityPool(t, r,
		cachedCommunityPeer{host: "a-high.example", seedByte: 44, capacityBS: 12_500_000},
		cachedCommunityPeer{host: "b-default.example", seedByte: 45, capacityBS: 3_125_000},
	)
	r.directory.Close()
	r.directory = nil

	type diagnosticCandidate struct {
		Endpoint            string `json:"endpoint"`
		Observation         string `json:"observation"`
		ObservedHandshakeMS string `json:"observed_handshake_ms"`
	}
	readDiagnostics := func(sel Selection) []diagnosticCandidate {
		t.Helper()
		raw, err := sel.DiagnosticRecord().Payload().Get("initial_candidates").MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var out []diagnosticCandidate
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	const highURL, defaultURL = "https://a-high.example", "https://b-default.example"
	initial, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil || len(initial.Plan.Network) != 2 || initial.Plan.Network[0].URL != highURL {
		t.Fatalf("initial community plan = (%+v, %v)", initial.Plan, err)
	}

	observe := r.Observer(initial, diagnostics.Sink{})
	observe(highURL, relayset.Attempt{State: relayset.AttemptStarted})
	observe(highURL, relayset.Attempt{State: relayset.AttemptSucceeded, HandshakeDuration: 145 * time.Millisecond})

	afterAdmit, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil {
		t.Fatal(err)
	}
	got := readDiagnostics(afterAdmit)
	if len(got) != 2 || got[0].Endpoint != highURL || got[0].Observation != "admitted" || got[0].ObservedHandshakeMS != "145" {
		t.Fatalf("admitted community diagnostics = %+v", got)
	}

	// First transport failure records Admitted=false with CooldownUntil=0; the failed high-capacity
	// peer must drop behind the unmeasured peer and report observation="failed".
	observe(highURL, relayset.Attempt{State: relayset.AttemptStarted})
	observe(highURL, relayset.Attempt{State: relayset.AttemptFailed, Failure: errors.New("dial tcp timeout")})

	afterFail, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil {
		t.Fatal(err)
	}
	got = readDiagnostics(afterFail)
	if len(got) != 2 || got[0].Endpoint != defaultURL || got[0].Observation != "unmeasured" ||
		got[1].Endpoint != highURL || got[1].Observation != "failed" || got[1].ObservedHandshakeMS != "" {
		t.Fatalf("post-failure community diagnostics = %+v", got)
	}
}

func TestCommunityReadsStayLockFreeAndPlanHonorsCancelOnPendingRecovery(t *testing.T) {
	r := fixtureRuntime(t)
	p := writeCachedCommunityPool(t, r, cachedCommunityPeer{host: "lockfree.example", seedByte: 46})
	r.directory.Close()
	r.directory = nil
	if err := statefile.Write(t.Context(), p, []byte(`{"profiles":[]}`)); err != nil {
		t.Fatal(err)
	}

	// Ordinary Plan, Unavailability, and ICEPool reads must not create config.json.lock.
	if selection, err := r.Plan(t.Context(), relayprofile.Plan(nil), ""); err != nil || len(selection.Relays) != 1 {
		t.Fatalf("lock-free Plan=(%+v, %v)", selection, err)
	}
	if failure := r.Unavailability(); failure != nil {
		t.Fatalf("lock-free Unavailability=%v", failure)
	}
	_ = r.ICEPool()
	if _, err := os.Stat(p + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary community reads created lock file: err=%v", err)
	}

	// Placing an unwritable barrier at config.json.lock simulates read-only lock failure.
	if err := os.Mkdir(p+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, allowed := r.communitySettings(t.Context()); !allowed {
		t.Fatal("read-only lock barrier disabled communitySettings")
	}
	if selection, err := r.Plan(t.Context(), relayprofile.Plan(nil), ""); err != nil || len(selection.Relays) != 1 {
		t.Fatalf("read-only Plan=(%+v, %v)", selection, err)
	}
	if failure := r.Unavailability(); failure != nil {
		t.Fatalf("read-only Unavailability=%v", failure)
	}
	_ = r.ICEPool()
	if err := os.Remove(p + ".lock"); err != nil {
		t.Fatal(err)
	}

	// Under external lock contention with no commit.json, reads still proceed without blocking.
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- statefile.WithLock(t.Context(), p, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	if selection, err := r.Plan(t.Context(), relayprofile.Plan(nil), ""); err != nil || len(selection.Relays) != 1 {
		close(release)
		t.Fatalf("contended lock-free Plan=(%+v, %v)", selection, err)
	}
	if failure := r.Unavailability(); failure != nil {
		close(release)
		t.Fatalf("contended lock-free Unavailability=%v", failure)
	}
	_ = r.ICEPool()

	// When commit.json recovery is pending under lock contention, Plan honors caller ctx cancellation.
	commitPayload := `[{"name":"settings","data":"eyJwcm9maWxlcyI6W119"},{"name":"community-eligibility.json","data":"e30="}]`
	if err := statefile.Write(t.Context(), filepath.Join(statefile.StateDirectory(p), "commit.json"), []byte(commitPayload)); err != nil {
		close(release)
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := r.Plan(ctx, relayprofile.Plan(nil), ""); !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("contended recovery Plan err=%v, want context.DeadlineExceeded", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if selection, err := r.Plan(t.Context(), relayprofile.Plan(nil), ""); err != nil || len(selection.Relays) != 1 {
		t.Fatalf("post-recovery Plan=(%+v, %v)", selection, err)
	}
}

func TestBootstrapDirectoryWinDoesNotPoisonCommunityRefreshOrVerify(t *testing.T) {
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
	rootKey, err := base64.RawURLEncoding.DecodeString(fixture.Roots[0].Key)
	if err != nil {
		t.Fatal(err)
	}

	const front = "https://front.example"
	var gossip, live json.RawMessage
	var pulls, probes atomic.Int32
	var probeAborted atomic.Bool
	warmEntered, releaseWarm := make(chan struct{}), make(chan struct{})
	var warmOnce sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/discovery/v1/peers":
			pulls.Add(1)
			_ = json.NewEncoder(w).Encode([]json.RawMessage{gossip})
		case "/.well-known/windshare":
			probes.Add(1)
			warmOnce.Do(func() { close(warmEntered) })
			select {
			case <-r.Context().Done():
				probeAborted.Store(true)
				return
			case <-releaseWarm:
			}
			_, _ = w.Write(live)
		case "/v2/ws":
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"front.example"}})
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
	makeEnvelope := func(domain string, lifetime time.Duration) json.RawMessage {
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{46}, ed25519.SeedSize))
		pub := key.Public().(ed25519.PublicKey)
		digest := sha256.Sum256(append([]byte("windshare/node-identity/v1\x00"), pub...))
		enc := base64.RawURLEncoding.EncodeToString
		payload, err := json.Marshal(map[string]any{
			"version": 2, "platform": "native", "node_id": enc(digest[:]), "node_key": enc(pub),
			"relay_endpoint": identity, "services": map[string]bool{"relay": true, "stun": false},
			"public_relay": serviceauthority.RelayAnonymous,
			"issued_at":    fixture.Now.Unix(), "expires_at": fixture.Now.Add(lifetime).Unix(),
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
	gossip = makeEnvelope(serviceauthority.GossipDomain, 24*time.Hour)
	live = makeEnvelope(serviceauthority.DescriptorDomain, 5*time.Minute)

	releaseDir := make(chan struct{})
	r, err := New(nil, func() time.Time { return fixture.Now }, "", diagnostics.Sink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	r.config.LinkHost = front
	r.config.BootstrapNodes = []string{seed}
	r.directory, err = networkdirectory.New(networkdirectory.Config{
		Network: "test",
		Roots:   []nodedirectory.Root{{ID: fixture.Roots[0].ID, Key: ed25519.PublicKey(rootKey)}},
		Mirrors: []string{"https://mirror.example/network"},
		Now:     r.now,
		Fetch: func(ctx context.Context, _ string) ([]byte, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-releaseDir:
				return fixture.Entries[0].Envelope, nil
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	local := netip.MustParseAddr("127.0.0.1")
	r.community = community.New("", publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig,
	}, r.now, diagnostics.Sink{})

	type planResult struct {
		sel Selection
		err error
	}
	planned := make(chan planResult, 1)
	go func() {
		sel, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
		planned <- planResult{sel, err}
	}()
	select {
	case <-warmEntered:
	case res := <-planned:
		t.Fatalf("Plan returned before community warm started: (%+v, %v)", res.sel, res.err)
	}

	scope := r.scope(front)
	refreshDone := make(chan struct{})
	go func() {
		r.community.Refresh(r.ctx, scope, r.seeds(relayconfig.Config{}))
		close(refreshDone)
	}()
	verifyErr := make(chan error, 1)
	go func() {
		verifyErr <- r.community.Verify(t.Context(), scope, identity)
	}()

	// Let directory.Refresh win bootstrap() and cancel bootstrapCtx while both
	// a coalesced Refresh and an unrelated live share's Verify are waiting.
	close(releaseDir)
	res := <-planned
	if res.err != nil || len(res.sel.Plan.DirectoryCandidates()) == 0 {
		t.Fatalf("bootstrap plan = (%+v, %v), want official candidates", res.sel.Plan, res.err)
	}

	close(releaseWarm)
	if err := <-verifyErr; err != nil {
		t.Fatalf("unrelated live Verify poisoned by bootstrap cancellation: %v", err)
	}
	<-refreshDone
	if probeAborted.Load() {
		t.Fatal("bootstrap cancellation aborted the in-flight community probe")
	}
	if pulls.Load() != 1 || probes.Load() != 1 {
		t.Fatalf("expected coalesced refresh/verify to reuse single flight, got pulls=%d probes=%d", pulls.Load(), probes.Load())
	}
	if peers := r.community.Candidates(scope); len(peers) != 1 || !peers[0].Verified {
		t.Fatalf("coalesced community refresh failed to complete: %+v", peers)
	}
}

func TestSeedsOrderUsableDirectoryAheadOfEmbeddedAndRetainExpiredFallback(t *testing.T) {
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
	rootKey, err := base64.RawURLEncoding.DecodeString(fixture.Roots[0].Key)
	if err != nil {
		t.Fatal(err)
	}

	var clock atomic.Int64
	clock.Store(fixture.Now.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }

	r, err := New(nil, now, "", diagnostics.Sink{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	r.config.LinkHost = "https://front.example"
	r.config.BootstrapNodes = []string{
		"https://retired1.example",
		"https://retired2.example",
		"https://JP1.example:443/", // canonical duplicate of usable directory node
	}
	r.directory, err = networkdirectory.New(networkdirectory.Config{
		Network:  "test",
		Roots:    []nodedirectory.Root{{ID: fixture.Roots[0].ID, Key: ed25519.PublicKey(rootKey)}},
		Embedded: fixture.Entries[0].Envelope, // active node: https://jp1.example
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Usable directory: Active contains directory node; Fallback drops duplicate jp1 and retains retired embedded seeds.
	got := r.seeds(relayconfig.Config{})
	if len(got.Configured) != 0 ||
		!slices.Equal(got.Active, []string{"https://jp1.example"}) ||
		!slices.Equal(got.Fallback, []string{"https://retired1.example", "https://retired2.example"}) {
		t.Fatalf("usable directory seeds = %+v", got)
	}

	// Explicit user-configured seeds take precedence and deduplicate against lower tiers.
	gotConfigured := r.seeds(relayconfig.Config{
		BootstrapNodes: []string{"https://custom.example", "https://jp1.example/"},
	})
	if !slices.Equal(gotConfigured.Configured, []string{"https://custom.example", "https://jp1.example/"}) ||
		len(gotConfigured.Active) != 0 ||
		!slices.Equal(gotConfigured.Fallback, []string{"https://retired1.example", "https://retired2.example"}) {
		t.Fatalf("configured seeds = %+v", gotConfigured)
	}

	// End-to-end: 2 dead embedded seeds must NOT starve the active directory node on cold refresh.
	const front = "https://front.example"
	var deadHits, dirHits atomic.Int32
	deadServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deadHits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer deadServer.Close()

	var gossip, live json.RawMessage
	dirServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/discovery/v1/peers":
			dirHits.Add(1)
			_ = json.NewEncoder(w).Encode([]json.RawMessage{gossip})
		case "/.well-known/windshare":
			_, _ = w.Write(live)
		case "/v2/ws":
			ws, err := websocket.Accept(w, req, &websocket.AcceptOptions{OriginPatterns: []string{"front.example"}})
			if err != nil {
				return
			}
			_ = ws.CloseNow()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer dirServer.Close()

	_, dirPort, _ := net.SplitHostPort(strings.TrimPrefix(dirServer.URL, "https://"))
	_, deadPort, _ := net.SplitHostPort(strings.TrimPrefix(deadServer.URL, "https://"))
	r.config.BootstrapNodes = []string{
		"https://retired1.example:" + deadPort,
		"https://retired2.example:" + deadPort,
	}
	peerIdentity := "wss://peer.example:" + dirPort + "/v2/ws"
	makeEnvelope := func(domain string, lifetime time.Duration) json.RawMessage {
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{91}, ed25519.SeedSize))
		pub := key.Public().(ed25519.PublicKey)
		digest := sha256.Sum256(append([]byte("windshare/node-identity/v1\x00"), pub...))
		enc := base64.RawURLEncoding.EncodeToString
		payload, err := json.Marshal(map[string]any{
			"version": 2, "platform": "native", "node_id": enc(digest[:]), "node_key": enc(pub),
			"relay_endpoint": peerIdentity, "services": map[string]bool{"relay": true, "stun": false},
			"public_relay": serviceauthority.RelayAnonymous,
			"issued_at":    now().Unix(), "expires_at": now().Add(lifetime).Unix(),
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
	gossip = makeEnvelope(serviceauthority.GossipDomain, 24*time.Hour)
	live = makeEnvelope(serviceauthority.DescriptorDomain, 5*time.Minute)

	local := netip.MustParseAddr("127.0.0.1")
	tlsConfig := dirServer.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConfig.InsecureSkipVerify = true
	r.community = community.New("", publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: tlsConfig,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == "127.0.0.1:443" {
				addr = "127.0.0.1:" + dirPort
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}, now, diagnostics.Sink{})

	scope := r.scope(front)
	r.community.Refresh(t.Context(), scope, r.seeds(relayconfig.Config{}))
	if dirHits.Load() != 1 || deadHits.Load() != 0 {
		t.Fatalf("active directory seed was not prioritized ahead of dead embedded seeds: dirHits=%d deadHits=%d", dirHits.Load(), deadHits.Load())
	}
	if peers := r.community.Candidates(scope); len(peers) != 1 || !peers[0].Verified {
		t.Fatalf("expected peer discovered via active directory seed to be verified, got %+v", peers)
	}

	// Expired directory (past payload expiry + grace): Active becomes empty, and
	// historical snapshot URLs remain in Fallback behind embedded seeds.
	clock.Store(fixture.Now.Add(30 * 24 * time.Hour).UnixNano())
	r.config.BootstrapNodes = []string{"https://community-seed.example"}
	gotExpired := r.seeds(relayconfig.Config{})
	if len(gotExpired.Configured) != 0 ||
		len(gotExpired.Active) != 0 ||
		!slices.Equal(gotExpired.Fallback, []string{"https://community-seed.example", "https://jp1.example"}) {
		t.Fatalf("expired directory seeds = %+v", gotExpired)
	}
}

func TestCommunityVerifyFailureBackoffUnblocksSubsequentFallbackPlans(t *testing.T) {
	r := fixtureRuntime(t)
	now := r.now()
	r.directory.Close()
	r.directory = nil

	const altFrontend = "https://alt-front.example"
	var broken503Recover atomic.Bool

	sign := func(seedByte byte, endpoint, domain string, state serviceauthority.PublicRelay, capBS uint64) json.RawMessage {
		t.Helper()
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seedByte}, ed25519.SeedSize))
		pub := key.Public().(ed25519.PublicKey)
		digest := sha256.Sum256(append([]byte("windshare/node-identity/v1\x00"), pub...))
		enc := base64.RawURLEncoding.EncodeToString
		lifetime := 24 * time.Hour
		if domain == serviceauthority.DescriptorDomain {
			lifetime = 5 * time.Minute
		}
		payload, err := json.Marshal(map[string]any{
			"version":                          2,
			"platform":                         "native",
			"node_id":                          enc(digest[:]),
			"node_key":                         enc(pub),
			"relay_endpoint":                   endpoint,
			"services":                         map[string]bool{"relay": true, "stun": false},
			"public_relay":                     state,
			"public_capacity_bytes_per_second": capBS,
			"issued_at":                        now.Unix(),
			"expires_at":                       now.Add(lifetime).Unix(),
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

	type peerSpec struct {
		host     string
		seedByte byte
		capBS    uint64
		live     serviceauthority.PublicRelay
	}
	specs := []peerSpec{
		{host: "a-sybil-403.example.com", seedByte: 51, capBS: math.MaxUint64, live: serviceauthority.RelayAnonymous},
		{host: "b-sybil-budget.example.com", seedByte: 52, capBS: math.MaxUint64, live: serviceauthority.RelayBudgetExhausted},
		{host: "c-broken-503.example.com", seedByte: 53, capBS: 125_000_000, live: serviceauthority.RelayAnonymous},
		{host: "d-healthy-1.example.com", seedByte: 54, capBS: 3_125_000, live: serviceauthority.RelayAnonymous},
		{host: "e-healthy-2.example.com", seedByte: 55, capBS: 3_125_000, live: serviceauthority.RelayAnonymous},
	}

	var port string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host, _, _ := net.SplitHostPort(req.Host)
		var spec peerSpec
		found := false
		for _, candidate := range specs {
			if candidate.host == host {
				spec, found = candidate, true
				break
			}
		}
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		endpoint := "wss://" + spec.host + ":" + port + "/v2/ws"
		switch req.URL.Path {
		case "/.well-known/windshare":
			if spec.host == "c-broken-503.example.com" && !broken503Recover.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write(sign(spec.seedByte, endpoint, serviceauthority.DescriptorDomain, spec.live, spec.capBS))
		case "/v2/ws":
			if spec.host == "a-sybil-403.example.com" && req.Header.Get("Origin") != altFrontend {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			ws, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			_ = ws.CloseNow()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	_, port, _ = net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))

	p := filepath.Join(t.TempDir(), "config.json")
	envelopes := make([]json.RawMessage, 0, len(specs))
	for _, spec := range specs {
		envelopes = append(envelopes, sign(spec.seedByte, "wss://"+spec.host+":"+port+"/v2/ws", serviceauthority.GossipDomain, serviceauthority.RelayAnonymous, spec.capBS))
	}
	raw, err := json.Marshal(envelopes)
	if err != nil {
		t.Fatal(err)
	}
	if err := statefile.Write(t.Context(), filepath.Join(statefile.StateDirectory(p), "community-peers.json"), raw); err != nil {
		t.Fatal(err)
	}
	local := netip.MustParseAddr("127.0.0.1")
	r.community = community.New(p, publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig,
	}, r.now, diagnostics.Sink{})
	r.config.CommunitySettingsPath = p

	urlFor := func(host string) string { return "https://" + host + ":" + port }

	// Plan 1: the two MaxUint64 Sybil nodes win cold slots, then both fail route.Check.
	sel1, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil || len(sel1.Relays) != 2 || sel1.Relays[0].PublicURL != urlFor("a-sybil-403.example.com") || sel1.Relays[1].PublicURL != urlFor("b-sybil-budget.example.com") {
		t.Fatalf("plan 1 = (%+v, %v)", sel1.Relays, err)
	}
	for _, reg := range sel1.Relays {
		if err := reg.Community.Check(t.Context()); err == nil {
			t.Fatalf("expected Check failure for %s", reg.PublicURL)
		}
	}

	// Plan 2: both rejected/budget-exhausted Sybils are suppressed; broken-503 and healthy-1 surface.
	sel2, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil || len(sel2.Relays) != 2 || sel2.Relays[0].PublicURL != urlFor("c-broken-503.example.com") || sel2.Relays[1].PublicURL != urlFor("d-healthy-1.example.com") {
		t.Fatalf("plan 2 = (%+v, %v)", sel2.Relays, err)
	}
	if err := sel2.Relays[0].Community.Check(t.Context()); err == nil {
		t.Fatal("expected 503 Check failure")
	}
	if err := sel2.Relays[1].Community.Check(t.Context()); err != nil {
		t.Fatalf("expected healthy-1 Check success, got %v", err)
	}

	// Plan 3: broken-503 is filtered via community_cooldown and both healthy peers take the slots.
	sel3, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil || len(sel3.Relays) != 2 || sel3.Relays[0].PublicURL != urlFor("d-healthy-1.example.com") || sel3.Relays[1].PublicURL != urlFor("e-healthy-2.example.com") {
		t.Fatalf("plan 3 = (%+v, %v)", sel3.Relays, err)
	}
	if !slices.ContainsFunc(sel3.Decision.Filters, func(f networkconfig.Filter) bool {
		return f.Endpoint == urlFor("c-broken-503.example.com") && f.Reason == "community_cooldown"
	}) {
		t.Fatalf("missing community_cooldown filter for broken-503: %+v", sel3.Decision.Filters)
	}

	// Switching to an allowed alternate frontend still lets a-sybil-403 verify (Origin isolation).
	selAlt, err := r.Plan(t.Context(), relayprofile.Plan(nil), altFrontend)
	if err != nil || len(selAlt.Relays) == 0 || selAlt.Relays[0].PublicURL != urlFor("a-sybil-403.example.com") {
		t.Fatalf("alt-frontend plan = (%+v, %v)", selAlt.Relays, err)
	}
	if err := selAlt.Relays[0].Community.Check(t.Context()); err != nil {
		t.Fatalf("a-sybil-403 should succeed on allowed alt frontend: %v", err)
	}

	// Sole transiently-failed community peer remains retryable via CooldownFallback (no NoRelayCandidates lockout).
	r.community.Close()
	soloPath := filepath.Join(t.TempDir(), "config.json")
	soloRaw, err := json.Marshal([]json.RawMessage{
		sign(53, "wss://c-broken-503.example.com:"+port+"/v2/ws", serviceauthority.GossipDomain, serviceauthority.RelayAnonymous, 125_000_000),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := statefile.Write(t.Context(), filepath.Join(statefile.StateDirectory(soloPath), "community-peers.json"), soloRaw); err != nil {
		t.Fatal(err)
	}
	r.community = community.New(soloPath, publicdial.Policy{
		Permit:          func(a netip.Addr) bool { return a == local },
		Lookup:          func(context.Context, string, string) ([]netip.Addr, error) { return []netip.Addr{local}, nil },
		TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig,
	}, r.now, diagnostics.Sink{})
	r.config.CommunitySettingsPath = soloPath

	solo1, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil || len(solo1.Relays) != 1 {
		t.Fatalf("solo plan 1 = (%+v, %v)", solo1.Relays, err)
	}
	if err := solo1.Relays[0].Community.Check(t.Context()); err == nil {
		t.Fatal("expected initial solo 503 failure")
	}
	broken503Recover.Store(true)
	solo2, err := r.Plan(t.Context(), relayprofile.Plan(nil), "")
	if err != nil || !solo2.Decision.CooldownFallback || len(solo2.Relays) != 1 {
		t.Fatalf("solo retry must fall back instead of returning NoRelayCandidates: (%+v, %v)", solo2, err)
	}
	if err := solo2.Relays[0].Community.Check(t.Context()); err != nil {
		t.Fatalf("solo retry failed after recovery: %v", err)
	}
}
