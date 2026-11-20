package cli

import (
	"bytes"
	"context"
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

	"github.com/windshare/windshare/adapters/filesystem"

	"github.com/windshare/windshare/core/diagnostics"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/networkconfig"
	"github.com/windshare/windshare/engine/networkdirectory"
	"github.com/windshare/windshare/engine/relayprofile"
)

func TestPrepareShareSenderEmitsSourceCatalogStorageMilestonesWithoutSlog(t *testing.T) {
	tempRoot := t.TempDir()
	for _, name := range []string{"TEMP", "TMP", "TMPDIR"} {
		t.Setenv(name, tempRoot)
	}
	sharedPath := filepath.Join(tempRoot, "shared.txt")
	if err := os.WriteFile(sharedPath, []byte("catalog observability"), 0o600); err != nil {
		t.Fatal(err)
	}

	var legacyOutput bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&legacyOutput, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	stream, err := diagnostics.NewStream(256, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	application, err := engine.New(engine.Config{Share: engine.ShareDependencies{
		Relays: func(context.Context, []relayprofile.Registration) (engine.ShareRelays, error) {
			return nil, errors.New("test ends after source preparation")
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = application.Close(context.Background()) }()
	current, err := application.StartShare(context.Background(), engine.ShareRequest{Diagnostics: stream.Sink(),
		Source: filesystem.ShareSource([]string{sharedPath}), RelayIntent: relayprofile.UseExplicit([]relayprofile.Candidate{{PublicURL: "https://relay.example"}}), ChunkSize: catalog.MinChunkSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := current.Wait(context.Background())
	if err != nil || result.CleanupError != nil {
		t.Fatalf("cleanup = %v/%v", err, result.CleanupError)
	}
	if legacyOutput.Len() != 0 {
		t.Fatalf("catalog producer bypassed the injected diagnostic sink: %q", legacyOutput.String())
	}

	var operations []string
	stream.Close()
	for record := range stream.Records() {
		if record.Name() == "catalog_storage" {
			operations = append(operations, record.Payload().Get("operation").Text())
		}
	}
	wantOperations := []string{
		"creating", "cleaning", "cleaned", "created",
		"recovering", "recovered", "cleaning", "cleaned",
	}
	if !slices.Equal(operations, wantOperations) {
		t.Fatalf("catalog storage milestones = %v, want %v", operations, wantOperations)
	}
}

func TestShareTraceCapturesEngineDirectoryAndFrontendDiagnostics(t *testing.T) {
	var slogOutput bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&slogOutput, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	probed := make(chan struct{}, 2)
	frontend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case probed <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer frontend.Close()

	dir := t.TempDir()
	sharedPath := filepath.Join(dir, "shared.txt")
	if err := os.WriteFile(sharedPath, []byte("engine trace"), 0o600); err != nil {
		t.Fatal(err)
	}
	tracePath := filepath.Join(dir, "share.ndjson")
	app := &App{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		engineConfig: engine.Config{
			Network: &networkconfig.Config{
				Directory: networkdirectory.Config{Network: "test", Cache: &failingDirectoryCache{err: errors.New("corrupt cache")}},
				LinkHost:  frontend.URL,
			},
			Share: engine.ShareDependencies{
				Relays: func(context.Context, []relayprofile.Registration) (engine.ShareRelays, error) {
					<-probed
					return nil, errors.New("stop after startup probe")
				},
			},
		},
	}
	t.Cleanup(app.closeTerminalOutput)
	_ = app.Run(t.Context(), []string{"share", sharedPath, "--relay", "https://relay.example", "--front-url", frontend.URL, "--trace", tracePath})

	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	var sawDirInit, sawFrontend, sawPlan bool
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Event string            `json:"event"`
			Scope diagnostics.Scope `json:"scope"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		switch rec.Event {
		case "network_directory_initialization_failed":
			if rec.Scope.EngineID != "" && rec.Scope.TaskID == "" {
				sawDirInit = true
			}
		case "network_frontend_health":
			if rec.Scope.EngineID != "" && rec.Scope.TaskID == "" {
				sawFrontend = true
			}
		case "share_network_plan":
			if rec.Scope.EngineID != "" && rec.Scope.TaskID != "" {
				sawPlan = true
			}
		}
	}
	if !sawDirInit || !sawFrontend || !sawPlan {
		t.Fatalf("missing trace events (dir=%t frontend=%t plan=%t):\n%s", sawDirInit, sawFrontend, sawPlan, data)
	}
	if slogOutput.Len() != 0 {
		t.Fatalf("CLI share bypassed diagnostics sink via slog: %s", slogOutput.String())
	}
}

type failingDirectoryCache struct {
	networkdirectory.MemoryCache
	err error
}

func (c *failingDirectoryCache) Load(context.Context) (networkdirectory.Record, error) {
	return networkdirectory.Record{}, c.err
}
