//go:build windows || linux

package resumecommand

import (
	"context"
	"encoding/hex"
	"os"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestFilesystemResumeInventoryOpensAbsentCertifiedRootWithoutState(t *testing.T) {
	root := newResumeCertifiedOutputTestRoot(t)
	if _, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{
		RootPath: root, CreateRoot: true,
	}); err != nil {
		t.Fatal(err)
	}
	inventory, err := (filesystemResumeStateInventoryOpener{}).OpenResumeStateInventory(
		context.Background(), root,
	)
	if err != nil {
		t.Fatal(err)
	}
	items, err := inventory.Items()
	if err != nil || len(items) != 0 {
		t.Fatalf("empty native inventory=(%+v, %v)", items, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("read-only inventory created state: entries=%v err=%v", entries, err)
	}
}

func TestFilesystemResumeInventoryProjectsAndDiscardsByStableOperationID(t *testing.T) {
	ctx := context.Background()
	root := newResumeCertifiedOutputTestRoot(t)
	output, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{
		RootPath: root, CreateRoot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var share catalog.ShareInstance
	share[0] = 1
	var syntheticRoot catalog.DirectoryID
	syntheticRoot[0] = 2
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(share, syntheticRoot, rules)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := output.ReserveDirectTree(
		ctx, selection, receivecontract.NewCatalogRootDirectoryTree(),
	)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := reservation.ReceiveIntent()
	if !ok {
		t.Fatalf("reservation kind=%d", reservation.Kind())
	}

	inventory, err := (filesystemResumeStateInventoryOpener{}).OpenResumeStateInventory(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	items, err := inventory.Items()
	if err != nil || len(items) != 1 {
		t.Fatalf("items=(%+v, %v)", items, err)
	}
	wantOperation := hex.EncodeToString(intent.OperationID().Bytes())
	if items[0].operationID != wantOperation || items[0].intentDigest != hex.EncodeToString(intent.Digest().Bytes()) ||
		items[0].status != resumeItemStatusRecorded || !items[0].discardable || items[0].resumable {
		t.Fatalf("projected item=%+v", items[0])
	}
	report, err := inventory.Discard(ctx, 0)
	if err != nil || report.status != resumeDiscardStatusSettled ||
		report.operationID != wantOperation || report.phase == 0 || report.stateGeneration <= items[0].stateGeneration {
		t.Fatalf("discard report=(%+v, %v)", report, err)
	}
}

func newResumeCertifiedOutputTestRoot(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := os.MkdirTemp(home, ".windshare-resume-command-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(reserved); err != nil {
			t.Errorf("remove certified resume command test root: %v", err)
		}
	})
	return reserved
}
