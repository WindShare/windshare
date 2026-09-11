package outputruntime

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

type processOnlyRuntimePlatform struct{ outputcap.Platform }

func (*processOnlyRuntimePlatform) DestinationCapabilities() (outputcap.DestinationCapabilities, error) {
	unsupported, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnverifiableCrashCleanup)
	return outputcap.NewDestinationCapabilities(outputcap.SupportedCapability(), unsupported, unsupported, unsupported)
}
func (*processOnlyRuntimePlatform) LiveCleanupNativeProfile() checkpointmodel.LiveCleanupNativeProfile {
	panic("process-only runtime requested restart profile")
}
func (*processOnlyRuntimePlatform) Certification() outputcap.CertificationID {
	panic("process-only runtime requested restart certification")
}
func (*processOnlyRuntimePlatform) RootBinding() (outputcap.OutputRootBinding, error) {
	panic("process-only runtime requested restart binding")
}

func TestProcessOnlyTransactionPublishesAndPreservesStalePrivateState(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	stale := filepath.Join(root, ".windshare-output")
	if err := os.Mkdir(stale, 0700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(stale, "foreign")
	if err := os.WriteFile(foreign, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	fixture := openLiveTransactionWithPlatform(t, root, 0xB1, false, func(base outputcap.Platform) outputcap.Platform {
		return &processOnlyRuntimePlatform{Platform: base}
	})
	transaction, durable, ok := fixture.start.Transaction()
	if !ok {
		t.Fatal("process file did not begin a transaction")
	}
	if fixture.authority.registry != nil {
		t.Fatal("process binding created a registry")
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := transaction.Checkpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if durable.Ranges().Len() != 0 || checkpoint.Ranges().Len() != 0 {
		t.Fatal("process ranges became restart-authoritative")
	}
	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("commit=%v %v", settlement.Kind(), err)
	}
	if _, err := fixture.session.FinalizeTree(context.Background(), transfer.DirectTreeOutcomeSuccess); err != nil {
		t.Fatal(err)
	}
	if err := fixture.authority.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(fixture.finalPath); err != nil || string(data) != "data" {
		t.Fatalf("final=%q %v", data, err)
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "untouched" {
		t.Fatalf("stale=%q %v", data, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".windshare-live-") {
			t.Fatalf("owned process stage leaked: %s", entry.Name())
		}
	}
}

func TestProcessOnlyTransactionPauseDeletesOnlyCurrentStage(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	fixture := openLiveTransactionWithPlatform(t, root, 0xC1, false, func(base outputcap.Platform) outputcap.Platform {
		return &processOnlyRuntimePlatform{Platform: base}
	})
	transaction, _, ok := fixture.start.Transaction()
	if !ok {
		t.Fatal("process file did not begin a transaction")
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("da")); err != nil {
		t.Fatal(err)
	}
	settlement, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted)
	if err != nil || settlement.Kind() != transfer.FilePaused {
		t.Fatalf("pause=%v %v", settlement.Kind(), err)
	}
	if _, err := fixture.session.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := fixture.authority.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture.finalPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("final=%v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("remaining entries=%v %v", entries, err)
	}
}
