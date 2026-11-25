//go:build windows || linux || darwin

package output

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func makeLargeTestPayload(size uint64) []byte {
	buf := make([]byte, size)
	copy(buf[:16], []byte("HEAD-WINDSHARE01"))
	mid := size / 2
	copy(buf[mid:mid+16], []byte("MIDL-WINDSHARE02"))
	copy(buf[size-16:], []byte("TAIL-WINDSHARE03"))
	return buf
}

func setupLostReceiptLargeFile(t *testing.T, seed byte) (string, transfer.SelectionSpec, transfer.ReceiveIntent, []byte, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, seed)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	art, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, intent, seed+1)
	payload := makeLargeTestPayload(SmallFileThreshold)
	f := testMaterializationFile(t, session, intent, seed+2, "large.bin", uint64(len(payload)), rootAdm)
	start, err := session.BeginFile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatal("expected large file transaction")
	}
	if err := tx.WriteRange(ctx, 0, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Accept(ctx); err != nil {
		t.Fatal(err)
	}
	op.journal.appendHook = func(kind string) error {
		if kind == "delivered" {
			return errors.New("simulated receipt write failure")
		}
		return nil
	}
	if _, err := tx.Commit(ctx); err == nil {
		t.Fatal("expected Commit to fail when delivery receipt append fails")
	}
	op.journal.appendHook = nil
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}
	res, _ := intent.MaterializationPlan().DestinationReservation()
	finalPath := filepath.Join(root, res.PhysicalName(), "large.bin")
	return root, sel, intent, payload, finalPath
}

func TestLargeFilePublishReceiptFailureReconcilesWithoutRedownload(t *testing.T) {
	ctx := context.Background()
	const seed byte = 0x71
	root, sel, _, payload, finalPath := setupLostReceiptLargeFile(t, seed)

	got, err := os.ReadFile(finalPath)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("published file not intact before resume: err=%v len=%d", err, len(got))
	}

	inv, err := ListOperations(ctx, root)
	if err != nil || len(inv.Summaries()) != 1 || inv.Summaries()[0].State() != RecoveryOperationResumable {
		t.Fatalf("inventory = (%+v, %v), want 1 resumable operation", inv, err)
	}

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
		t.Fatalf("reopen = (%v, %v)", lookup.Kind(), err)
	}
	reIntent, _ := lookup.Operation().ReceiveIntent()
	session, err := reopen.OpenOperation(ctx, lookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, reIntent, seed+1)
	f := testMaterializationFile(t, session, reIntent, seed+2, "large.bin", uint64(len(payload)), rootAdm)

	start, err := session.BeginFile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	settled, immediate := start.ImmediateSettlement()
	if !immediate || settled.Kind() != transfer.FilePreviouslyPublished {
		t.Fatalf("reconcile start = (immediate=%v, kind=%v), want FilePreviouslyPublished", immediate, settled.Kind())
	}
	delivery, found, err := session.(transfer.DeliveryHistory).LookupDelivery(ctx, f.Descriptor().FileID(), f.MaterializationRelativePath().String())
	if err != nil || !found || delivery.Size != uint64(len(payload)) {
		t.Fatalf("delivery receipt not recorded after reconcile: (%+v, %v, %v)", delivery, found, err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reopen.Close(); err != nil {
		t.Fatal(err)
	}

	// Once reconciled to a durable receipt, removing the published file must still skip redownload.
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}
	third, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	if err := third.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	thirdLookup, err := third.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || thirdLookup.Kind() != LookupReopened {
		t.Fatalf("third reopen = (%v, %v)", thirdLookup.Kind(), err)
	}
	thirdSession, err := third.OpenOperation(ctx, thirdLookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	delivery, found, err = thirdSession.(transfer.DeliveryHistory).LookupDelivery(ctx, f.Descriptor().FileID(), f.MaterializationRelativePath().String())
	if err != nil || !found || delivery.Size != uint64(len(payload)) {
		t.Fatalf("durable receipt lost after reopen: (%+v, %v, %v)", delivery, found, err)
	}
}

func TestPublishReceiptFailureReconcilesViaLookupDeliveryBeforeBeginFile(t *testing.T) {
	ctx := context.Background()
	const seed byte = 0x76
	root, sel, intent, payload, _ := setupLostReceiptLargeFile(t, seed)

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
		t.Fatalf("reopen = (%v, %v)", lookup.Kind(), err)
	}
	session, err := reopen.OpenOperation(ctx, lookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	res, _ := intent.MaterializationPlan().DestinationReservation()
	canonicalPath := res.RequestedName() + "/large.bin"
	delivery, found, err := session.(transfer.DeliveryHistory).LookupDelivery(ctx, testID16[catalog.FileID](seed+2), canonicalPath)
	if err != nil || !found || delivery.Size != uint64(len(payload)) {
		t.Fatalf("LookupDelivery did not reconcile publishing entry: (%+v, %v, %v)", delivery, found, err)
	}
}

func TestPublishReceiptFailureProtectsForeignReplacement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
		seed   byte
	}{
		{
			name: "same size different content",
			seed: 0x81,
			mutate: func(orig []byte) []byte {
				foreign := make([]byte, len(orig))
				copy(foreign, orig)
				foreign[len(foreign)/2] ^= 0xff
				return foreign
			},
		},
		{
			name: "different size",
			seed: 0x86,
			mutate: func([]byte) []byte {
				return []byte("foreign-user-file")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root, sel, _, payload, finalPath := setupLostReceiptLargeFile(t, tc.seed)
			foreign := tc.mutate(payload)
			if err := os.WriteFile(finalPath, foreign, 0o600); err != nil {
				t.Fatal(err)
			}

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
				t.Fatalf("reopen = (%v, %v)", lookup.Kind(), err)
			}
			reIntent, _ := lookup.Operation().ReceiveIntent()
			session, err := reopen.OpenOperation(ctx, lookup.Operation())
			if err != nil {
				t.Fatal(err)
			}
			rootAdm := testAdmitRoot(t, session, reIntent, tc.seed+1)
			f := testMaterializationFile(t, session, reIntent, tc.seed+2, "large.bin", uint64(len(payload)), rootAdm)

			if _, found, err := session.(transfer.DeliveryHistory).LookupDelivery(ctx, f.Descriptor().FileID(), f.MaterializationRelativePath().String()); err != nil || found {
				t.Fatalf("LookupDelivery matched foreign file: found=%v err=%v", found, err)
			}
			start, err := session.BeginFile(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			settled, immediate := start.ImmediateSettlement()
			if !immediate || settled.Kind() != transfer.FileCollision {
				t.Fatalf("foreign replacement start = (immediate=%v, kind=%v), want FileCollision", immediate, settled.Kind())
			}
			actual, err := os.ReadFile(finalPath)
			if err != nil || !bytes.Equal(actual, foreign) {
				t.Fatalf("foreign file was modified: err=%v", err)
			}
		})
	}
}

func TestPublishInterruptedBeforeRenameResumesWithoutRedownload(t *testing.T) {
	ctx := context.Background()
	const seed byte = 0x91
	root := t.TempDir()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, seed)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	art, _ := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, intent, seed+1)
	payload := makeLargeTestPayload(SmallFileThreshold)
	f := testMaterializationFile(t, session, intent, seed+2, "pending.bin", uint64(len(payload)), rootAdm)
	start, err := session.BeginFile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, _ := start.Transaction()
	if err := tx.WriteRange(ctx, 0, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Accept(ctx); err != nil {
		t.Fatal(err)
	}
	// Simulate journal write failure on "publishing" before rename: stage must stay intact.
	op.journal.appendHook = func(kind string) error {
		if kind == "publishing" {
			return errors.New("simulated pre-rename journal failure")
		}
		return nil
	}
	if _, err := tx.Commit(ctx); err == nil {
		t.Fatal("expected Commit to fail when publishing intent append fails")
	}
	op.journal.appendHook = nil
	res, _ := intent.MaterializationPlan().DestinationReservation()
	finalPath := filepath.Join(root, res.PhysicalName(), "pending.bin")
	if _, err := os.Stat(finalPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("destination file was published despite publishing intent failure: %v", err)
	}
	// Now simulate a crash right after durable publishing intent, before rename runs.
	partName := partFileName(f.Descriptor().FileID(), f.Target().Locator().Digest())
	partHandle, err := openChildFile(op.opDir, partName, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := samplePublicationFingerprint(partHandle, uint64(len(payload)))
	_ = partHandle.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := op.journal.recordPublishing(publishingEntry{
		FileID:        f.Descriptor().FileID(),
		Path:          f.MaterializationRelativePath().String(),
		Revision:      f.Descriptor().FileRevision(),
		Metadata:      f.Descriptor().SourceMetadata(),
		ExactSize:     uint64(len(payload)),
		PartName:      partName,
		CheckpointGen: 2,
		Fingerprint:   fp,
	}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	reopen, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	if err := reopen.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	reLookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || reLookup.Kind() != LookupReopened {
		t.Fatalf("reopen = (%v, %v)", reLookup.Kind(), err)
	}
	reIntent, _ := reLookup.Operation().ReceiveIntent()
	reSession, err := reopen.OpenOperation(ctx, reLookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	reRoot := testAdmitRoot(t, reSession, reIntent, seed+1)
	reFile := testMaterializationFile(t, reSession, reIntent, seed+2, "pending.bin", uint64(len(payload)), reRoot)

	if _, found, err := reSession.(transfer.DeliveryHistory).LookupDelivery(ctx, reFile.Descriptor().FileID(), reFile.MaterializationRelativePath().String()); err != nil || found {
		t.Fatalf("LookupDelivery skipped unpublished stage: found=%v err=%v", found, err)
	}
	reStart, err := reSession.BeginFile(ctx, reFile)
	if err != nil {
		t.Fatal(err)
	}
	reTx, durable, ok := reStart.Transaction()
	if !ok || len(durable.Ranges().Ranges()) != 1 || durable.Ranges().Ranges()[0] != (content.Range{Offset: 0, End: uint64(len(payload))}) {
		t.Fatalf("expected full reusable range from pending stage, got ok=%v ranges=%v", ok, durable.Ranges().Ranges())
	}
	settled, err := reTx.Commit(ctx)
	if err != nil || settled.Kind() != transfer.FilePublished {
		t.Fatalf("commit resumed pending publish = (%v, %v)", settled.Kind(), err)
	}
	got, err := os.ReadFile(finalPath)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("published content mismatch: err=%v len=%d", err, len(got))
	}
}

func TestMissingStageWithExistingDestinationPreflightsCollision(t *testing.T) {
	ctx := context.Background()
	const seed byte = 0x99
	root := t.TempDir()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, seed)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	art, _ := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, intent, seed+1)
	f := testMaterializationFile(t, session, intent, seed+2, "partial.bin", 16, rootAdm)
	start, err := session.BeginFile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, _ := start.Transaction()
	if err := tx.WriteRange(ctx, 0, []byte("halfhalf")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	opDir := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()))
	partPath := filepath.Join(opDir, partFileName(f.Descriptor().FileID(), f.Target().Locator().Digest()))
	if err := os.Remove(partPath); err != nil {
		t.Fatal(err)
	}
	res, _ := intent.MaterializationPlan().DestinationReservation()
	finalPath := filepath.Join(root, res.PhysicalName(), "partial.bin")
	if err := os.WriteFile(finalPath, []byte("keep-foreign"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopen, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	if err := reopen.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	reLookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || reLookup.Kind() != LookupReopened {
		t.Fatalf("reopen = (%v, %v)", reLookup.Kind(), err)
	}
	reIntent, _ := reLookup.Operation().ReceiveIntent()
	reSession, err := reopen.OpenOperation(ctx, reLookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	reRoot := testAdmitRoot(t, reSession, reIntent, seed+1)
	reFile := testMaterializationFile(t, reSession, reIntent, seed+2, "partial.bin", 16, reRoot)
	reStart, err := reSession.BeginFile(ctx, reFile)
	if err != nil {
		t.Fatal(err)
	}
	settled, immediate := reStart.ImmediateSettlement()
	if !immediate || settled.Kind() != transfer.FileCollision {
		t.Fatalf("missing stage with occupied destination = (immediate=%v, kind=%v), want FileCollision", immediate, settled.Kind())
	}
	if _, err := os.Stat(partPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("collision preflight unexpectedly recreated .part: %v", err)
	}
	if got, err := os.ReadFile(finalPath); err != nil || string(got) != "keep-foreign" {
		t.Fatalf("foreign file altered: %q %v", got, err)
	}
}

func TestSmallFileFastPathOmitsPublishingRecord(t *testing.T) {
	ctx := context.Background()
	const seed byte = 0xa1
	root := t.TempDir()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, seed)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	art, _ := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, intent, seed+1)
	payload := []byte("small-fast-path")
	f := testMaterializationFile(t, session, intent, seed+2, "small.txt", uint64(len(payload)), rootAdm)
	start, err := session.BeginFile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, _ := start.Transaction()
	if err := tx.WriteRange(ctx, 0, payload); err != nil {
		t.Fatal(err)
	}
	if settled, err := tx.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
		t.Fatalf("commit small file = (%v, %v)", settled.Kind(), err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()), journalFileName)
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"k":"publishing"`) || strings.Contains(string(raw), `"k":"partial"`) {
		t.Fatalf("small-file fast path unexpectedly wrote partial/publishing journal records:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"k":"delivered"`) {
		t.Fatalf("missing delivered record in journal:\n%s", raw)
	}
}

func TestSingleFileLayoutLostReceiptReconcilesViaLookupDelivery(t *testing.T) {
	ctx := context.Background()
	const seed byte = 0xa9
	root := t.TempDir()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, seed)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	fileID := testID16[catalog.FileID](seed + 2)
	art, err := receivecontract.NewSingleFileDirectoryTree(fileID, "movie.bin", "movie.bin")
	if err != nil {
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	payload := makeLargeTestPayload(SmallFileThreshold)
	f := testSingleFileMaterialization(t, session, intent, fileID, seed+3, "movie.bin", uint64(len(payload)))
	start, err := session.BeginFile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, _ := start.Transaction()
	if err := tx.WriteRange(ctx, 0, payload); err != nil {
		t.Fatal(err)
	}
	op.journal.appendHook = func(kind string) error {
		if kind == "delivered" {
			return errors.New("simulated receipt write failure")
		}
		return nil
	}
	if _, err := tx.Commit(ctx); err == nil {
		t.Fatal("expected Commit failure on simulated receipt error")
	}
	op.journal.appendHook = nil
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	reopen, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	if err := reopen.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	reLookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || reLookup.Kind() != LookupReopened {
		t.Fatalf("reopen = (%v, %v)", reLookup.Kind(), err)
	}
	reSession, err := reopen.OpenOperation(ctx, reLookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	delivery, found, err := reSession.(transfer.DeliveryHistory).LookupDelivery(ctx, fileID, "movie.bin")
	if err != nil || !found || delivery.Size != uint64(len(payload)) {
		t.Fatalf("single-file LookupDelivery reconcile = (%+v, %v, %v)", delivery, found, err)
	}
}

func TestLargeFileCommitCollisionKeepsPublishingIntactWithoutDowngrade(t *testing.T) {
	ctx := context.Background()
	const seed byte = 0xb1
	root := t.TempDir()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, seed)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	art, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, intent, seed+1)
	payload := makeLargeTestPayload(SmallFileThreshold)
	f := testMaterializationFile(t, session, intent, seed+2, "large-collide.bin", uint64(len(payload)), rootAdm)
	start, err := session.BeginFile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatal("expected large file transaction")
	}
	if err := tx.WriteRange(ctx, 0, payload); err != nil {
		t.Fatal(err)
	}

	res, _ := intent.MaterializationPlan().DestinationReservation()
	finalPath := filepath.Join(root, res.PhysicalName(), "large-collide.bin")
	foreign := []byte("occupied-before-commit")
	if err := os.WriteFile(finalPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}

	// Durable-publish collision must keep the already-recorded publishing entry
	// intact without appending a partial downgrade.
	op.journal.appendHook = func(kind string) error {
		if kind == "partial" {
			return errors.New("unexpected partial downgrade on durable-publish collision")
		}
		return nil
	}
	settled, err := tx.Commit(ctx)
	op.journal.appendHook = nil
	if err != nil || settled.Kind() != transfer.FileCollision {
		t.Fatalf("commit on collision = (%v, %v), want FileCollision", settled.Kind(), err)
	}
	canonicalPath := f.MaterializationRelativePath().String()
	pub, hasPub := op.journal.lookupPublishing(canonicalPath)
	if !hasPub || pub.Fingerprint == ([stageFingerprintBytes]byte{}) {
		t.Fatalf("publishing entry lost after collision: hasPub=%t fp=%x", hasPub, pub.Fingerprint)
	}
	if _, hasPartial := op.journal.lookupPartial(canonicalPath); hasPartial {
		t.Fatal("durable-publish collision unexpectedly wrote a partial entry")
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	// Clear the colliding file and verify retry publishes with zero re-download.
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}
	reopen, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	if err := reopen.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	reLookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || reLookup.Kind() != LookupReopened {
		t.Fatalf("reopen = (%v, %v)", reLookup.Kind(), err)
	}
	reIntent, _ := reLookup.Operation().ReceiveIntent()
	reSession, err := reopen.OpenOperation(ctx, reLookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	reRoot := testAdmitRoot(t, reSession, reIntent, seed+1)
	reFile := testMaterializationFile(t, reSession, reIntent, seed+2, "large-collide.bin", uint64(len(payload)), reRoot)
	reStart, err := reSession.BeginFile(ctx, reFile)
	if err != nil {
		t.Fatal(err)
	}
	reTx, durable, ok := reStart.Transaction()
	if !ok || rangeTotalBytes(durable.Ranges().Ranges()) != uint64(len(payload)) {
		t.Fatalf("reopened collision stage = (ok=%t, bytes=%d), want %d", ok, rangeTotalBytes(durable.Ranges().Ranges()), len(payload))
	}
	published, err := reTx.Commit(ctx)
	if err != nil || published.Kind() != transfer.FilePublished {
		t.Fatalf("retry commit = (%v, %v)", published.Kind(), err)
	}
	if provenance, hasProv := published.PublicationProvenance(); !hasProv || provenance != transfer.FileResumed {
		t.Fatalf("provenance = (%v, %t), want FileResumed", provenance, hasProv)
	}
	if got, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("published content mismatch: err=%v len=%d", err, len(got))
	}
}

func TestPublishingRevisionConflictPreservesStageAndResumesWhenSourceMatches(t *testing.T) {
	ctx := context.Background()
	const seed byte = 0xb9
	root := t.TempDir()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, seed)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	art, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, intent, seed+1)
	payload := makeLargeTestPayload(SmallFileThreshold)
	fileID := testID16[catalog.FileID](seed + 2)
	origRev := testID16[content.FileRevision](seed + 3)
	origFile := testMaterializationFileWithIDs(t, session.(*Session), intent, fileID, origRev, "drift-pub.bin", uint64(len(payload)), rootAdm)
	start, err := session.BeginFile(ctx, origFile)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, _ := start.Transaction()
	if err := tx.WriteRange(ctx, 0, payload); err != nil {
		t.Fatal(err)
	}
	res, _ := intent.MaterializationPlan().DestinationReservation()
	finalPath := filepath.Join(root, res.PhysicalName(), "drift-pub.bin")
	if err := os.WriteFile(finalPath, []byte("occupy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if settled, err := tx.Commit(ctx); err != nil || settled.Kind() != transfer.FileCollision {
		t.Fatalf("initial commit collision = (%v, %v)", settled.Kind(), err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}

	// Reopen 1: source drifts -> must keep publishing entry with BlockedReason
	// instead of clobbering it with a zero-fingerprint partial.
	reopen1, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopen1.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup1, err := reopen1.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup1.Kind() != LookupReopened {
		t.Fatalf("reopen1 = (%v, %v)", lookup1.Kind(), err)
	}
	op1 := lookup1.Operation()
	intent1, _ := op1.ReceiveIntent()
	session1, err := reopen1.OpenOperation(ctx, op1)
	if err != nil {
		t.Fatal(err)
	}
	root1 := testAdmitRoot(t, session1, intent1, seed+1)
	driftedFile := testMaterializationFileWithIDs(
		t, session1.(*Session), intent1, fileID, testID16[content.FileRevision](seed+9),
		"drift-pub.bin", uint64(len(payload))+4096, root1,
	)
	blockedStart, err := session1.BeginFile(ctx, driftedFile)
	if err != nil {
		t.Fatal(err)
	}
	settled, immediate := blockedStart.ImmediateSettlement()
	_, reason, blocked := settled.ItemBlock()
	if !immediate || settled.Kind() != transfer.FileItemBlocked || !blocked || reason != transfer.ItemBlockRevisionConflict {
		t.Fatalf("drifted publishing start = (immediate=%t, kind=%v, reason=%v), want ItemBlockRevisionConflict", immediate, settled.Kind(), reason)
	}
	canonicalPath := driftedFile.MaterializationRelativePath().String()
	pub1, hasPub := op1.journal.lookupPublishing(canonicalPath)
	if !hasPub || pub1.BlockedReason != transfer.ItemBlockRevisionConflict || pub1.Fingerprint == ([stageFingerprintBytes]byte{}) {
		t.Fatalf("blocked publishing entry = (%+v, %t), want BlockedReason=RevisionConflict with valid fp", pub1, hasPub)
	}
	if _, hasPartial := op1.journal.lookupPartial(canonicalPath); hasPartial {
		t.Fatal("publishing revision conflict unexpectedly clobbered publishing with partial")
	}
	if _, err := session1.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reopen1.Close(); err != nil {
		t.Fatal(err)
	}

	// Recovery inventory must report the blocked publishing file as blocked, not resumable.
	inv, err := ListOperations(ctx, root)
	if err != nil || inv.Status() != RecoveryListReady || len(inv.Summaries()) != 1 {
		t.Fatalf("inventory = (%+v, %v), want 1 ready summary", inv, err)
	}
	summary := inv.Summaries()[0]
	if summary.State() != RecoveryOperationIncomplete || len(summary.Items()) != 1 ||
		summary.Items()[0].State() != RecoveryItemBlocked ||
		summary.Items()[0].BlockReason() != RecoveryItemBlockRevisionConflict {
		t.Fatalf("blocked publishing summary = %+v items=%+v, want incomplete with BlockedRevisionConflict", summary, summary.Items())
	}

	// Reopen 2: source matches original revision again -> must unblock and reuse
	// 100% of the completed stage instead of deleting .part and restarting from 0.
	reopen2, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen2.Close()
	if err := reopen2.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup2, err := reopen2.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup2.Kind() != LookupReopened {
		t.Fatalf("reopen2 = (%v, %v)", lookup2.Kind(), err)
	}
	intent2, _ := lookup2.Operation().ReceiveIntent()
	session2, err := reopen2.OpenOperation(ctx, lookup2.Operation())
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := session2.(transfer.DeliveryHistory).LookupDelivery(ctx, fileID, canonicalPath); err != nil || found {
		t.Fatalf("LookupDelivery unexpectedly matched blocked publishing: found=%t err=%v", found, err)
	}
	root2 := testAdmitRoot(t, session2, intent2, seed+1)
	restoredFile := testMaterializationFileWithIDs(t, session2.(*Session), intent2, fileID, origRev, "drift-pub.bin", uint64(len(payload)), root2)
	resumedStart, err := session2.BeginFile(ctx, restoredFile)
	if err != nil {
		t.Fatal(err)
	}
	reTx, durable, ok := resumedStart.Transaction()
	if !ok || rangeTotalBytes(durable.Ranges().Ranges()) != uint64(len(payload)) {
		t.Fatalf("restored publishing start = (ok=%t, bytes=%d), want %d", ok, rangeTotalBytes(durable.Ranges().Ranges()), len(payload))
	}
	published, err := reTx.Commit(ctx)
	if err != nil || published.Kind() != transfer.FilePublished {
		t.Fatalf("commit restored publishing = (%v, %v)", published.Kind(), err)
	}
	if got, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("published content mismatch: err=%v len=%d", err, len(got))
	}
}
