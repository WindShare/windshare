package main

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
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/account"
	"github.com/windshare/windshare/engine/builtinnetwork"
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
	monitorLoop(ctx, interval, 10*time.Millisecond, func(context.Context) error {
		calls++
		if calls == 1 {
			time.Sleep(30 * time.Millisecond)
			firstFinished = time.Now()
			return nil
		}
		secondStarted = time.Now()
		cancel()
		return nil
	}, sleepContext)
	if calls != 2 {
		t.Fatalf("calls = %d; want 2", calls)
	}
	if gap := secondStarted.Sub(firstFinished); gap < interval-5*time.Millisecond {
		t.Fatalf("post-completion wait = %v; want at least %v", gap, interval)
	}
}

func TestMonitorLoopBacksOffOnFailureAndResetsOnRecovery(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// 7 consecutive failures verify exponential doubling and capping at fleetInterval;
	// recovery then restores fleetInterval and resets backoff for a later transient fault.
	transient := errors.New("authority cycle returned HTTP 503")
	outcomes := []error{
		transient,
		transient,
		transient,
		transient,
		transient,
		transient,
		transient,
		nil,
		transient,
		nil,
	}
	var delays []time.Duration
	index := 0
	monitorLoop(ctx, fleetInterval, fleetRetryInitial, func(context.Context) error {
		err := outcomes[index]
		index++
		if index == len(outcomes) {
			cancel()
		}
		return err
	}, func(_ context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		return true
	})

	want := []time.Duration{
		15 * time.Second,
		30 * time.Second,
		60 * time.Second,
		120 * time.Second,
		240 * time.Second,
		fleetInterval,
		fleetInterval,
		fleetInterval,
		15 * time.Second,
	}
	if !slices.Equal(delays, want) {
		t.Fatalf("delays = %v; want %v", delays, want)
	}
	if !strings.Contains(logs.String(), `"msg":"fleet cycle failed"`) || !strings.Contains(logs.String(), `"msg":"fleet cycle recovered"`) {
		t.Fatalf("expected failure and recovery logs, got %q", logs.String())
	}
}

func TestRetryBackoffEdgeCases(t *testing.T) {
	if got := retryBackoff(fleetRetryInitial, fleetInterval, 0); got != fleetInterval {
		t.Fatalf("failures=0 delay = %v; want %v", got, fleetInterval)
	}
	if got := retryBackoff(0, fleetInterval, 1); got != fleetInterval {
		t.Fatalf("initial=0 delay = %v; want %v", got, fleetInterval)
	}
	if got := retryBackoff(10*time.Minute, fleetInterval, 1); got != fleetInterval {
		t.Fatalf("initial>max delay = %v; want %v", got, fleetInterval)
	}
	if got := retryBackoff(2*time.Nanosecond, 5*time.Nanosecond, 2); got != 4*time.Nanosecond {
		t.Fatalf("odd max step-2 delay = %v; want 4ns", got)
	}
	if got := retryBackoff(2*time.Nanosecond, 5*time.Nanosecond, 3); got != 5*time.Nanosecond {
		t.Fatalf("odd max cap delay = %v; want 5ns", got)
	}
	if got := retryBackoff(fleetRetryInitial, fleetInterval, 1000); got != fleetInterval {
		t.Fatalf("high failure count delay = %v; want %v", got, fleetInterval)
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

func writeSignedReleaseFile(t *testing.T, dir, name, network, linkHost, authorityURL string) string {
	t.Helper()
	b64 := base64.RawURLEncoding
	rootA := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{21}, ed25519.SeedSize))
	rootB := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{22}, ed25519.SeedSize))
	online := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{23}, ed25519.SeedSize))
	authRoot := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{24}, ed25519.SeedSize))
	now := time.Now().UTC().Truncate(time.Second)
	roots := []nodedirectory.Root{
		{ID: "stage-a", Key: rootA.Public().(ed25519.PublicKey)},
		{ID: "stage-b", Key: rootB.Public().(ed25519.PublicKey)},
	}
	cert, err := nodedirectory.Certify(nodedirectory.Certificate{
		Format:     nodedirectory.Format,
		Network:    network,
		Generation: 1,
		Key:        b64.EncodeToString(online.Public().(ed25519.PublicKey)),
		NotBefore:  now.Add(-time.Minute),
		ExpiresAt:  now.Add(24 * time.Hour),
	}, "stage-a", rootA)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := nodedirectory.Sign(nodedirectory.Payload{
		Format:     nodedirectory.Format,
		Network:    network,
		Generation: 1,
		Serial:     1,
		IssuedAt:   now,
		ExpiresAt:  now.Add(24 * time.Hour),
	}, cert, online, roots)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"network":   network,
		"link_host": linkHost,
		"roots": []map[string]string{
			{"id": "stage-a", "key": b64.EncodeToString(rootA.Public().(ed25519.PublicKey))},
			{"id": "stage-b", "key": b64.EncodeToString(rootB.Public().(ed25519.PublicKey))},
		},
		"snapshot": json.RawMessage(snapshot),
	}
	if authorityURL != "" {
		manifest["authority"] = map[string]string{
			"url":  authorityURL,
			"root": b64.EncodeToString(authRoot.Public().(ed25519.PublicKey)),
		}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigOverridesReleaseAndAuthority(t *testing.T) {
	t.Parallel()
	embedded, err := builtinnetwork.Config("")
	if err != nil {
		t.Fatal(err)
	}

	def, err := loadConfig("", "")
	if err != nil {
		t.Fatal(err)
	}
	if def.LinkHost != embedded.LinkHost || def.Authority == nil || def.Authority.URL != embedded.Authority.URL {
		t.Fatalf("default config = %+v, want embedded %+v", def, embedded)
	}

	overrideOnly, err := loadConfig("", "https://staging-api.example.workers.dev/")
	if err != nil {
		t.Fatal(err)
	}
	if overrideOnly.LinkHost != embedded.LinkHost || overrideOnly.Directory.Network != embedded.Directory.Network || overrideOnly.Authority.URL != "https://staging-api.example.workers.dev" {
		t.Fatalf("authority-only override config = %+v", overrideOnly)
	}

	dir := t.TempDir()
	releaseWithAuthority := writeSignedReleaseFile(t, dir, "with-authority.json", "windshare-staging", "https://stage-front.example", "https://stage-api.example/")
	fromRelease, err := loadConfig(releaseWithAuthority, "")
	if err != nil {
		t.Fatal(err)
	}
	if fromRelease.Directory.Network != "windshare-staging" || fromRelease.LinkHost != "https://stage-front.example" || fromRelease.Authority.URL != "https://stage-api.example" {
		t.Fatalf("release-override config = %+v", fromRelease)
	}

	releaseWithoutAuthority := writeSignedReleaseFile(t, dir, "no-authority.json", "windshare-staging", "https://stage-front.example", "")
	if _, err := loadConfig(releaseWithoutAuthority, ""); err == nil {
		t.Fatal("release without authority accepted when no -authority override given")
	}
	combined, err := loadConfig(releaseWithoutAuthority, "https://direct-origin.example/")
	if err != nil {
		t.Fatal(err)
	}
	if combined.Directory.Network != "windshare-staging" || combined.LinkHost != "https://stage-front.example" || combined.Authority.URL != "https://direct-origin.example" {
		t.Fatalf("combined override config = %+v", combined)
	}

	for _, bad := range []string{"http://stage-api.example", "https://stage-api.example/v1", "https://user@stage-api.example"} {
		if _, err := loadConfig("", bad); err == nil {
			t.Fatalf("accepted invalid -authority %q", bad)
		}
	}
	if _, err := loadConfig(filepath.Join(dir, "missing.json"), ""); err == nil {
		t.Fatal("accepted missing -release file")
	}
}

func TestPostSurfacesAuthorityFaultJSONAndFallsBackOnPlainText(t *testing.T) {
	t.Parallel()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, ed25519.SeedSize))
	cases := []struct {
		path       string
		status     int
		body       string
		wantKind   string
		wantReason string
		wantError  string
	}{
		{
			path:       "poll",
			status:     http.StatusForbidden,
			body:       `{"kind":"denied","reason":"unknown_vantage"}`,
			wantKind:   "denied",
			wantReason: "unknown_vantage",
			wantError:  "authority poll: HTTP 403 (denied: unknown_vantage)",
		},
		{
			path:      "result",
			status:    http.StatusNotFound,
			body:      `{"kind":"node_not_found"}`,
			wantKind:  "node_not_found",
			wantError: "authority result: HTTP 404 (node_not_found)",
		},
		{
			path:      "cycle",
			status:    http.StatusServiceUnavailable,
			body:      "fleet temporarily unavailable",
			wantError: "authority cycle: HTTP 503",
		},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			a := agent{
				key:       key,
				authority: server.Client(),
				config:    &networkconfig.Config{Authority: &account.Config{URL: server.URL}},
			}
			err := a.post(t.Context(), tc.path, pollDomain, map[string]int64{"at": 1}, nil)
			var authorityErr *networkprobe.AuthorityError
			if !errors.As(err, &authorityErr) {
				t.Fatalf("error = %T (%v), want *networkprobe.AuthorityError", err, err)
			}
			if authorityErr.StatusCode != tc.status || authorityErr.Kind != tc.wantKind || authorityErr.Reason != tc.wantReason || err.Error() != tc.wantError {
				t.Fatalf("error = %+v (%q), want status=%d kind=%q reason=%q err=%q",
					authorityErr, err.Error(), tc.status, tc.wantKind, tc.wantReason, tc.wantError)
			}
		})
	}
}
