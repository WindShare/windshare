package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/diagnostics"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/engine"
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
		Relays: func(context.Context, []relayprofile.Candidate) (engine.ShareRelays, error) {
			return nil, errors.New("test ends after source preparation")
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = application.Close(context.Background()) }()
	current, err := application.StartShare(context.Background(), engine.ShareRequest{Diagnostics: stream.Sink(),
		Source: shareFileSource([]string{sharedPath}), Relays: []engine.ShareRelayCandidate{{PublicURL: "https://relay.example"}}, ChunkSize: catalog.MinChunkSize,
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
