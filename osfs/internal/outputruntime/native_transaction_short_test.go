package outputruntime

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func TestPublishedOrdinaryFileRestoresDeliveryReceiptAndPreservesUserEdits(t *testing.T) {
	for _, payload := range []string{"data", "EDIT"} {
		t.Run(payload, func(t *testing.T) {
			root := newRuntimeTestRootSpec(t).path
			first := openOrdinaryResumeSession(t, root, 0xb1, 4)
			if err := first.transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
				t.Fatal(err)
			}
			if _, err := first.transaction.Checkpoint(context.Background()); err != nil {
				t.Fatal(err)
			}
			if settlement, err := first.transaction.Commit(context.Background()); err != nil ||
				settlement.Kind() != transfer.FilePublished {
				t.Fatalf("first commit = (%d, %v)", settlement.Kind(), err)
			}
			if _, err := first.session.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
				t.Fatal(err)
			}
			if err := first.authority.Close(); err != nil {
				t.Fatal(err)
			}

			// WriteAt preserves the native file object and length, reproducing the
			// ambiguity that an ownership witness cannot resolve into a content proof.
			before, err := os.Stat(first.finalPath)
			if err != nil {
				t.Fatal(err)
			}
			if payload != "data" {
				file, err := os.OpenFile(first.finalPath, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := file.WriteAt([]byte(payload), 0)
				if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
					t.Fatal(err)
				}
			}
			after, err := os.Stat(first.finalPath)
			if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() {
				t.Fatalf("edit changed the file witness: %v", err)
			}
			reopened := reopenOrdinaryResumeFile(t, root, 0xb1, 4)
			if reopened.transaction != nil || reopened.settlement.Kind() != transfer.FilePreviouslyPublished {
				t.Fatalf("reopened published file = transaction %T settlement %d", reopened.transaction, reopened.settlement.Kind())
			}
			if _, verified := reopened.settlement.VerifiedProgress(); verified {
				t.Fatal("historical delivery exposed a verified checkpoint")
			}
			if _, transferred := reopened.settlement.PublicationProvenance(); transferred {
				t.Fatal("historical delivery was reported as downloaded or resumed")
			}
			tree, err := reopened.session.FinalizeTree(context.Background(), transfer.DirectTreeOutcomeSuccess)
			if err != nil || tree.Kind() != transfer.DirectTreeSettlementSuccess {
				t.Fatalf("reopened tree = (%d, %v)", tree.Kind(), err)
			}
			if err := reopened.authority.Close(); err != nil {
				t.Fatal(err)
			}
			repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform, nil)
			if err != nil {
				t.Fatal(err)
			}
			page, err := repository.Page(context.Background(), resumeauthority.PageCursor{}, 8)
			if err != nil || len(page.Headers()) != 0 {
				t.Fatalf("completed operation inventory = (%d, %v)", len(page.Headers()), err)
			}
			if data, err := os.ReadFile(reopened.finalPath); err != nil || string(data) != payload {
				t.Fatalf("published sibling = (%q, %v)", data, err)
			}
			if _, err := os.Stat(filepath.Join(root, checkpointstore.ControlDirectory)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("successful resumable transfer retained private control state: %v", err)
			}
		})
	}
}
