package outputruntime

import (
	"context"
	"os"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func TestLiveOperationRetainsRangesAcrossPauseAndPublicationCollision(t *testing.T) {
	for _, collision := range []bool{false, true} {
		name := "pause"
		if collision {
			name = "collision"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			fixture := openLiveTransactionWithPlatform(t, newRuntimeTestRootSpec(t).path, 0x71, false, func(base outputcap.Platform) outputcap.Platform {
				return &processOnlyRuntimePlatform{Platform: base}
			})
			operation := fixture.authority.operation
			first, _, _ := fixture.start.Transaction()
			end := uint64(2)
			if collision {
				end = 4
			}
			if err := first.WriteRange(ctx, 0, []byte("data")[:end]); err != nil {
				t.Fatal(err)
			}
			if collision {
				if err := os.WriteFile(fixture.finalPath, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
				settled, err := first.Commit(ctx)
				if err != nil || settled.Kind() != transfer.FileCollision {
					t.Fatalf("collision: %v %v", settled.Kind(), err)
				}
			} else if _, err := first.Pause(ctx, transfer.FilePauseInterrupted); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
				t.Fatal(err)
			}
			if !operation.RetryAvailable() {
				t.Fatal("settled attempt lost operation authority")
			}
			if collision {
				if bytes, err := os.ReadFile(fixture.finalPath); err != nil || string(bytes) != "foreign" {
					t.Fatal("collision replaced foreign output")
				}
				if err := os.Remove(fixture.finalPath); err != nil {
					t.Fatal(err)
				}
			}
			session, err := fixture.authority.OpenOperation(ctx, operation)
			if err != nil {
				t.Fatal(err)
			}
			defer session.PauseTree(ctx, transfer.JobPauseInterrupted)
			source := transfer.AuthenticatedSourceDirectory{
				DirectoryID: operation.intent.SelectionSpec().SyntheticRoot(),
				Generation:  incrementalTestIdentity16[catalog.DirectoryGeneration](0x74),
				SourcePath:  ordinaryoutput.EmptySourceCatalogPath(),
			}
			file := ordinaryResumeMaterializationFile(t, session, operation.intent, first.Binding().Descriptor(),
				"live.bin", source, transfer.DirectoryAdmission{}, transfer.MaterializedDirectoryClaim{})
			start, err := session.BeginFile(ctx, file)
			if err != nil {
				t.Fatal(err)
			}
			next, progress, ok := start.Transaction()
			if !ok || progress.Retention() != transfer.RangesInProcess || progress.Ranges().Len() != 1 ||
				progress.Ranges().Ranges()[0].End != end || next.Binding().ObjectIdentity() != first.Binding().ObjectIdentity() {
				t.Fatalf("retry replaced retained object or ranges: %+v", progress)
			}
			if err := first.WriteRange(ctx, 2, []byte("ta")); err == nil {
				t.Fatal("old attempt remained writable")
			}
			if !collision {
				if err := next.WriteRange(ctx, 2, []byte("ta")); err != nil {
					t.Fatal(err)
				}
			}
			if settled, err := next.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
				t.Fatalf("retry publish: %v %v", settled.Kind(), err)
			}
			if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(fixture.finalPath); err != nil || string(data) != "data" {
				t.Fatalf("final %q %v", data, err)
			}
		})
	}
}

func TestCapabilityLossKeepsExistingOperationVisibleAndCannotCreateDuplicate(t *testing.T) {
	ctx := context.Background()
	root := newRuntimeTestRootSpec(t).path
	original := openOrdinaryResumeSession(t, root, 0x81, 4)
	if err := original.transaction.WriteRange(ctx, 0, []byte("da")); err != nil {
		t.Fatal(err)
	}
	if _, err := original.transaction.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := original.session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := original.authority.Close(); err != nil {
		t.Fatal(err)
	}
	factory := func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return &processOnlyRuntimePlatform{Platform: platform}, nil
	}
	authority, err := New(Config{RootPath: root, PlatformFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	if mode, err := authority.BindDestination(ctx); err != nil || !mode.LiveOnly() {
		t.Fatalf("downgrade %v %v", mode, err)
	}
	lookup, err := authority.LookupActive(ctx, original.intent.SelectionSpec())
	if err != nil || lookup.Kind() != ActiveLookupNeedsAttention || lookup.StateReason() != FilesystemOutputStateRecoveryUnavailable {
		t.Fatalf("capability loss became fresh miss: %v %v", lookup.Kind(), err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	repository, err := NewNativeResumeRepository(root, factory, nil)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := resumeauthority.New(repository)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := inventory.List(ctx)
	if err != nil || len(listed.Summaries()) != 1 || listed.Summaries()[0].State() != resumeauthority.OperationRecoveryUnavailable {
		t.Fatalf("blocked inventory: %+v %v", listed.Summaries(), err)
	}
	reopened := reopenOrdinaryResumeFile(t, root, 0x81, 4)
	defer reopened.authority.Close()
	defer reopened.session.PauseTree(ctx, transfer.JobPauseInterrupted)
	if reopened.transaction == nil || reopened.durable.Ranges().Len() != 1 || reopened.durable.Ranges().Ranges()[0].End != 2 {
		t.Fatalf("downgrade lost progress: %+v", reopened.durable)
	}
}
