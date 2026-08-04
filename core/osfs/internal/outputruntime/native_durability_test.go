package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func TestLongOutputV3NativePublicationDurability(t *testing.T) {
	requireDurableFilesystemScenario(t)
	root := newNativeRuntimeTestRoot(t)
	payload := []byte("native-durable-publication")
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	authority := v3RecoveryAuthority(t, root, nil)
	authority.platformFactory = openNativeOutputRuntimeTestPlatform
	opened := v3RecoveryOpen(t, authority, root, selection)
	file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file)

	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("native publication = (kind=%v, err=%v)", settlement.Kind(), err)
	}
	job, err := opened.Session.CompleteJob(context.Background(), transfer.JobSucceeded)
	if err != nil || job.Kind() != transfer.JobClosed {
		t.Fatalf("native session completion = (kind=%v, err=%v)", job.Kind(), err)
	}
	actual, err := os.ReadFile(filepath.Join(root, v3RecoveryFilePath))
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("native published bytes = %q, err=%v", actual, err)
	}
}

func newNativeRuntimeTestRoot(t *testing.T) string {
	t.Helper()
	fixture := newNativeRuntimeTestRootSpec(t)
	platform, err := openNativeOutputRuntimeTestPlatform(fixture.path, fixture.create)
	if errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Skipf("certified output filesystem unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.ProbeRecoverableFeatures(); err != nil {
		_ = platform.Close()
		t.Fatalf("probe certified output filesystem: %v", err)
	}
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}
	return fixture.path
}
