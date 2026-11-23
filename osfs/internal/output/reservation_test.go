//go:build windows || linux || darwin

package output

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestResultRootAvoidsPausedSingleFileReservation(t *testing.T) {
	root := t.TempDir()

	// Pause a single-file receive named "photos" before publication.
	fileSel, fileIntent, fileID := createPausedNamedSingleFileOp(t, root, 0x11, "photos")
	fileRes, _ := fileIntent.MaterializationPlan().DestinationReservation()
	if fileRes.PhysicalName() != "photos" {
		t.Fatalf("paused single-file physical name = %q, want %q", fileRes.PhysicalName(), "photos")
	}

	// Receive a directory named "photos" into the same container root.
	dirAuth, _, dirIntent := openActiveResultRootOp(t, root, 0x21, "photos")
	dirRes, _ := dirIntent.MaterializationPlan().DestinationReservation()
	if dirRes.PhysicalName() == "photos" || !strings.HasPrefix(dirRes.PhysicalName(), "photos-") {
		_ = dirAuth.Close()
		t.Fatalf("directory reservation stole single-file name: got %q", dirRes.PhysicalName())
	}
	if err := dirAuth.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(filepath.Join(root, "photos")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("reserved single-file path was materialized by folder task: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(root, dirRes.PhysicalName())); err != nil || !info.IsDir() {
		t.Fatalf("suffixed folder reservation missing: info=%v err=%v", info, err)
	}

	// Resuming the paused single file must still publish cleanly to "photos".
	finishPausedNamedSingleFileOp(t, root, fileSel, fileID, 0x11, "photos")
}

func TestResultRootAvoidsActiveSingleFileReservation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	fileAuth, fileOp, fileIntent, fileID := openActiveNamedSingleFileOp(t, root, 0x31, "photos")
	defer fileAuth.Close()

	dirAuth, _, dirIntent := openActiveResultRootOp(t, root, 0x41, "photos")
	defer dirAuth.Close()

	dirRes, _ := dirIntent.MaterializationPlan().DestinationReservation()
	if dirRes.PhysicalName() == "photos" {
		t.Fatalf("directory reservation collided with active single-file reservation %q", dirRes.PhysicalName())
	}

	// Publish the active single file while the folder reservation is live.
	session, err := fileAuth.OpenOperation(ctx, fileOp)
	if err != nil {
		t.Fatal(err)
	}
	f, err := buildSingleFileMaterialization(session, fileIntent, fileID, 0x34, "photos", uint64(len(retirementFullPayload)))
	if err != nil {
		t.Fatal(err)
	}
	start, err := session.BeginFile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatal("expected active transaction for single-file transfer")
	}
	if err := tx.WriteRange(ctx, 0, []byte(retirementFullPayload)); err != nil {
		t.Fatal(err)
	}
	settlement, err := tx.Commit(ctx)
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("single-file commit = (%v, %v), want FilePublished", settlement.Kind(), err)
	}
	if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "photos"))
	if err != nil || string(got) != retirementFullPayload {
		t.Fatalf("published file = (%q, %v), want %q", got, err, retirementFullPayload)
	}
}

func TestTopLevelReservationFoldsCaseAcrossFilesAndFolders(t *testing.T) {
	root := t.TempDir()

	fileSel, fileIntent, fileID := createPausedNamedSingleFileOp(t, root, 0x51, "Photos")
	fileRes, _ := fileIntent.MaterializationPlan().DestinationReservation()
	if fileRes.PhysicalName() != "Photos" {
		t.Fatalf("paused single-file physical name = %q, want %q", fileRes.PhysicalName(), "Photos")
	}

	// A folder differing only in case must not occupy the case-folded alias.
	dirAuth, _, dirIntent := openActiveResultRootOp(t, root, 0x61, "photos")
	dirRes, _ := dirIntent.MaterializationPlan().DestinationReservation()
	if err := dirAuth.Close(); err != nil {
		t.Fatal(err)
	}
	if catalog.SiblingNameKey(dirRes.PhysicalName()) == catalog.SiblingNameKey("Photos") {
		t.Fatalf("folder reservation %q aliases paused single file %q", dirRes.PhysicalName(), "Photos")
	}

	// Another single file differing only in case must also receive a suffix.
	otherFileAuth, _, otherFileIntent, _ := openActiveNamedSingleFileOp(t, root, 0x71, "PHOTOS")
	otherRes, _ := otherFileIntent.MaterializationPlan().DestinationReservation()
	if err := otherFileAuth.Close(); err != nil {
		t.Fatal(err)
	}
	if catalog.SiblingNameKey(otherRes.PhysicalName()) == catalog.SiblingNameKey("Photos") {
		t.Fatalf("single-file reservation %q aliases paused single file %q", otherRes.PhysicalName(), "Photos")
	}

	finishPausedNamedSingleFileOp(t, root, fileSel, fileID, 0x51, "Photos")
}

func TestRemovedPausedResultRootRemainsReservedUntilDiscarded(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	dirAuth, _, dirIntent := openActiveResultRootOp(t, root, 0x81, "photos")
	dirRes, _ := dirIntent.MaterializationPlan().DestinationReservation()
	if dirRes.PhysicalName() != "photos" {
		_ = dirAuth.Close()
		t.Fatalf("initial folder physical name = %q, want %q", dirRes.PhysicalName(), "photos")
	}
	if err := dirAuth.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate the user deleting the paused result directory while its retained
	// operation still exists under .windshare-tmp.
	if err := os.Remove(filepath.Join(root, "photos")); err != nil {
		t.Fatal(err)
	}

	nextDirAuth, _, nextDirIntent := openActiveResultRootOp(t, root, 0x91, "photos")
	nextDirRes, _ := nextDirIntent.MaterializationPlan().DestinationReservation()
	if err := nextDirAuth.Close(); err != nil {
		t.Fatal(err)
	}
	if nextDirRes.PhysicalName() == "photos" {
		t.Fatalf("new folder reused retained paused folder name %q", nextDirRes.PhysicalName())
	}

	nextFileAuth, _, nextFileIntent, _ := openActiveNamedSingleFileOp(t, root, 0xa1, "photos")
	nextFileRes, _ := nextFileIntent.MaterializationPlan().DestinationReservation()
	if err := nextFileAuth.Close(); err != nil {
		t.Fatal(err)
	}
	if nextFileRes.PhysicalName() == "photos" {
		t.Fatalf("new single file reused retained paused folder name %q", nextFileRes.PhysicalName())
	}

	// Discarding the retained operation releases its top-level name claim.
	if _, err := DiscardOperation(ctx, root, dirIntent.OperationID()); err != nil {
		t.Fatal(err)
	}
	freshDirAuth, _, freshDirIntent := openActiveResultRootOp(t, root, 0xb1, "photos")
	freshDirRes, _ := freshDirIntent.MaterializationPlan().DestinationReservation()
	if err := freshDirAuth.Close(); err != nil {
		t.Fatal(err)
	}
	if freshDirRes.PhysicalName() != "photos" {
		t.Fatalf("folder after discard physical name = %q, want %q", freshDirRes.PhysicalName(), "photos")
	}
}

func openActiveNamedSingleFileOp(
	t *testing.T,
	root string,
	seed byte,
	name string,
) (*Authority, *Operation, transfer.ReceiveIntent, catalog.FileID) {
	t.Helper()
	ctx := context.Background()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, seed)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		_ = auth.Close()
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	fileID := testID16[catalog.FileID](seed + 2)
	art, err := receivecontract.NewSingleFileDirectoryTree(fileID, name, name)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	return auth, op, intent, fileID
}

func openActiveResultRootOp(
	t *testing.T,
	root string,
	seed byte,
	dirName string,
) (*Authority, *Operation, transfer.ReceiveIntent) {
	t.Helper()
	ctx := context.Background()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, seed)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		_ = auth.Close()
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	layout, err := receivecontract.NewCompleteDirectoryResultRoot(testID16[catalog.DirectoryID](seed+2), dirName)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	art, err := receivecontract.NewResultRootDirectoryTree(layout)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	return auth, op, intent
}

func createPausedNamedSingleFileOp(
	t *testing.T,
	root string,
	seed byte,
	name string,
) (transfer.SelectionSpec, transfer.ReceiveIntent, catalog.FileID) {
	t.Helper()
	ctx := context.Background()
	auth, op, intent, fileID := openActiveNamedSingleFileOp(t, root, seed, name)
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	f, err := buildSingleFileMaterialization(session, intent, fileID, seed+3, name, uint64(len(retirementFullPayload)))
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	start, err := session.BeginFile(ctx, f)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		_ = auth.Close()
		t.Fatal("expected initial file transaction")
	}
	if err := tx.WriteRange(ctx, 0, []byte(retirementFirstHalf)); err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	if _, err := tx.Pause(ctx); err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}
	return intent.SelectionSpec(), intent, fileID
}

func finishPausedNamedSingleFileOp(
	t *testing.T,
	root string,
	sel transfer.SelectionSpec,
	fileID catalog.FileID,
	seed byte,
	name string,
) {
	t.Helper()
	ctx := context.Background()
	reopen, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	if err := reopen.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupReopened {
		t.Fatalf("reopen select = (%v, %v)", lookup.Kind(), err)
	}
	op := lookup.Operation()
	intent, _ := op.ReceiveIntent()
	session, err := reopen.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	f, err := buildSingleFileMaterialization(session, intent, fileID, seed+3, name, uint64(len(retirementFullPayload)))
	if err != nil {
		t.Fatal(err)
	}
	start, err := session.BeginFile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	tx, durable, ok := start.Transaction()
	if !ok {
		t.Fatal("reopened single file did not start transaction")
	}
	ranges := durable.Ranges().Ranges()
	if len(ranges) != 1 || ranges[0].Offset != 0 || ranges[0].End != uint64(len(retirementFirstHalf)) {
		t.Fatalf("reopened ranges = %v", ranges)
	}
	if err := tx.WriteRange(ctx, uint64(len(retirementFirstHalf)), []byte(retirementSecondHalf)); err != nil {
		t.Fatal(err)
	}
	settlement, err := tx.Commit(ctx)
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("commit = (%v, %v), want FilePublished", settlement.Kind(), err)
	}
	if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || string(got) != retirementFullPayload {
		t.Fatalf("published content = (%q, %v), want %q", got, err, retirementFullPayload)
	}
}
