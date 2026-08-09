//go:build windows || linux

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/transfer"
)

func TestReserveGetOutputOperationDeterministicallyReopensOneCompatibleOperation(t *testing.T) {
	ctx := context.Background()
	rootPath := newCLICertifiedOutputTestRoot(t)
	openAuthority := func() *osfs.FilesystemOutputAuthority {
		authority, err := osfs.NewFilesystemOutputAuthority(osfs.FilesystemOutputAuthorityConfig{
			RootPath: rootPath, CreateRoot: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return authority
	}
	var share catalog.ShareInstance
	share[0] = 1
	var root catalog.DirectoryID
	root[0] = 2
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}

	first, firstKind, err := reserveGetOutputOperation(ctx, openAuthority(), selection)
	if err != nil {
		t.Fatal(err)
	}
	if firstKind != osfs.NativeDirectTreeReserved {
		t.Fatalf("first reservation kind=%d", firstKind)
	}
	second, secondKind, err := reserveGetOutputOperation(ctx, openAuthority(), selection)
	if err != nil {
		t.Fatal(err)
	}
	if secondKind != osfs.NativeDirectTreeReopened || !second.EqualCanonical(first) {
		t.Fatalf("repeat reservation kind=%d same_intent=%t", secondKind, second.EqualCanonical(first))
	}
	active, err := openAuthority().OpenDirectTree(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	paused := false
	t.Cleanup(func() {
		if !paused {
			_, _ = active.PauseTree(context.Background(), transfer.JobPauseInterrupted)
		}
	})
	uncertain, uncertainKind, err := reserveGetOutputOperation(ctx, openAuthority(), selection)
	if !errors.Is(err, errGetOutputOperationNeedsAttention) ||
		uncertainKind != osfs.NativeDirectTreeNeedsAttention || !uncertain.IsZero() {
		t.Fatalf("uncertain reservation=(kind=%d intent_zero=%t err=%v)", uncertainKind, uncertain.IsZero(), err)
	}
	if _, err := active.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	paused = true

	differentRules, err := transfer.NewPathSelectionRules([]string{"different.txt"})
	if err != nil {
		t.Fatal(err)
	}
	differentSelection, err := transfer.NewSelectionSpec(share, root, differentRules)
	if err != nil {
		t.Fatal(err)
	}
	different, differentKind, err := reserveGetOutputOperation(ctx, openAuthority(), differentSelection)
	if err != nil {
		t.Fatal(err)
	}
	if differentKind != osfs.NativeDirectTreeReserved || different.OperationID() == first.OperationID() {
		t.Fatalf("zero-compatible-match reservation kind=%d operation_reused=%t", differentKind, different.OperationID() == first.OperationID())
	}
	reopened, reopenedKind, err := reserveGetOutputOperation(ctx, openAuthority(), selection)
	if err != nil {
		t.Fatal(err)
	}
	if reopenedKind != osfs.NativeDirectTreeReopened || reopened.OperationID() != first.OperationID() {
		t.Fatalf("compatible lookup selected kind=%d operation=%x want=%x", reopenedKind, reopened.OperationID().Bytes(), first.OperationID().Bytes())
	}

	firstJob, err := transfer.NewTransferJobID()
	if err != nil {
		t.Fatal(err)
	}
	secondJob, err := transfer.NewTransferJobID()
	if err != nil {
		t.Fatal(err)
	}
	if firstJob == secondJob || bytes.Equal(first.OperationID().Bytes(), firstJob.Bytes()) ||
		bytes.Equal(first.OperationID().Bytes(), secondJob.Bytes()) {
		t.Fatal("stable operation identity was reused as per-run transfer job identity")
	}
}

func newCLICertifiedOutputTestRoot(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := os.MkdirTemp(home, ".windshare-cli-reopen-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(reserved); err != nil {
			t.Errorf("remove certified CLI output test root: %v", err)
		}
	})
	return reserved
}

func TestReserveGetOutputOperationRejectsMissingAuthorityOrSelection(t *testing.T) {
	if _, _, err := reserveGetOutputOperation(context.Background(), nil, transfer.SelectionSpec{}); !errors.Is(err, errGetOutputReservationContract) {
		t.Fatalf("error=%v", err)
	}
}
