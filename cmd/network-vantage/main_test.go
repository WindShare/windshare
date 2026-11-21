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

func TestNextPollTimestampStaysMonotonicAcrossClockStepBack(t *testing.T) {
	var a agent
	if got := a.nextPollTimestamp(time.Unix(1000, 0)); got != 1000 {
		t.Fatalf("first timestamp = %d; want 1000", got)
	}
	if got := a.nextPollTimestamp(time.Unix(970, 0)); got != 1001 {
		t.Fatalf("step-back timestamp = %d; want 1001", got)
	}
	if got := a.nextPollTimestamp(time.Unix(1001, 0)); got != 1002 {
		t.Fatalf("equal timestamp = %d; want 1002", got)
	}
	if got := a.nextPollTimestamp(time.Unix(1060, 0)); got != 1060 {
		t.Fatalf("caught-up timestamp = %d; want 1060", got)
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
					_ = json.NewEncoder(w).Encode(map[string]any{"cycle": 41, "zone": zone, "nodes": nodes, "origins": []string{}})
				case "cycle":
					reports++
					var report struct {
						Cycle  uint64
						Mirror *string
						Nodes  map[string]networkprobe.Observation
					}
					if err := json.Unmarshal(body, &report); err != nil {
						t.Error(err)
						return
					}
					if report.Cycle != 41 || len(report.Nodes) != 2 {
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
