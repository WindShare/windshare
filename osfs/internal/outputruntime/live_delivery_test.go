package outputruntime

import (
	"context"
	"os"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestLiveDeliveryReceiptPreservesUserReplacementAcrossAttempts(t *testing.T) {
	for _, crashCleanup := range []bool{false, true} {
		name := "process-only"
		if crashCleanup {
			name = "cleanup-journal"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			fixture := openLiveTransactionWithPlatform(t, newRuntimeTestRootSpec(t).path, 0x61, false, func(base outputcap.Platform) outputcap.Platform {
				if crashCleanup {
					return &liveOnlyRuntimePlatform{Platform: base}
				}
				return &processOnlyRuntimePlatform{Platform: base}
			})
			first, _, _ := fixture.start.Transaction()
			if err := first.WriteRange(ctx, 0, []byte("data")); err != nil {
				t.Fatal(err)
			}
			if settlement, err := first.Commit(ctx); err != nil || settlement.Kind() != transfer.FilePublished {
				t.Fatalf("publication: %v %v", settlement.Kind(), err)
			}
			// Moving the delivered object and replacing its name must neither
			// resurrect its write authority nor delete the user's replacement.
			moved := fixture.finalPath + ".moved"
			if err := os.Rename(fixture.finalPath, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixture.finalPath, []byte("user replacement with different size"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
				t.Fatal(err)
			}
			operation := fixture.authority.operation
			session, err := fixture.authority.OpenOperation(ctx, operation)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = session.PauseTree(ctx, transfer.JobPauseInterrupted) })
			source := transfer.AuthenticatedSourceDirectory{
				DirectoryID: operation.intent.SelectionSpec().SyntheticRoot(),
				Generation:  incrementalTestIdentity16[catalog.DirectoryGeneration](0x64),
				SourcePath:  ordinaryoutput.EmptySourceCatalogPath(),
			}
			file := ordinaryResumeMaterializationFile(t, session, operation.intent, first.Binding().Descriptor(),
				"live.bin", source, transfer.DirectoryAdmission{}, transfer.MaterializedDirectoryClaim{})
			start, err := session.BeginFile(ctx, file)
			settlement, settled := start.ImmediateSettlement()
			if err != nil || !settled || settlement.Kind() != transfer.FilePreviouslyPublished {
				t.Fatalf("delivery receipt became a new write or collision: %v %v", settlement.Kind(), err)
			}
			if _, verified := settlement.VerifiedProgress(); verified {
				t.Fatal("historical delivery claimed current byte authentication")
			}
			if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
				t.Fatal(err)
			}
			if err := fixture.authority.Close(); err != nil {
				t.Fatal(err)
			}
			for path, expected := range map[string]string{fixture.finalPath: "user replacement with different size", moved: "data"} {
				actual, err := os.ReadFile(path)
				if err != nil || string(actual) != expected {
					t.Fatalf("delivery cleanup changed user output: %q %v", actual, err)
				}
			}
		})
	}
}
