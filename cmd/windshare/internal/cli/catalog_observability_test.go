package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestPrepareShareSenderEmitsStructuredCatalogStorageMilestones(t *testing.T) {
	tempRoot := t.TempDir()
	for _, name := range []string{"TEMP", "TMP", "TMPDIR"} {
		t.Setenv(name, tempRoot)
	}
	sharedPath := filepath.Join(tempRoot, "shared.txt")
	if err := os.WriteFile(sharedPath, []byte("catalog observability"), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	app := &App{Stderr: io.Discard}
	prepared, code := app.prepareShareSender(context.Background(), shareRequest{
		paths: []string{sharedPath}, relayURL: DefaultRelayURL, chunkSize: catalog.MinChunkSize,
	})
	if code != ExitOK {
		t.Fatalf("prepare share sender exit = %d", code)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}

	decoder := json.NewDecoder(&output)
	var records []map[string]any
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "share: catalog storage" {
			records = append(records, record)
		}
	}

	wantOperations := []string{
		"creating", "cleaning", "cleaned", "created",
		"recovering", "recovered", "cleaning", "cleaned",
	}
	operations := make([]string, 0, len(records))
	var shareInstance string
	for _, record := range records {
		operation, ok := record["operation"].(string)
		if !ok {
			t.Fatalf("catalog milestone has no operation: %#v", record)
		}
		operations = append(operations, operation)
		identity, ok := record["share_instance"].(string)
		if !ok || identity == "" {
			t.Fatalf("catalog milestone has no stable share identity: %#v", record)
		}
		if shareInstance == "" {
			shareInstance = identity
		} else if identity != shareInstance {
			t.Fatalf("catalog milestone identity changed from %q to %q", shareInstance, identity)
		}
		for _, key := range []string{
			"recovered_entries", "recovered_memory_bytes", "recovered_spill_bytes",
			"legacy_roots_removed", "failed", "cause",
		} {
			if _, ok := record[key]; !ok {
				t.Fatalf("catalog milestone lost %q decision context: %#v", key, record)
			}
		}
	}
	if !slices.Equal(operations, wantOperations) {
		t.Fatalf("catalog storage milestones = %v, want %v", operations, wantOperations)
	}
}
