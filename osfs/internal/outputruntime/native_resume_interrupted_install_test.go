package outputruntime

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func TestNativeResumeIncompleteInstallationRetainsVerifiedProgress(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	fixture := openOrdinaryResumeSession(t, root, 0x65, 4)
	ctx := context.Background()
	if err := fixture.transaction.WriteRange(ctx, 0, []byte("ab")); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.transaction.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	pauseOrdinaryResumeFixture(t, fixture)

	var candidate string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(entry.Name(), ".checkpoint") {
			candidate = filepath.Join(filepath.Dir(path),
				checkpointstore.TemporaryName(entry.Name(), []byte("interrupted-next-image"), 0))
			return fs.SkipAll
		}
		return nil
	}); err != nil || candidate == "" {
		t.Fatalf("locate checkpoint: %v", err)
	}
	if err := os.WriteFile(candidate, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform, nil)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := repository.Acquire(ctx, fixture.intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := lease.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := resumeauthority.ReduceRecovery(snapshot)
	if err != nil || summary.State() == resumeauthority.OperationNeedsAttention ||
		len(snapshot.Items()) != 1 || snapshot.Items()[0].State() != resumeauthority.ItemResumable {
		t.Fatalf("incomplete image blocked recovery: %+v, %v", summary, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := reopenOrdinaryResumeFile(t, root, 0x65, 4)
	if restarted.intent.OperationID() != fixture.intent.OperationID() || restarted.durable.Ranges().Len() != 1 || restarted.durable.Ranges().Ranges()[0].End != 2 {
		t.Fatal("restart lost operation or verified ranges")
	}
	if err := restarted.transaction.WriteRange(ctx, 2, []byte("cd")); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.transaction.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if settlement, err := restarted.transaction.Commit(ctx); err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("publication after interrupted installation: %+v, %v", settlement, err)
	}
	if _, err := restarted.session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := restarted.authority.Close(); err != nil {
		t.Fatal(err)
	}
	if actual, err := os.ReadFile(restarted.finalPath); err != nil || string(actual) != "abcd" {
		t.Fatalf("recovered content = %q, %v", actual, err)
	}
	if info, err := os.Stat(candidate); err != nil || info.Size() != 0 {
		t.Fatalf("unknown installation image was removed or overwritten: %v", err)
	}
}
