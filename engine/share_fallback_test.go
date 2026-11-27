package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/connectivity/relayplan"
	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/engine/internal/nativeconnectivity"
	"github.com/windshare/windshare/engine/internal/networkruntime"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/osfs"
	"github.com/windshare/windshare/transport/relayv2"
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
			if record.Payload().Get("reason").Text() != string(relayset.FallbackReasonWaveFallback) ||
				record.Payload().Get("next_tier").Text() != string(relayplan.TierOfficialCanary) ||
				!slices.Equal(active, activeURLs) || !slices.Equal(canaries, []string{canaryURL}) {
				t.Fatalf("unexpected fallback payload: reason=%q tier=%q active=%v canaries=%v",
					record.Payload().Get("reason").Text(), record.Payload().Get("next_tier").Text(), active, canaries)
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

type readyShareRelayEndpoint struct{}

func (*readyShareRelayEndpoint) WaitReady(context.Context) error { return nil }
func (*readyShareRelayEndpoint) ObserveRegistration(observe func(relayset.RegistrationState)) {
	observe(relayset.RegistrationAvailable)
}
func (*readyShareRelayEndpoint) Wake() {}
func (*readyShareRelayEndpoint) Accept(ctx context.Context) (*relayv2.Channel, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (*readyShareRelayEndpoint) StopRecovery()                 {}
func (*readyShareRelayEndpoint) Cleanup(context.Context) error { return nil }
func (*readyShareRelayEndpoint) CompleteObservations() relayv2.LifecycleObservationCompletion {
	return relayv2.LifecycleObservationCompletion{}
}

func TestShareReadyAndSnapshotStayImmutableAfterRetiringUnverifiedCommunityRoute(t *testing.T) {
	const (
		configuredURL = "https://own.example"
		communityURL  = "https://cold-community.example"
		frontendURL   = "https://front.example"
	)
	checkEntered := make(chan struct{})
	checkCanceled := make(chan struct{})
	coldRoute := &relayprofile.CommunityRoute{
		Client: &http.Client{},
		Origin: frontendURL,
		Check: func(ctx context.Context) error {
			close(checkEntered)
			<-ctx.Done()
			close(checkCanceled)
			return ctx.Err()
		},
	}
	plan := relayplan.Plan{
		Configured: []relayplan.Candidate{{URL: configuredURL, Identity: "wss://own.example/v2/ws", Source: relayplan.Configured}},
		Network:    []relayplan.Candidate{{URL: communityURL, Identity: "wss://cold-community.example/v2/ws", Source: relayplan.Community}},
	}
	selection := networkruntime.Selection{
		Plan:     plan,
		Frontend: frontendURL,
		Relays: []relayprofile.Registration{
			{PublicURL: configuredURL},
			{PublicURL: communityURL, Community: coldRoute},
		},
	}
	app := newTestEngine(t, Config{})
	controller := share.NewController()
	var communityPrepared atomic.Bool
	rootDir := t.TempDir()
	source := liveshare.FileSourceFactoryFunc(func(ctx context.Context, scope liveshare.FileSourceContext) (liveshare.FileSource, error) {
		return osfs.NewSelectedFileSource(ctx, osfs.SelectedCatalogSourceConfig{
			Paths: []string{rootDir}, SyntheticRoot: scope.SyntheticRoot,
			Identities: osfs.CatalogIdentitySourceFunc(scope.NewIdentity),
		})
	})
	rawTask, err := task.Start(t.Context(), task.Config[ShareResult]{
		ID:                  "share-freeze-community",
		Now:                 time.Now,
		Random:              rand.Reader,
		CleanupTimeout:      time.Second,
		ObservationCapacity: 16,
		Run: func(ctx context.Context, control task.Control) task.Completion[ShareResult] {
			return share.Run(ctx, share.Request{Source: source, Relays: selection.Relays}, share.Dependencies{
				Control:          control,
				Controller:       controller,
				RevisionCapacity: app.revisions,
				CatalogBudget:    app.catalog,
				CacheBudget:      app.cache,
				Peers:            func(nativeconnectivity.SenderConfig) (share.Peers, error) { return &facadePeers{}, nil },
				PollNetwork:      func(ctx context.Context, _ func()) { <-ctx.Done() },
				Relays:           share.PlannedRelays(plan, nil),
				Relay: func(cfg relayset.SenderLifecycleConfig) (share.Relay, error) {
					if cfg.Candidate.PublicURL == communityURL {
						communityPrepared.Store(true)
						return nil, context.Canceled
					}
					<-checkEntered
					return &readyShareRelayEndpoint{}, nil
				},
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	current := &ShareTask{Task: &Task[ShareResult]{current: rawTask}, controller: controller, selection: selection}
	defer func() {
		current.StopShare()
		_, _ = current.Wait(context.Background())
	}()

	ready1, err := current.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	<-checkCanceled
	if !slices.Equal(ready1.Capability.Relays, []string{configuredURL}) || len(ready1.FrozenCandidates) != 1 || ready1.FrozenCandidates[0].URL != configuredURL {
		t.Fatalf("first Ready included unverified community route: relays=%v frozen=%+v", ready1.Capability.Relays, ready1.FrozenCandidates)
	}

	// Even if the retired route's Ready flag flips afterward (e.g. a late race),
	// published link hints, FrozenCandidates, and RelayAvailability remain immutable.
	coldRoute.Ready.Store(true)
	ready2, err := current.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ready2.Capability.Relays, ready1.Capability.Relays) || !slices.Equal(ready2.FrozenCandidates, ready1.FrozenCandidates) {
		t.Fatalf("published Ready mutated after retirement: first=%+v second=%+v", ready1, ready2)
	}
	snap := current.Snapshot()
	if snap.Startup != ShareReadyReached || !slices.Equal(snap.Ready.Capability.Relays, []string{configuredURL}) {
		t.Fatalf("snapshot mutated published hints: %+v", snap.Ready.Capability.Relays)
	}
	if snap.RelayAvailability == nil || snap.RelayAvailability.Total != 1 || snap.RelayAvailability.Available != 1 {
		t.Fatalf("relay availability counted retired community route: %+v", snap.RelayAvailability)
	}
	if communityPrepared.Load() {
		t.Fatal("retired unverified community route constructed a SenderLifecycle")
	}
}
