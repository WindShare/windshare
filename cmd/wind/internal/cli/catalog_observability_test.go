package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/catalog"
)

func TestPrepareShareSenderEmitsTypedCatalogStorageMilestonesWithoutSlog(t *testing.T) {
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
	emitter := &shareRecordingEmitter{}
	observations := newShareObservations(emitter)
	prepared, code := new(App).prepareShareSender(
		context.Background(),
		shareRequest{
			paths: []string{sharedPath}, relayURL: DefaultRelayURL, chunkSize: catalog.MinChunkSize,
		},
		newSystemCommandClock(time.Now),
		emitter,
		observations,
	)
	if code != ExitOK {
		t.Fatalf("prepare share sender exit = %d", code)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if legacyOutput.Len() != 0 {
		t.Fatalf("catalog producer bypassed the typed CLI boundary: %q", legacyOutput.String())
	}

	var operations []string
	for _, event := range emitter.events {
		catalogEvent, ok := event.(clievent.CatalogStorageObserved)
		if !ok {
			continue
		}
		name, ok := catalogEvent.Operation().Name()
		if !ok {
			t.Fatalf("invalid catalog operation: %#v", catalogEvent)
		}
		operations = append(operations, name)
	}
	wantOperations := []string{
		"creating", "cleaning", "cleaned", "created",
		"recovering", "recovered", "cleaning", "cleaned",
	}
	if !slices.Equal(operations, wantOperations) {
		t.Fatalf("catalog storage milestones = %v, want %v", operations, wantOperations)
	}
}
