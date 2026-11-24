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
	"slices"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestResumeValidPartialPreflightsDestinationCollisionAndContinuesOnceCleared(t *testing.T) {
	t.Run("result root isolates sibling and preserves partial", func(t *testing.T) {
		ctx := context.Background()
		const (
			seed       byte = 0x31
			totalSize       = 16 * 1024
			prefixSize      = 8 * 1024
		)
		expected := makePatternPayload(totalSize, seed)
		siblingPayload := makePatternPayload(4*1024, seed+9)
		root, sel, intent, partPath, finalPath := setupPausedPartialFile(
			t, seed, "target.bin", totalSize,
			[]struct {
				offset uint64
				data   []byte
			}{{offset: 0, data: expected[:prefixSize]}},
			false,
		)
		foreign := []byte("keep-foreign-file")
		if err := os.WriteFile(finalPath, foreign, 0o600); err != nil {
			t.Fatal(err)
		}

		stream, err := diagnostics.NewStream(64, nil)
		if err != nil {
			t.Fatal(err)
		}
		reopen, err := New(Config{RootPath: root, Diagnostics: stream.Sink()})
		if err != nil {
			t.Fatal(err)
		}
		if err := reopen.BindDestination(ctx); err != nil {
			t.Fatal(err)
		}
		lookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || lookup.Kind() != LookupReopened {
			t.Fatalf("reopen = (%v, %v)", lookup.Kind(), err)
		}
		reIntent, _ := lookup.Operation().ReceiveIntent()
		if reIntent.OperationID() != intent.OperationID() {
			t.Fatalf("reopened operation ID mismatch")
		}
		session, err := reopen.OpenOperation(ctx, lookup.Operation())
		if err != nil {
			t.Fatal(err)
		}
		rootAdm := testAdmitRoot(t, session, reIntent, seed+1)
		targetFile := testMaterializationFile(t, session, reIntent, seed+2, "target.bin", totalSize, rootAdm)
		start, err := session.BeginFile(ctx, targetFile)
		if err != nil {
			t.Fatal(err)
		}
		settled, immediate := start.ImmediateSettlement()
		if !immediate || settled.Kind() != transfer.FileCollision {
			t.Fatalf("occupied resume start = (immediate=%v, kind=%v), want FileCollision", immediate, settled.Kind())
		}
		assertDestinationCollisionTrace(t, stream, prefixSize)

		if got, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(got, foreign) {
			t.Fatalf("foreign file altered: %q %v", got, err)
		}
		if info, err := os.Stat(partPath); err != nil || info.Size() != prefixSize {
			t.Fatalf("staged partial lost after preflight collision: info=%v err=%v", info, err)
		}

		// Independent siblings in the same operation must still publish cleanly.
		siblingFile := testMaterializationFile(t, session, reIntent, seed+5, "sibling.bin", uint64(len(siblingPayload)), rootAdm)
		sibStart, err := session.BeginFile(ctx, siblingFile)
		if err != nil {
			t.Fatal(err)
		}
		sibTx, _, ok := sibStart.Transaction()
		if !ok {
			t.Fatalf("sibling did not start transaction after collision")
		}
		if err := sibTx.WriteRange(ctx, 0, siblingPayload); err != nil {
			t.Fatal(err)
		}
		if sibSettled, err := sibTx.Commit(ctx); err != nil || sibSettled.Kind() != transfer.FilePublished {
			t.Fatalf("sibling commit = (%v, %v)", sibSettled.Kind(), err)
		}
		if _, err := session.FinalizeDirectory(ctx, rootAdm); err != nil {
			t.Fatal(err)
		}
		if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomePartial); err != nil {
			t.Fatal(err)
		}
		if err := reopen.Close(); err != nil {
			t.Fatal(err)
		}

		inv, err := ListOperations(ctx, root)
		if err != nil || len(inv.Summaries()) != 1 || inv.Summaries()[0].State() != RecoveryOperationResumable {
			t.Fatalf("inventory after collision = (%+v, %v), want 1 resumable operation", inv, err)
		}

		// Clear the colliding file and verify the same operation resumes from prefixSize.
		if err := os.Remove(finalPath); err != nil {
			t.Fatal(err)
		}
		retryAuth, err := New(Config{RootPath: root})
		if err != nil {
			t.Fatal(err)
		}
		defer retryAuth.Close()
		if err := retryAuth.BindDestination(ctx); err != nil {
			t.Fatal(err)
		}
		retryLookup, err := retryAuth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || retryLookup.Kind() != LookupReopened {
			t.Fatalf("retry reopen = (%v, %v)", retryLookup.Kind(), err)
		}
		retryIntent, _ := retryLookup.Operation().ReceiveIntent()
		if retryIntent.OperationID() != intent.OperationID() {
			t.Fatalf("retry operation ID mismatch")
		}
		retrySession, err := retryAuth.OpenOperation(ctx, retryLookup.Operation())
		if err != nil {
			t.Fatal(err)
		}
		if _, found, err := retrySession.(transfer.DeliveryHistory).LookupDelivery(ctx, siblingFile.Descriptor().FileID(), siblingFile.MaterializationRelativePath().String()); err != nil || !found {
			t.Fatalf("delivered sibling missing from history: found=%v err=%v", found, err)
		}
		retryRoot := testAdmitRoot(t, retrySession, retryIntent, seed+1)
		retryTarget := testMaterializationFile(t, retrySession, retryIntent, seed+2, "target.bin", totalSize, retryRoot)
		retryStart, err := retrySession.BeginFile(ctx, retryTarget)
		if err != nil {
			t.Fatal(err)
		}
		retryTx, durable, ok := retryStart.Transaction()
		if !ok || !slices.Equal(durable.Ranges().Ranges(), []content.Range{{Offset: 0, End: prefixSize}}) {
			t.Fatalf("retry resume = (ok=%v, ranges=%v), want [0,%d)", ok, durable.Ranges().Ranges(), prefixSize)
		}
		if err := retryTx.WriteRange(ctx, prefixSize, expected[prefixSize:]); err != nil {
			t.Fatal(err)
		}
		published, err := retryTx.Commit(ctx)
		if err != nil || published.Kind() != transfer.FilePublished {
			t.Fatalf("retry commit = (%v, %v)", published.Kind(), err)
		}
		if provenance, ok := published.PublicationProvenance(); !ok || provenance != transfer.FileResumed {
			t.Fatalf("publication provenance = (%v, %v), want FileResumed", provenance, ok)
		}
		if _, err := retrySession.FinalizeDirectory(ctx, retryRoot); err != nil {
			t.Fatal(err)
		}
		if _, err := retrySession.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
			t.Fatal(err)
		}

		if got, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(got, expected) {
			t.Fatalf("published target mismatch: err=%v len=%d", err, len(got))
		}
		if _, err := os.Stat(filepath.Join(root, ControlDirName)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("control dir survived completed retry: %v", err)
		}
	})

	t.Run("single file preflights occupied destination and resumes once cleared", func(t *testing.T) {
		ctx := context.Background()
		const (
			seed       byte = 0x41
			totalSize       = 16 * 1024
			prefixSize      = 8 * 1024
		)
		expected := makePatternPayload(totalSize, seed)
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
		f := testSingleFileMaterialization(t, session, intent, fileID, seed+3, "movie.bin", totalSize)
		start, err := session.BeginFile(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		tx, _, _ := start.Transaction()
		if err := tx.WriteRange(ctx, 0, expected[:prefixSize]); err != nil {
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

		// Single files stay staged in .windshare-tmp while paused, so another
		// file can occupy the destination root path before resume.
		finalPath := filepath.Join(root, "movie.bin")
		foreign := []byte("keep-single-foreign")
		if err := os.WriteFile(finalPath, foreign, 0o600); err != nil {
			t.Fatal(err)
		}

		reopen, err := New(Config{RootPath: root})
		if err != nil {
			t.Fatal(err)
		}
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
		reFile := testSingleFileMaterialization(t, reSession, reIntent, fileID, seed+3, "movie.bin", totalSize)
		reStart, err := reSession.BeginFile(ctx, reFile)
		if err != nil {
			t.Fatal(err)
		}
		settled, immediate := reStart.ImmediateSettlement()
		if !immediate || settled.Kind() != transfer.FileCollision {
			t.Fatalf("single-file occupied resume = (immediate=%v, kind=%v), want FileCollision", immediate, settled.Kind())
		}
		if _, err := reSession.FinalizeTree(ctx, transfer.DirectTreeOutcomePartial); err != nil {
			t.Fatal(err)
		}
		if err := reopen.Close(); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(got, foreign) {
			t.Fatalf("single-file foreign entry altered: %q %v", got, err)
		}

		if err := os.Remove(finalPath); err != nil {
			t.Fatal(err)
		}
		retryAuth, err := New(Config{RootPath: root})
		if err != nil {
			t.Fatal(err)
		}
		defer retryAuth.Close()
		if err := retryAuth.BindDestination(ctx); err != nil {
			t.Fatal(err)
		}
		retryLookup, err := retryAuth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || retryLookup.Kind() != LookupReopened {
			t.Fatalf("retry reopen = (%v, %v)", retryLookup.Kind(), err)
		}
		retryIntent, _ := retryLookup.Operation().ReceiveIntent()
		if retryIntent.OperationID() != intent.OperationID() {
			t.Fatalf("single-file retry operation ID mismatch")
		}
		retrySession, err := retryAuth.OpenOperation(ctx, retryLookup.Operation())
		if err != nil {
			t.Fatal(err)
		}
		retryFile := testSingleFileMaterialization(t, retrySession, retryIntent, fileID, seed+3, "movie.bin", totalSize)
		retryStart, err := retrySession.BeginFile(ctx, retryFile)
		if err != nil {
			t.Fatal(err)
		}
		retryTx, durable, ok := retryStart.Transaction()
		if !ok || !slices.Equal(durable.Ranges().Ranges(), []content.Range{{Offset: 0, End: prefixSize}}) {
			t.Fatalf("single-file retry = (ok=%v, ranges=%v), want [0,%d)", ok, durable.Ranges().Ranges(), prefixSize)
		}
		if err := retryTx.WriteRange(ctx, prefixSize, expected[prefixSize:]); err != nil {
			t.Fatal(err)
		}
		if committed, err := retryTx.Commit(ctx); err != nil || committed.Kind() != transfer.FilePublished {
			t.Fatalf("single-file retry commit = (%v, %v)", committed.Kind(), err)
		}
		if _, err := retrySession.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(got, expected) {
			t.Fatalf("single-file final content mismatch: err=%v len=%d", err, len(got))
		}
	})
}

func TestResumePendingPublishPreflightsDestinationCollisionAndPublishesOnceCleared(t *testing.T) {
	ctx := context.Background()
	const seed byte = 0x51
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
	if _, err := tx.Pause(ctx); err != nil {
		t.Fatal(err)
	}
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

	res, _ := intent.MaterializationPlan().DestinationReservation()
	finalPath := filepath.Join(root, res.PhysicalName(), "pending.bin")
	foreign := []byte("keep-foreign-pending")
	if err := os.WriteFile(finalPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}

	stream, err := diagnostics.NewStream(64, nil)
	if err != nil {
		t.Fatal(err)
	}
	reopen, err := New(Config{RootPath: root, Diagnostics: stream.Sink()})
	if err != nil {
		t.Fatal(err)
	}
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
	reStart, err := reSession.BeginFile(ctx, reFile)
	if err != nil {
		t.Fatal(err)
	}
	settled, immediate := reStart.ImmediateSettlement()
	if !immediate || settled.Kind() != transfer.FileCollision {
		t.Fatalf("pending publish occupied start = (immediate=%v, kind=%v), want FileCollision", immediate, settled.Kind())
	}
	assertDestinationCollisionTrace(t, stream, uint64(len(payload)))
	if _, err := reSession.FinalizeDirectory(ctx, reRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := reSession.FinalizeTree(ctx, transfer.DirectTreeOutcomePartial); err != nil {
		t.Fatal(err)
	}
	if err := reopen.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(got, foreign) {
		t.Fatalf("foreign file altered: %q %v", got, err)
	}

	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}
	retryAuth, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer retryAuth.Close()
	if err := retryAuth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	retryLookup, err := retryAuth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || retryLookup.Kind() != LookupReopened {
		t.Fatalf("retry reopen = (%v, %v)", retryLookup.Kind(), err)
	}
	retryIntent, _ := retryLookup.Operation().ReceiveIntent()
	retrySession, err := retryAuth.OpenOperation(ctx, retryLookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	retryRoot := testAdmitRoot(t, retrySession, retryIntent, seed+1)
	retryFile := testMaterializationFile(t, retrySession, retryIntent, seed+2, "pending.bin", uint64(len(payload)), retryRoot)
	retryStart, err := retrySession.BeginFile(ctx, retryFile)
	if err != nil {
		t.Fatal(err)
	}
	retryTx, durable, ok := retryStart.Transaction()
	if !ok || !slices.Equal(durable.Ranges().Ranges(), []content.Range{{Offset: 0, End: uint64(len(payload))}}) {
		t.Fatalf("retry pending publish = (ok=%v, ranges=%v), want full range", ok, durable.Ranges().Ranges())
	}
	if committed, err := retryTx.Commit(ctx); err != nil || committed.Kind() != transfer.FilePublished {
		t.Fatalf("retry commit = (%v, %v)", committed.Kind(), err)
	}
	if got, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("published content mismatch: err=%v len=%d", err, len(got))
	}
}

func TestResumeRevisionConflictPrecedesDestinationCollision(t *testing.T) {
	ctx := context.Background()
	const (
		seed       byte = 0x61
		totalSize       = 16 * 1024
		prefixSize      = 8 * 1024
	)
	expected := makePatternPayload(totalSize, seed)
	root, sel, _, _, finalPath := setupPausedPartialFile(
		t, seed, "drift.bin", totalSize,
		[]struct {
			offset uint64
			data   []byte
		}{{offset: 0, data: expected[:prefixSize]}},
		false,
	)
	foreign := []byte("keep-foreign-drift")
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
	rootAdm := testAdmitRoot(t, session, reIntent, seed+1)
	changed := testMaterializationFileWithIDs(
		t, session.(*Session), reIntent,
		testID16[catalog.FileID](seed+2),
		testID16[content.FileRevision](seed+9),
		"drift.bin", totalSize+4096, rootAdm,
	)
	start, err := session.BeginFile(ctx, changed)
	if err != nil {
		t.Fatal(err)
	}
	settled, immediate := start.ImmediateSettlement()
	_, reason, blocked := settled.ItemBlock()
	if !immediate || settled.Kind() != transfer.FileItemBlocked || !blocked || reason != transfer.ItemBlockRevisionConflict {
		t.Fatalf("changed revision on occupied path = (immediate=%v, kind=%v, reason=%v), want ItemBlockRevisionConflict", immediate, settled.Kind(), reason)
	}
	if got, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(got, foreign) {
		t.Fatalf("foreign file altered: %q %v", got, err)
	}
}

func TestTransferJobResumeCollisionRequestsNoRemainingRangesAndContinuesOnceCleared(t *testing.T) {
	ctx := context.Background()
	const (
		seed       byte = 0x71
		totalSize       = 16 * 1024
		prefixSize      = 8 * 1024
	)
	targetPayload := makePatternPayload(totalSize, seed)
	siblingPayload := makePatternPayload(4*1024, seed+9)
	root, sel, intent, partPath, finalPath := setupPausedPartialFile(
		t, seed, "big.bin", totalSize,
		[]struct {
			offset uint64
			data   []byte
		}{{offset: 0, data: targetPayload[:prefixSize]}},
		false,
	)
	res, _ := intent.MaterializationPlan().DestinationReservation()
	siblingFinalPath := filepath.Join(root, res.PhysicalName(), "sibling.bin")

	foreign := []byte("foreign-blocking-big")
	if err := os.WriteFile(finalPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}

	targetID := testID16[catalog.FileID](seed + 2)
	targetRev := testID16[content.FileRevision](seed + 3)
	siblingID := testID16[catalog.FileID](seed + 5)
	siblingRev := testID16[content.FileRevision](seed + 6)
	source := newResumeCollisionSpySource(t, intent, []spySourceFile{
		{id: targetID, rev: targetRev, name: "big.bin", payload: targetPayload},
		{id: siblingID, rev: siblingRev, name: "sibling.bin", payload: siblingPayload},
	})

	// Run 1: finalPath is occupied; TransferJob must not request any remaining
	// ranges for big.bin while still downloading sibling.bin and keeping .part.
	run1 := runReopenedTransferJob(t, ctx, root, sel, intent.OperationID(), source)
	if run1.Outcome != transfer.DirectTreeOutcomePartial {
		t.Fatalf("run1 outcome = %v, want Partial (files=%+v)", run1.Outcome, run1.Files)
	}
	if run1.Progress.FileOutcomes.CollisionFiles != 1 || run1.Progress.FileOutcomes.DownloadedFiles != 1 {
		t.Fatalf("run1 outcomes = %+v, want 1 collision and 1 downloaded", run1.Progress.FileOutcomes)
	}
	if got := source.requestedRanges(targetID); len(got) != 0 {
		t.Fatalf("run1 requested remaining ranges for collided file: %+v", got)
	}
	if got := source.requestedRanges(siblingID); !slices.Equal(got, []content.Range{{Offset: 0, End: uint64(len(siblingPayload))}}) {
		t.Fatalf("run1 sibling ranges = %+v, want full sibling", got)
	}
	if got, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(got, foreign) {
		t.Fatalf("run1 altered foreign file: %q %v", got, err)
	}
	if info, err := os.Stat(partPath); err != nil || info.Size() != prefixSize {
		t.Fatalf("run1 lost staged partial: info=%v err=%v", info, err)
	}

	// Run 2: remove the colliding file; the same operation must skip sibling.bin
	// and request only [prefixSize, totalSize) for big.bin.
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}
	source.resetRequests()
	run2 := runReopenedTransferJob(t, ctx, root, sel, intent.OperationID(), source)
	if run2.Outcome != transfer.DirectTreeOutcomeSuccess {
		t.Fatalf("run2 outcome = %v, want Success (files=%+v, term=%v, settle=%v)", run2.Outcome, run2.Files, run2.TerminationCause, run2.SettlementFailure)
	}
	if run2.Progress.FileOutcomes.PreviouslyPublishedFiles != 1 || run2.Progress.FileOutcomes.ResumedFiles != 1 {
		t.Fatalf("run2 outcomes = %+v, want 1 previously published and 1 resumed", run2.Progress.FileOutcomes)
	}
	if got := source.requestedRanges(siblingID); len(got) != 0 {
		t.Fatalf("run2 unexpectedly re-requested delivered sibling: %+v", got)
	}
	if got := source.requestedRanges(targetID); !slices.Equal(got, []content.Range{{Offset: prefixSize, End: totalSize}}) {
		t.Fatalf("run2 target ranges = %+v, want [%d,%d)", got, prefixSize, totalSize)
	}
	if got, err := os.ReadFile(finalPath); err != nil || !bytes.Equal(got, targetPayload) {
		t.Fatalf("run2 target payload mismatch: err=%v len=%d", err, len(got))
	}
	if got, err := os.ReadFile(siblingFinalPath); err != nil || !bytes.Equal(got, siblingPayload) {
		t.Fatalf("run2 sibling payload mismatch: err=%v len=%d", err, len(got))
	}
	opDir := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()))
	if _, err := os.Stat(opDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("operation scratch dir survived completion: %v", err)
	}
}

func assertDestinationCollisionTrace(t *testing.T, stream *diagnostics.Stream, wantRetained uint64) {
	t.Helper()
	for {
		select {
		case rec := <-stream.Records():
			p := rec.Payload()
			if p.Get("runtime_operation").Text() == "recover_file" &&
				p.Get("runtime_decision").Text() == "collision" &&
				p.Get("checkpoint_decision").Text() == "destination_collision" {
				if p.Get("retained_bytes").Uint() != wantRetained || p.Get("reused_bytes").Uint() != 0 {
					t.Fatalf("destination_collision trace bytes = (retained=%d, reused=%d), want (%d, 0)",
						p.Get("retained_bytes").Uint(), p.Get("reused_bytes").Uint(), wantRetained)
				}
				return
			}
		default:
			t.Fatal("expected filesystem_output trace with runtime_decision=collision and checkpoint_decision=destination_collision")
		}
	}
}

type spySourceFile struct {
	id      catalog.FileID
	rev     content.FileRevision
	name    string
	payload []byte
}

type resumeCollisionSpySource struct {
	share   catalog.ShareInstance
	rootDir catalog.DirectoryID
	page    catalog.CatalogPage
	files   map[catalog.FileID]spySourceFile
	descs   map[catalog.FileID]content.FileRevisionDescriptor

	mu       sync.Mutex
	requests map[catalog.FileID][]content.Range
}

func newResumeCollisionSpySource(t *testing.T, intent transfer.ReceiveIntent, files []spySourceFile) *resumeCollisionSpySource {
	t.Helper()
	entries := make([]catalog.Entry, 0, len(files))
	fileMap := make(map[catalog.FileID]spySourceFile, len(files))
	descMap := make(map[catalog.FileID]content.FileRevisionDescriptor, len(files))
	for _, f := range files {
		entry, err := catalog.NewFileEntry(f.id, f.name, uint64(len(f.payload)), catalog.ModifiedTime{})
		if err != nil {
			t.Fatal(err)
		}
		geom, err := content.NewFileGeometry(uint64(len(f.payload)), catalog.DefaultChunkSize)
		if err != nil {
			t.Fatal(err)
		}
		desc, err := content.NewFileRevisionDescriptor(intent.ShareInstance(), f.id, f.rev, geom, catalog.ModifiedTime{})
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
		fileMap[f.id] = f
		descMap[f.id] = desc
	}
	page, _, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: intent.ShareInstance(),
		DirectoryID:   intent.SyntheticRoot(),
		Generation:    testID16[catalog.DirectoryGeneration](0xe1),
		PageIndex:     0,
		Entries:       entries,
		Terminal:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &resumeCollisionSpySource{
		share:    intent.ShareInstance(),
		rootDir:  intent.SyntheticRoot(),
		page:     page,
		files:    fileMap,
		descs:    descMap,
		requests: make(map[catalog.FileID][]content.Range),
	}
}

func (s *resumeCollisionSpySource) OpenDirectoryPages(_ context.Context, dirID catalog.DirectoryID) (catalog.DirectoryPageCursor, error) {
	if dirID != s.rootDir {
		return nil, errors.New("unexpected directory id")
	}
	return &singlePageCursor{page: s.page}, nil
}

func (s *resumeCollisionSpySource) OpenRevision(_ context.Context, fileID catalog.FileID) (transfer.OpenedRevision, error) {
	desc, ok := s.descs[fileID]
	if !ok {
		return transfer.OpenedRevision{}, errors.New("unknown file id")
	}
	var handle transfer.RevisionHandle
	copy(handle[:], fileID.Bytes())
	return transfer.NewOpenedRevision(handle, desc)
}

func (s *resumeCollisionSpySource) ReleaseRevision(context.Context, transfer.RevisionHandle) error {
	return nil
}

func (s *resumeCollisionSpySource) ReadRange(
	ctx context.Context,
	_ transfer.RevisionHandle,
	desc content.FileRevisionDescriptor,
	requested content.Range,
	sink transfer.RangeSink,
) error {
	s.mu.Lock()
	s.requests[desc.FileID()] = append(s.requests[desc.FileID()], requested)
	s.mu.Unlock()
	f, ok := s.files[desc.FileID()]
	if !ok {
		return errors.New("unknown file id")
	}
	return sink.WriteRange(ctx, requested.Offset, f.payload[requested.Offset:requested.End])
}

func (s *resumeCollisionSpySource) requestedRanges(fileID catalog.FileID) []content.Range {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests[fileID])
}

func (s *resumeCollisionSpySource) resetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.requests)
}

type singlePageCursor struct {
	page catalog.CatalogPage
	done bool
}

func (c *singlePageCursor) Next(context.Context) (catalog.CatalogPage, bool, error) {
	if c.done {
		return catalog.CatalogPage{}, false, nil
	}
	c.done = true
	return c.page, true, nil
}

func (c *singlePageCursor) Close() error { return nil }

func runReopenedTransferJob(
	t *testing.T,
	ctx context.Context,
	root string,
	sel transfer.SelectionSpec,
	wantOpID receivecontract.OperationID,
	source *resumeCollisionSpySource,
) transfer.JobResult {
	t.Helper()
	auth, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupReopened {
		t.Fatalf("reopen = (%v, %v)", lookup.Kind(), err)
	}
	op := lookup.Operation()
	intent, _ := op.ReceiveIntent()
	if intent.OperationID() != wantOpID {
		t.Fatalf("reopened operation ID = %x, want %x", intent.OperationID().Bytes(), wantOpID.Bytes())
	}
	jobID, err := transfer.NewTransferJobID()
	if err != nil {
		t.Fatal(err)
	}
	job, err := transfer.NewTransferJob(transfer.TransferJobConfig{
		ReceiveIntent: intent,
		JobID:         jobID,
		Catalog:       source,
		Revisions:     source,
		Blocks:        source,
		Materializer: transfer.DirectTreeMaterializerFunc(func(openCtx context.Context, _ transfer.ReceiveIntent) (transfer.DirectTreeSession, error) {
			return auth.OpenOperation(openCtx, op)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return job.Run(ctx)
}
