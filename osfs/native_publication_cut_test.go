//go:build windows || linux || darwin

package osfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
)

func pausedPortableFile(t *testing.T, size uint64) (string, transfer.ReceiveIntent) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	authority, operation, intent := openPublicOrdinaryOperation(t, root, 0x61)
	session, err := authority.OpenOperation(ctx, operation)
	if err != nil {
		t.Fatal(err)
	}
	admission := admitRetainedCandidateRoot(t, session, intent, 0x62)
	file := nativeDirectTreeTestFile(t, session, intent, 0x63, "file.bin", size, admission)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		t.Fatal("file did not start")
	}
	if err := transaction.WriteRange(ctx, 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Pause(ctx, transfer.FilePauseShutdown); err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(ctx, transfer.JobPauseShutdown); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	return root, intent
}

func onlyPortableStage(t *testing.T, root string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && filepath.Ext(path) == ".stage" {
			if found != "" {
				t.Fatal("multiple staged data objects")
			}
			found = path
		}
		return nil
	})
	if err != nil || found == "" {
		t.Fatalf("missing stage: %s %v", found, err)
	}
	return found
}

func TestPortablePublicationCrashReconcilesWithoutWritableStage(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		t.Run(map[bool]string{false: "published before receipt", true: "replacement before receipt"}[replaced], func(t *testing.T) {
			ctx := context.Background()
			root, intent := pausedPortableFile(t, 4)
			repository, policy, closeRepository := openRetainedCandidateRepository(t, root, intent)
			store, err := checkpointstore.NewFileExecutionStoreWithStagePolicy(repository, policy)
			if err != nil {
				t.Fatal(err)
			}
			records, attention := store.Snapshot()
			if len(records) != 1 || len(attention) != 0 {
				t.Fatal("missing checkpoint")
			}
			previous := records[0]
			publishing, err := checkpointmodel.AdvanceState(previous, previous.StateGeneration()+1, checkpointmodel.PhasePublishing, checkpointmodel.CommitVerified, 0, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repository.Replace(previous, publishing); err != nil {
				t.Fatal(err)
			}
			closeRepository()
			final := filepath.Join(nativeDirectTreeResultRoot(t, root, intent), "file.bin")
			if err := os.Rename(onlyPortableStage(t, root), final); err != nil {
				t.Fatal(err)
			}
			if replaced {
				if err := os.Rename(final, final+".moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(final, []byte("user"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
			if err != nil {
				t.Fatal(err)
			}
			defer authority.Close()
			if _, err := authority.BindDestination(ctx); err != nil {
				t.Fatal(err)
			}
			lookup, err := authority.LookupActive(ctx, intent.SelectionSpec())
			if err != nil {
				t.Fatal(err)
			}
			session, err := authority.OpenOperation(ctx, lookup.Operation())
			if err != nil {
				t.Fatal(err)
			}
			admission := admitRetainedCandidateRoot(t, session, intent, 0x62)
			file := nativeDirectTreeTestFile(t, session, intent, 0x63, "file.bin", 4, admission)
			start, err := session.BeginFile(ctx, file)
			if err != nil {
				t.Fatal(err)
			}
			settlement, ok := start.ImmediateSettlement()
			if !ok {
				t.Fatal("crash reconciliation started another writable copy")
			}
			want := transfer.FilePreviouslyPublished
			if replaced {
				want = transfer.FileCollision
			}
			if settlement.Kind() != want {
				t.Fatalf("settlement %v, want %v", settlement.Kind(), want)
			}
			_, found, err := session.(transfer.DeliveryHistory).LookupDelivery(ctx, file.Descriptor().FileID(), file.MaterializationRelativePath().String())
			if err != nil || found == replaced {
				t.Fatalf("receipt did not match publication evidence: %v %v", found, err)
			}
			wantData := "data"
			if replaced {
				wantData = "user"
			}
			if actual, err := os.ReadFile(final); err != nil || string(actual) != wantData {
				t.Fatalf("public bytes changed: %q %v", actual, err)
			}
			if _, err := session.PauseTree(ctx, transfer.JobPauseShutdown); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPortableDiscardPreservesReplacedPartial(t *testing.T) {
	ctx := context.Background()
	root, intent := pausedPortableFile(t, 8)
	stage := onlyPortableStage(t, root)
	if err := os.Rename(stage, stage+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stage, []byte("external"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovery, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := recovery.Discard(ctx, intent.OperationID())
	if err == nil && result.State() == ResumeOperationDiscarded {
		t.Fatal("uncertain partial was declared discarded")
	}
	if actual, err := os.ReadFile(stage); err != nil || string(actual) != "external" {
		t.Fatalf("discard removed a replacement: %q %v", actual, err)
	}
}
