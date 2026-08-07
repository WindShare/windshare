//go:build windows

package osfs

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
)

const (
	windowsV3PlacementDirectory = "guarded"
	windowsV3PlacementFile      = windowsV3PlacementDirectory + "/file.bin"
)

func TestWindowsV3ParentDisplacementCutsFailClosedAndRecoverInsideRoot(t *testing.T) {
	t.Skip("legacy frozen-selection restart publication is retired; incremental recovery requires FileCheckpointV1")
	t.Run("before begin", func(t *testing.T) {
		root := t.TempDir()
		selection := windowsV3PlacementSelection(t, 4)
		authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
		if err != nil {
			t.Fatal(err)
		}
		session, parentAdmission := windowsV3OperationOpen(t, authority, root, selection)
		parent := filepath.Join(root, windowsV3PlacementDirectory)
		displaced := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-before-begin-displaced")
		t.Cleanup(func() { _ = os.RemoveAll(displaced) })
		if err := os.Rename(parent, displaced); err != nil {
			t.Fatalf("move parent between admission and BeginFile: %v", err)
		}
		file := windowsV3OperationGuardFile(t, session, selection, parentAdmission)
		if start, err := session.BeginFile(context.Background(), file); err == nil {
			t.Fatalf("BeginFile after parent displacement = (start=%+v, err=%v), want pre-record failure", start, err)
		}
		if _, err := os.Stat(filepath.Join(displaced, "file.bin")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("outside final after rejected BeginFile: %v", err)
		}
		windowsV3OperationCloseSession(t, session)
		if err := os.Rename(displaced, parent); err != nil {
			t.Fatal(err)
		}
		reopenedAuthority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
		if err != nil {
			t.Fatal(err)
		}
		reopened, reopenedAdmission := windowsV3OperationOpen(t, reopenedAuthority, root, selection)
		start, err := reopened.BeginFile(context.Background(), windowsV3OperationGuardFile(t, reopened, selection, reopenedAdmission))
		if err != nil {
			t.Fatal(err)
		}
		transaction, durable, ok := start.Transaction()
		if !ok || len(durable.Ranges().Ranges()) != 0 {
			settlement, _ := start.ImmediateSettlement()
			t.Fatalf("reopen after rejected BeginFile = (transaction=%t ranges=%v settlement=%v), want empty transaction",
				ok, durable.Ranges().Ranges(), settlement.Kind())
		}
		if settlement, err := transaction.Pause(context.Background(), transfer.FilePauseOutputFailure); err != nil || settlement.Kind() != transfer.FilePaused {
			t.Fatalf("pause fresh transaction after rejected BeginFile = (kind=%v, err=%v)", settlement.Kind(), err)
		}
		windowsV3OperationCloseSession(t, reopened)
	})

	t.Run("after checkpoint and recovery publication", func(t *testing.T) {
		root := t.TempDir()
		selection := windowsV3PlacementSelection(t, 4)
		authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
		if err != nil {
			t.Fatal(err)
		}
		session, parentAdmission := windowsV3OperationOpen(t, authority, root, selection)
		file := windowsV3OperationGuardFile(t, session, selection, parentAdmission)
		transaction := windowsV3OperationBeginTransaction(t, session, file)
		if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Checkpoint(context.Background()); err != nil {
			t.Fatal(err)
		}
		parent := filepath.Join(root, windowsV3PlacementDirectory)
		displaced := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-checkpoint-displaced")
		t.Cleanup(func() { _ = os.RemoveAll(displaced) })
		if err := os.Rename(parent, displaced); err != nil {
			t.Fatalf("move parent after checkpoint: %v", err)
		}
		if settlement, err := transaction.Commit(context.Background()); err == nil || settlement.Kind() != 0 {
			t.Fatalf("commit through displaced parent = (kind=%v, err=%v), want retained failure", settlement.Kind(), err)
		}
		if _, err := os.Stat(filepath.Join(displaced, "file.bin")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("outside final after failed publication: %v", err)
		}
		paused, err := session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
		if err != nil || paused.Kind() != transfer.JobPaused {
			t.Fatalf("pause witnessed publication = (kind=%v, err=%v)", paused.Kind(), err)
		}
		if err := os.Rename(displaced, parent); err != nil {
			t.Fatalf("restore selected parent: %v", err)
		}

		reopenedAuthority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
		if err != nil {
			t.Fatal(err)
		}
		reopened, reopenedAdmission := windowsV3OperationOpen(t, reopenedAuthority, root, selection)
		recoveryFile := windowsV3OperationGuardFile(t, reopened, selection, reopenedAdmission)
		start, err := reopened.BeginFile(context.Background(), recoveryFile)
		if err != nil {
			t.Fatal(err)
		}
		settlement, immediate := start.ImmediateSettlement()
		if !immediate || settlement.Kind() != transfer.FilePublished {
			t.Fatalf("recovery publication = (kind=%v, immediate=%t), want published", settlement.Kind(), immediate)
		}
		windowsV3RequirePlacementResult(t, root, displaced, []byte("data"))
		windowsV3OperationCloseSession(t, reopened)
	})
}

func windowsV3PlacementSelection(t *testing.T, size uint64) transfer.OutputSelection {
	t.Helper()
	share := windowsV3TestIdentity16[catalog.ShareInstance](1)
	root := windowsV3TestIdentity16[catalog.DirectoryID](2)
	rootGeneration := windowsV3TestIdentity16[catalog.DirectoryGeneration](3)
	parent := windowsV3TestIdentity16[catalog.DirectoryID](4)
	parentGeneration := windowsV3TestIdentity16[catalog.DirectoryGeneration](5)
	modified := windowsV3TestModifiedTime(t)
	plan, err := transfer.NewOutputSelection(
		share,
		root,
		rootGeneration,
		[]transfer.OutputSelectionDirectory{{
			Path: windowsV3PlacementDirectory, DirectoryID: parent,
			Generation: parentGeneration, ModifiedTime: modified,
		}},
		[]transfer.OutputSelectionFile{{
			Path: windowsV3PlacementFile, FileID: windowsV3TestIdentity16[catalog.FileID](6),
			ParentDirectoryID: parent, ParentGeneration: parentGeneration,
			ExpectedSize: size, ModifiedTime: modified,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewTerminalSelectionObservationV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func windowsV3RequirePlacementResult(t *testing.T, root, displaced string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(filepath.Join(root, windowsV3PlacementFile))
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("published bytes = %q, err=%v, want %q", actual, err, expected)
	}
	if _, err := os.Stat(displaced); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("displaced tree exists after guarded publication: %v", err)
	}
}
