package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/osfs"
)

func signedCanaryDirectory(t *testing.T, now time.Time, activeURLs []string, canaryURL string) ([]nodedirectory.Root, []byte) {
	t.Helper()
	rootKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	onlineKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32))
	roots := []nodedirectory.Root{{ID: "root", Key: rootKey.Public().(ed25519.PublicKey)}}
	onlinePub := base64.RawURLEncoding.EncodeToString(onlineKey.Public().(ed25519.PublicKey))
	cert, err := nodedirectory.Certify(nodedirectory.Certificate{
		Format: 1, Network: "test", Generation: 1, Key: onlinePub,
		NotBefore: now.Add(-time.Hour), ExpiresAt: now.Add(24 * time.Hour),
	}, "root", rootKey)
	if err != nil {
		t.Fatal(err)
	}
	nodeKey := func(seed byte) string {
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32))
		return base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	}
	nodes := make([]nodedirectory.Node, 0, len(activeURLs)+1)
	for i, activeURL := range activeURLs {
		nodes = append(nodes, nodedirectory.Node{
			Alias: "active" + string(rune('1'+i)), NodeKey: nodeKey(byte(10 + i)), Relay: activeURL,
			Roles: []string{"relay"}, Zone: "global", Region: "jp", Provider: "p" + string(rune('1'+i)),
			Operator: "op" + string(rune('1'+i)), CapacityMbps: 50, Status: nodedirectory.Active,
		})
	}
	nodes = append(nodes, nodedirectory.Node{
		Alias: "canary1", NodeKey: nodeKey(20), Relay: canaryURL,
		Roles: []string{"relay"}, Zone: "global", Region: "sg", Provider: "p3",
		Operator: "op3", CapacityMbps: 50, Status: nodedirectory.Canary,
	})
	payload := nodedirectory.Payload{
		Format: 1, Network: "test", Generation: 1, Serial: 1,
		IssuedAt: now, ExpiresAt: now.Add(24 * time.Hour),
		Nodes: nodes,
	}
	envelope, err := nodedirectory.Sign(payload, cert, onlineKey, roots)
	if err != nil {
		t.Fatal(err)
	}
	return roots, envelope
}

func TestShareEmitsCanaryFallbackDiagnosticWithoutSlog(t *testing.T) {
	var slogOutput bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&slogOutput, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	activeURLs := []string{"https://active1.example", "https://active2.example"}
	const canaryURL = "https://canary.example"
	now := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	roots, envelope := signedCanaryDirectory(t, now, activeURLs, canaryURL)
	rootDir := t.TempDir()
	source := liveshare.FileSourceFactoryFunc(func(ctx context.Context, scope liveshare.FileSourceContext) (liveshare.FileSource, error) {
		return osfs.NewSelectedFileSource(ctx, osfs.SelectedCatalogSourceConfig{
			Paths: []string{rootDir}, SyntheticRoot: scope.SyntheticRoot,
			Identities: osfs.CatalogIdentitySourceFunc(scope.NewIdentity),
		})
	})

	synctest.Test(t, func(t *testing.T) {
		transport := &unavailableTransport{}
		previous := http.DefaultClient.Transport
		http.DefaultClient.Transport = transport
		t.Cleanup(func() { http.DefaultClient.Transport = previous })

		stream, err := diagnostics.NewStream(64, nil)
		if err != nil {
			t.Fatal(err)
		}
		dirConfig := networkdirectory.Config{Network: "test", Roots: roots, Embedded: envelope}
		app := newTestEngine(t, Config{
			Clock:   doubleSpeedClock{origin: now, started: time.Now()},
			Network: &networkconfig.Config{Directory: dirConfig},
			Share: ShareDependencies{
				Peers:       func() (SharePeers, error) { return &facadePeers{}, nil },
				PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
			},
		})
		task, err := app.StartShare(t.Context(), ShareRequest{
			Source:      source,
			RelayIntent: relayprofile.Plan(nil),
			Diagnostics: stream.Sink(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(task.selection.Plan.Network) != len(activeURLs) || len(task.selection.Plan.Standby) != 1 {
			t.Fatalf("unexpected plan: network=%v standby=%v", task.selection.Plan.Network, task.selection.Plan.Standby)
		}
		synctest.Wait()
		task.StopShare()
		if _, err := task.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		stream.Close()

		var sawFallback bool
		for record := range stream.Records() {
			if record.Name() != "network_relay_fallback" {
				continue
			}
			scope := record.Scope()
			if scope.EngineID == "" || scope.TaskID == "" || scope.Workflow != "share" {
				t.Fatalf("unexpected fallback scope: %+v", scope)
			}
			var active, canaries []string
			rawActive, err := record.Payload().Get("active_relays").MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			rawCanaries, err := record.Payload().Get("next_relays").MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(rawActive, &active); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(rawCanaries, &canaries); err != nil {
				t.Fatal(err)
			}
			slices.Sort(active)
			if len(active) != 1 || !slices.Contains(activeURLs, active[0]) || !slices.Equal(canaries, []string{canaryURL}) {
				t.Fatalf("unexpected fallback payload: active=%v canaries=%v", active, canaries)
			}
			sawFallback = true
		}
		if !sawFallback {
			t.Fatal("missing network_relay_fallback diagnostic record")
		}
		if slogOutput.Len() != 0 {
			t.Fatalf("share canary fallback bypassed diagnostics sink via slog: %s", slogOutput.String())
		}
	})
}
