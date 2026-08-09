//go:build windows || linux

package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/transfer"
)

func TestFilesystemResumeStateAuthorityListsAbsentV2WithoutCreatingControlState(t *testing.T) {
	root := t.TempDir()
	authority, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.ListResumeState(context.Background())
	if err != nil || inventory.Status() != ResumeStateListReady ||
		len(inventory.Summaries()) != 0 || len(inventory.Attention()) != 0 {
		t.Fatalf("absent inventory = (%+v, %v)", inventory, err)
	}
	_, err = os.Lstat(filepath.Join(root, checkpointstore.ControlDirectory))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only resume list created control state: %v", err)
	}
	if _, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: "relative"}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("relative resume root error = %v", err)
	}
}

func TestFilesystemResumeStateAuthorityListsAndDiscardsOwnedZeroFileOperation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "owned-output")
	output, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath: root, CreateRoot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	intent := coverageC6FilesystemIntent(t, output, 0xc1)
	authority, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.ListResumeState(context.Background())
	if err != nil || inventory.Status() != ResumeStateListReady || len(inventory.Summaries()) != 1 {
		t.Fatalf("owned inventory = (%+v, %v)", inventory, err)
	}
	listed := inventory.Summaries()[0]
	if listed.OperationID() != intent.OperationID() ||
		listed.Phase() != uint8(checkpointmodel.LifecycleIntentFrozen) {
		t.Fatalf("owned summary = %+v", listed)
	}
	discarded, err := authority.Discard(context.Background(), intent.OperationID())
	if err != nil || discarded.Phase() != uint8(checkpointmodel.LifecycleDiscarded) ||
		discarded.OperationID() != intent.OperationID() {
		t.Fatalf("zero-file discard = (%+v, %v)", discarded, err)
	}
	if names, err := os.ReadDir(root); err != nil || len(names) != 1 || names[0].Name() != checkpointstore.ControlDirectory {
		t.Fatalf("discard changed public output = (%v, %v)", names, err)
	}
}

func TestFilesystemResumeStateAuthorityExposesStableBusyError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "busy-output")
	output, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath: root, CreateRoot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	intent := coverageC6FilesystemIntent(t, output, 0xd1)
	session, err := output.OpenDirectTree(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			_, _ = session.PauseTree(context.Background(), transfer.JobPauseInterrupted)
		}
	})
	authority, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Discard(context.Background(), intent.OperationID()); !errors.Is(err, ErrResumeStateBusy) {
		t.Fatalf("busy discard error = %v", err)
	}
	if _, err := session.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	released = true
	recovered, err := authority.Recover(context.Background(), intent.OperationID(), 1)
	if err != nil || !recovered.Resumable() || recovered.OperationID() != intent.OperationID() {
		t.Fatalf("reacquired resume state = (%+v, %v)", recovered, err)
	}
}

func TestFilesystemResumeStateAuthorityRemovesOnlyOwnedEmptyDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "directory-cleanup")
	output, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath: root, CreateRoot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	intent := coverageC6FilesystemIntent(t, output, 0xdb)
	session, err := output.OpenDirectTree(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	rootAdmission, err := session.AdmitDirectory(context.Background(), transfer.MaterializationDirectory{
		DirectoryID: intent.SyntheticRoot(),
		Generation:  coverageC6Identity[catalog.DirectoryGeneration](0xdc),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.AdmitDirectory(context.Background(), transfer.MaterializationDirectory{
		DirectoryID:     coverageC6Identity[catalog.DirectoryID](0xdd),
		Generation:      coverageC6Identity[catalog.DirectoryGeneration](0xde),
		ParentAdmission: rootAdmission,
		Path:            "empty",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.AdmitDirectory(context.Background(), transfer.MaterializationDirectory{
		DirectoryID:     coverageC6Identity[catalog.DirectoryID](0xdf),
		Generation:      coverageC6Identity[catalog.DirectoryGeneration](0xe0),
		ParentAdmission: rootAdmission,
		Path:            "kept",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	keptPath := filepath.Join(root, "kept", "caller-owned.txt")
	if err := os.WriteFile(keptPath, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root, "empty")); err != nil || !info.IsDir() {
		t.Fatalf("owned empty directory = (%v, %v)", info, err)
	}
	authority, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	discarded, err := authority.Discard(context.Background(), intent.OperationID())
	if err != nil || discarded.Phase() != uint8(checkpointmodel.LifecycleDiscarded) {
		t.Fatalf("directory discard = (%+v, %v)", discarded, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "empty")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned empty directory survived discard: %v", err)
	}
	if kept, err := os.ReadFile(keptPath); err != nil || string(kept) != "preserve" {
		t.Fatalf("non-empty owned directory was mutated: (%q, %v)", kept, err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("caller root was removed: %v", err)
	}
}

func TestFilesystemResumeStateAuthorityProjectsForeignOwnershipAsAttention(t *testing.T) {
	root := filepath.Join(t.TempDir(), "foreign-output")
	output, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{
		RootPath: root, CreateRoot: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	intent := coverageC6FilesystemIntent(t, output, 0xe1)
	marker := filepath.Join(
		root,
		checkpointstore.ControlDirectory,
		checkpointstore.CheckpointDirectory,
		checkpointstore.OwnershipDirectory,
		checkpointstore.OwnershipFile,
	)
	foreign, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	foreign[0] ^= 0xff
	if err := os.WriteFile(marker, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	authority, err := NewFilesystemResumeStateAuthority(FilesystemResumeRoot{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.ListResumeState(context.Background())
	if err != nil || inventory.Status() != ResumeStateListNeedsAttention ||
		len(inventory.Summaries()) != 0 || len(inventory.Attention()) != 1 ||
		inventory.Attention()[0].OperationID() != intent.OperationID() ||
		inventory.Attention()[0].Reason() != "target-ownership-unknown" {
		t.Fatalf("foreign inventory = (%+v, %v)", inventory, err)
	}
	after, err := os.ReadFile(marker)
	if err != nil || string(after) != string(foreign) {
		t.Fatalf("foreign ownership was mutated: %v", err)
	}
}
