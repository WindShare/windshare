package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestCycleSignsMirrorEvidenceAndDistinguishesShutdownFromNodeFailure(t *testing.T) {
	dir, digest := mirrorFixture(t)
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
						Mirror string
						Shares [][link.SenderAuthenticatedShareIDBytes]byte
					}
					if err := json.Unmarshal(data, &poll); err != nil || poll.Mirror != digest || len(poll.Shares) != networkprobe.OriginCount || poll.Shares[0] == poll.Shares[1] {
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
			a := agent{key: key, mainland: true, client: node.Client(), authority: authority.Client(),
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
