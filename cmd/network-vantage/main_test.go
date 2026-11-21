package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/account"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/internal/networkprobe"
)

func mirrorFixture(t *testing.T) (networkdirectory.Config, string) {
	t.Helper()
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	online := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))
	now := time.Now().UTC().Truncate(time.Second)
	roots := []nodedirectory.Root{{ID: "test", Key: root.Public().(ed25519.PublicKey)}}
	grant, err := nodedirectory.Certify(nodedirectory.Certificate{Format: 1, Network: "test", Generation: 1,
		Key: base64.RawURLEncoding.EncodeToString(online.Public().(ed25519.PublicKey)), NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}, "test", root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := nodedirectory.Sign(nodedirectory.Payload{Format: 1, Network: "test", Generation: 1, Serial: 1, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}, grant, online, roots)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := nodedirectory.Verify(data, "test", roots)
	if err != nil {
		t.Fatal(err)
	}
	digest := snapshot.Cursor().Digest
	return networkdirectory.Config{Network: "test", Roots: roots, Embedded: data}, base64.RawURLEncoding.EncodeToString(digest[:])
}

func TestEnrollmentCycleDistinguishesShutdownFromNodeFailure(t *testing.T) {
	dir, _ := mirrorFixture(t)
	for _, want := range []string{"released", "failed"} {
		t.Run(want, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			nodeMux := http.NewServeMux()
			nodeMux.HandleFunc("GET /.well-known/windshare-network", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(dir.Embedded)
			})
			nodeMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
				if want == "released" {
					cancel()
				}
				http.Error(w, "stopped", http.StatusServiceUnavailable)
			})
			node := httptest.NewTLSServer(nodeMux)
			defer node.Close()
			key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize))
			pub := key.Public().(ed25519.PublicKey)
			b64 := base64.RawURLEncoding
			nodeKey := b64.EncodeToString(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, ed25519.SeedSize)).Public().(ed25519.PublicKey))
			reported := make(chan string, 1)
			authority := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var proof struct{ Key, Payload, Signature string }
				if err := json.NewDecoder(r.Body).Decode(&proof); err != nil {
					t.Error(err)
					return
				}
				data, decodeErr := b64.DecodeString(proof.Payload)
				sig, sigErr := b64.DecodeString(proof.Signature)
				domain := pollDomain
				if r.URL.Path == "/v1/vantage/result" {
					domain = resultDomain
				}
				if decodeErr != nil || sigErr != nil || proof.Key != b64.EncodeToString(pub) || !ed25519.Verify(pub, append([]byte(domain), data...), sig) {
					t.Error("invalid signed vantage request")
					return
				}
				switch r.URL.Path {
				case "/v1/vantage/poll":
					var poll struct {
						Shares [][link.SenderAuthenticatedShareIDBytes]byte
					}
					if err := json.Unmarshal(data, &poll); err != nil || len(poll.Shares) != networkprobe.OriginCount || poll.Shares[0] == poll.Shares[1] {
						t.Errorf("invalid poll evidence: %+v (%v)", poll, err)
						return
					}
					_ = json.NewEncoder(w).Encode(networkprobe.Work{OperationID: "enroll", Lease: "claim", Node: nodedirectory.Node{Relay: node.URL, NodeKey: nodeKey, Roles: []string{"relay"}}})
				case "/v1/vantage/result":
					var result struct {
						Lease   string
						Outcome struct{ Kind, Reason string }
					}
					if err := json.Unmarshal(data, &result); err != nil || result.Lease != "claim" {
						t.Errorf("invalid lease result: %v", err)
						return
					}
					if want == "failed" && !strings.Contains(result.Outcome.Reason, "health") {
						t.Errorf("missing diagnostic: %s", result.Outcome.Reason)
					}
					reported <- result.Outcome.Kind
				default:
					http.NotFound(w, r)
				}
			}))
			defer authority.Close()
			a := agent{key: key, client: node.Client(), authority: authority.Client(),
				config: &networkconfig.Config{Directory: dir, LinkHost: node.URL, Authority: &account.Config{URL: authority.URL}}}
			if err := a.cycle(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case kind := <-reported:
				if kind != want {
					t.Fatalf("reported %s; want %s", kind, want)
				}
			default:
				t.Fatal("assignment was not reported")
			}
		})
	}
}

func TestTruncateUTF8PreservesBoundariesWithinByteBudget(t *testing.T) {
	ascii := strings.Repeat("a", maxFailureReasonBytes+10)
	if got := truncateUTF8(ascii, maxFailureReasonBytes); len(got) != maxFailureReasonBytes {
		t.Fatalf("ascii length = %d; want %d", len(got), maxFailureReasonBytes)
	}
	boundary := strings.Repeat("a", maxFailureReasonBytes-1) + "中tail"
	got := truncateUTF8(boundary, maxFailureReasonBytes)
	if len(got) != maxFailureReasonBytes-1 || !utf8.ValidString(got) {
		t.Fatalf("boundary truncation = %q (len=%d, valid=%v)", got, len(got), utf8.ValidString(got))
	}
	multibyte := strings.Repeat("错", 100)
	got = truncateUTF8(multibyte, maxFailureReasonBytes)
	if len(got) > maxFailureReasonBytes || !utf8.ValidString(got) || got != strings.Repeat("错", maxFailureReasonBytes/3) {
		t.Fatalf("multibyte truncation invalid: len=%d valid=%v", len(got), utf8.ValidString(got))
	}
}

func TestPollTimestampStaysMonotonicOnlyAfterClaimedTimestamp(t *testing.T) {
	var a agent
	if got := a.pollTimestamp(time.Unix(1000, 0)); got != 1000 {
		t.Fatalf("initial timestamp = %d; want 1000", got)
	}
	if got := a.pollTimestamp(time.Unix(970, 0)); got != 970 {
		t.Fatalf("uncommitted step-back timestamp = %d; want 970", got)
	}
	a.lastClaimedAt = 1000
	if got := a.pollTimestamp(time.Unix(970, 0)); got != 1001 {
		t.Fatalf("claimed step-back timestamp = %d; want 1001", got)
	}
	if got := a.pollTimestamp(time.Unix(970, 0)); got != 1001 {
		t.Fatalf("pure repeated step-back timestamp = %d; want 1001", got)
	}
	a.lastClaimedAt = 1001
	if got := a.pollTimestamp(time.Unix(1001, 0)); got != 1002 {
		t.Fatalf("equal timestamp = %d; want 1002", got)
	}
	if got := a.pollTimestamp(time.Unix(1060, 0)); got != 1060 {
		t.Fatalf("caught-up timestamp = %d; want 1060", got)
	}
}

func TestPollCycleCommitsTimestampOnlyAfterClaimingWork(t *testing.T) {
	dir, _ := mirrorFixture(t)
	nodeMux := http.NewServeMux()
	nodeMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "offline", http.StatusServiceUnavailable)
	})
	node := httptest.NewTLSServer(nodeMux)
	defer node.Close()

	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{31}, ed25519.SeedSize))
	b64 := base64.RawURLEncoding
	nodeKey := b64.EncodeToString(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{32}, ed25519.SeedSize)).Public().(ed25519.PublicKey))

	const serverNow int64 = 1_700_000_000
	const freshSeconds int64 = 90
	var (
		hasWork      bool
		savedLeaseAt int64
		pollAts      []int64
		resultCount  int
	)
	authority := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var proof struct{ Payload string }
		if err := json.NewDecoder(r.Body).Decode(&proof); err != nil {
			t.Error(err)
			return
		}
		data, err := b64.DecodeString(proof.Payload)
		if err != nil {
			t.Error(err)
			return
		}
		switch r.URL.Path {
		case "/v1/vantage/poll":
			var poll struct{ At int64 }
			if err := json.Unmarshal(data, &poll); err != nil {
				t.Error(err)
				return
			}
			pollAts = append(pollAts, poll.At)
			diff := poll.At - serverNow
			if diff < 0 {
				diff = -diff
			}
			if diff > freshSeconds || (savedLeaseAt > 0 && poll.At <= savedLeaseAt) {
				http.Error(w, "invalid", http.StatusBadRequest)
				return
			}
			if !hasWork {
				_, _ = w.Write([]byte("null"))
				return
			}
			savedLeaseAt = poll.At
			_ = json.NewEncoder(w).Encode(networkprobe.Work{
				OperationID: "enroll",
				Lease:       "claim",
				Node:        nodedirectory.Node{Relay: node.URL, NodeKey: nodeKey, Roles: []string{"relay"}},
			})
		case "/v1/vantage/result":
			resultCount++
			_, _ = w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer authority.Close()

	clock := time.Unix(serverNow+3600, 0)
	a := agent{
		key:       key,
		client:    node.Client(),
		authority: authority.Client(),
		config:    &networkconfig.Config{Directory: dir, LinkHost: node.URL, Authority: &account.Config{URL: authority.URL}},
		now:       func() time.Time { return clock },
	}

	// Fast RTC (>90s ahead) rejected with HTTP 400 must not latch lastClaimedAt.
	if err := a.cycle(t.Context()); err == nil {
		t.Fatal("expected HTTP 400 rejection for fast RTC poll")
	}
	if a.lastClaimedAt != 0 {
		t.Fatalf("lastClaimedAt latched on rejected poll = %d; want 0", a.lastClaimedAt)
	}

	// Once NTP steps the clock back, an idle poll (work == nil) succeeds without latching.
	clock = time.Unix(serverNow, 0)
	if err := a.cycle(t.Context()); err != nil {
		t.Fatalf("post-NTP idle poll failed: %v", err)
	}
	if a.lastClaimedAt != 0 {
		t.Fatalf("lastClaimedAt latched on idle poll = %d; want 0", a.lastClaimedAt)
	}

	// Claiming work persists vantage-lease on the authority and commits lastClaimedAt.
	hasWork = true
	clock = time.Unix(serverNow+40, 0)
	if err := a.cycle(t.Context()); err != nil {
		t.Fatalf("claimed poll failed: %v", err)
	}
	if a.lastClaimedAt != serverNow+40 || savedLeaseAt != serverNow+40 {
		t.Fatalf("claimed watermark mismatch: client=%d server=%d; want %d", a.lastClaimedAt, savedLeaseAt, serverNow+40)
	}

	// A backward clock step after a real claim stays strictly above the persisted lease,
	// without ratcheting client state when the poll returns idle.
	hasWork = false
	clock = time.Unix(serverNow, 0)
	if err := a.cycle(t.Context()); err != nil {
		t.Fatalf("step-back idle poll failed: %v", err)
	}
	if a.lastClaimedAt != serverNow+40 {
		t.Fatalf("idle poll ratcheted lastClaimedAt = %d; want %d", a.lastClaimedAt, serverNow+40)
	}

	// The next real claim while still behind lastClaimedAt advances both client and server watermarks.
	hasWork = true
	clock = time.Unix(serverNow+10, 0)
	if err := a.cycle(t.Context()); err != nil {
		t.Fatalf("step-back claimed poll failed: %v", err)
	}
	if a.lastClaimedAt != serverNow+41 || savedLeaseAt != serverNow+41 {
		t.Fatalf("second claimed watermark mismatch: client=%d server=%d; want %d", a.lastClaimedAt, savedLeaseAt, serverNow+41)
	}

	wantAts := []int64{serverNow + 3600, serverNow, serverNow + 40, serverNow + 41, serverNow + 41}
	if len(pollAts) != len(wantAts) {
		t.Fatalf("poll timestamps = %v; want %v", pollAts, wantAts)
	}
	for i := range wantAts {
		if pollAts[i] != wantAts[i] {
			t.Fatalf("poll[%d].at = %d; want %d (all=%v)", i, pollAts[i], wantAts[i], pollAts)
		}
	}
	if resultCount != 2 {
		t.Fatalf("result reports = %d; want 2", resultCount)
	}
}

func TestMirrorLogsVerificationFailures(t *testing.T) {
	dir, _ := mirrorFixture(t)
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not-a-directory"))
	}))
	defer host.Close()

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)

	a := agent{

		client: host.Client(),
		config: &networkconfig.Config{Directory: dir, LinkHost: host.URL},
	}
	if got := a.mirror(t.Context()); got != nil {
		t.Fatalf("mirror = %v; want nil", *got)
	}
	if !strings.Contains(logs.String(), "mainland mirror verification failed") {
		t.Fatalf("expected mirror warning log, got %q", logs.String())
	}
}

func TestFleetCycleSendsOneSignedAggregateAndOnlyCNChecksItsMirror(t *testing.T) {
	directory, digest := mirrorFixture(t)
	b64 := base64.RawURLEncoding
	for _, zone := range []string{"cn", "global"} {
		t.Run(zone, func(t *testing.T) {
			var mirrorReads int
			mux := http.NewServeMux()
			mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "offline", http.StatusServiceUnavailable) })
			mux.HandleFunc("GET /.well-known/windshare-network", func(w http.ResponseWriter, _ *http.Request) { mirrorReads++; _, _ = w.Write(directory.Embedded) })
			node := httptest.NewTLSServer(mux)
			defer node.Close()
			signing := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{28}, 32))
			pub := signing.Public().(ed25519.PublicKey)
			nodes := []networkprobe.Target{}
			for _, tag := range []byte{8, 9} {
				nodes = append(nodes, networkprobe.Target{OperationID: string([]byte{'a' + tag}), Node: nodedirectory.Node{
					NodeKey: b64.EncodeToString(bytes.Repeat([]byte{tag}, 32)), Relay: node.URL + "/", Roles: []string{"relay"},
				}})
			}
			reports := 0
			service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var proof struct{ Key, Payload, Signature string }
				if err := json.NewDecoder(r.Body).Decode(&proof); err != nil {
					t.Error(err)
					return
				}
				body, err := b64.DecodeString(proof.Payload)
				sig, sigErr := b64.DecodeString(proof.Signature)
				action := strings.TrimPrefix(r.URL.Path, "/v1/vantage/")
				domain := "windshare/fleet-" + action + "/v1\x00"
				if err != nil || sigErr != nil || proof.Key != b64.EncodeToString(pub) || !ed25519.Verify(pub, append([]byte(domain), body...), sig) {
					t.Error("unsigned request")
					return
				}
				switch action {
				case "targets":
					_ = json.NewEncoder(w).Encode(map[string]any{"zone": zone, "nodes": nodes, "origins": []string{}})
				case "cycle":
					reports++
					var report struct {
						At     int64
						Mirror *string
						Nodes  map[string]networkprobe.Observation
					}
					if err := json.Unmarshal(body, &report); err != nil {
						t.Error(err)
						return
					}
					if report.At <= 0 || len(report.Nodes) != 2 {
						t.Errorf("aggregate: %+v", report)
					}
					if zone == "cn" && (report.Mirror == nil || *report.Mirror != digest) || zone == "global" && report.Mirror != nil {
						t.Errorf("unexpected mirror %v", report.Mirror)
					}
					for _, target := range nodes {
						got := report.Nodes[target.Node.NodeKey]
						if got.OperationID != target.OperationID || !got.TCP || got.TLSExpires == nil || got.HTTPS || got.Relay == nil || *got.Relay || got.STUN != nil {
							t.Errorf("bad observation: %+v", got)
						}
					}
				default:
					t.Errorf("unexpected %s", action)
				}
			}))
			defer service.Close()
			a := agent{key: signing, client: node.Client(), authority: service.Client(), config: &networkconfig.Config{
				LinkHost: node.URL, Directory: directory, Authority: &account.Config{URL: service.URL},
			}}
			if err := a.fleetCycle(t.Context()); err != nil {
				t.Fatal(err)
			}
			if reports != 1 || (zone == "cn") != (mirrorReads == 1) {
				t.Fatalf("%d aggregate reports, %d mirror reads", reports, mirrorReads)
			}
		})
	}
}

func TestMonitorLoopWaitsFullIntervalAfterCycleCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const interval = 40 * time.Millisecond
	var firstFinished, secondStarted time.Time
	calls := 0
	monitorLoop(ctx, interval, func(context.Context) error {
		calls++
		if calls == 1 {
			time.Sleep(30 * time.Millisecond)
			firstFinished = time.Now()
			return nil
		}
		secondStarted = time.Now()
		cancel()
		return nil
	})
	if calls != 2 {
		t.Fatalf("calls = %d; want 2", calls)
	}
	if gap := secondStarted.Sub(firstFinished); gap < interval-5*time.Millisecond {
		t.Fatalf("post-completion wait = %v; want at least %v", gap, interval)
	}
}

func TestFleetCycleBoundsMirrorInsideCycleBudget(t *testing.T) {
	directory, _ := mirrorFixture(t)
	b64 := base64.RawURLEncoding
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "offline", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("GET /.well-known/windshare-network", func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	node := httptest.NewTLSServer(mux)
	defer node.Close()

	signing := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{29}, 32))
	nodes := []networkprobe.Target{{
		OperationID: "op-mirror-bound",
		Node: nodedirectory.Node{
			NodeKey: b64.EncodeToString(bytes.Repeat([]byte{10}, 32)),
			Relay:   node.URL + "/",
			Roles:   []string{"relay"},
		},
	}}
	var reported struct {
		Mirror *string
		Nodes  map[string]networkprobe.Observation
	}
	service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/v1/vantage/") {
		case "targets":
			_ = json.NewEncoder(w).Encode(map[string]any{"zone": "cn", "nodes": nodes, "origins": []string{}})
		case "cycle":
			var proof struct{ Payload string }
			if err := json.NewDecoder(r.Body).Decode(&proof); err != nil {
				t.Error(err)
				return
			}
			body, err := b64.DecodeString(proof.Payload)
			if err != nil {
				t.Error(err)
				return
			}
			if err := json.Unmarshal(body, &reported); err != nil {
				t.Error(err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer service.Close()

	a := agent{
		key:       signing,
		client:    node.Client(),
		authority: service.Client(),
		config:    &networkconfig.Config{LinkHost: node.URL, Directory: directory, Authority: &account.Config{URL: service.URL}},
	}
	start := time.Now()
	if err := a.fleetCycleWithBudget(t.Context(), 500*time.Millisecond, 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed >= 400*time.Millisecond {
		t.Fatalf("hung mirror was not bounded inside mirrorBudget: elapsed %v", elapsed)
	}
	if reported.Mirror != nil || len(reported.Nodes) != 1 {
		t.Fatalf("expected nil mirror and 1 node observation after mirror timeout, got %+v", reported)
	}
}
