//go:build windows || linux || darwin

package output

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	longStreamingFileSize  = uint64(32 * 1024 * 1024)
	longStreamingBlockSize = uint64(1024 * 1024)
)

func TestLongNativeOrdinaryRestartResumesLargeStreamingFile(t *testing.T) {
	if testing.Short() {
		t.Skip("long native restart scenario")
	}
	ctx := context.Background()
	root := t.TempDir()
	sel := testSelectionSpec(t, 0x71)

	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
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
	rootAdm := testAdmitRoot(t, session, intent, 0x72)
	file := testMaterializationFile(t, session, intent, 0x73, "stream.bin", longStreamingFileSize, rootAdm)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatal("missing initial transaction")
	}
	block := bytes.Repeat([]byte{0xa7}, int(longStreamingBlockSize))
	restartCut := longStreamingFileSize / 2
	for offset := uint64(0); offset < restartCut; offset += longStreamingBlockSize {
		if err := tx.WriteRange(ctx, offset, block); err != nil {
			t.Fatal(err)
		}
		// Large files auto-checkpoint every 8 MiB via Accept.
		if _, err := tx.Accept(ctx); err != nil {
			t.Fatal(err)
		}
	}
	durable, err := tx.Checkpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := durable.Ranges().Ranges(); len(got) != 1 || got[0] != (content.Range{Offset: 0, End: restartCut}) {
		t.Fatalf("durable ranges = %+v", got)
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

	// Reopen and finish with descending WriteAt blocks.
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
	reRootAdm := testAdmitRoot(t, reSession, reIntent, 0x72)
	reFile := testMaterializationFile(t, reSession, reIntent, 0x73, "stream.bin", longStreamingFileSize, reRootAdm)
	reStart, err := reSession.BeginFile(ctx, reFile)
	if err != nil {
		t.Fatal(err)
	}
	reTx, reDurable, ok := reStart.Transaction()
	if !ok {
		t.Fatal("reopened partial did not yield transaction")
	}
	if got := reDurable.Ranges().Ranges(); len(got) != 1 || got[0] != (content.Range{Offset: 0, End: restartCut}) {
		t.Fatalf("reopened ranges = %+v", got)
	}
	for offset := int64(longStreamingFileSize - longStreamingBlockSize); offset >= int64(restartCut); offset -= int64(longStreamingBlockSize) {
		if err := reTx.WriteRange(ctx, uint64(offset), block); err != nil {
			t.Fatal(err)
		}
	}
	settled, err := reTx.Commit(ctx)
	if err != nil || settled.Kind() != transfer.FilePublished {
		t.Fatalf("large commit = (%v, %v)", settled.Kind(), err)
	}
	if _, err := reSession.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	res, _ := reIntent.MaterializationPlan().DestinationReservation()
	finalPath := filepath.Join(root, res.PhysicalName(), "stream.bin")
	assertRepeatedBlockFile(t, finalPath, block, longStreamingFileSize/longStreamingBlockSize)
}

func TestLongNativeOrdinaryRestartReusesPublishedFile(t *testing.T) {
	if testing.Short() {
		t.Skip("long native restart scenario")
	}
	ctx := context.Background()
	root := t.TempDir()
	sel := testSelectionSpec(t, 0x79)
	payload := []byte("already-published")

	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
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
	rootAdm := testAdmitRoot(t, session, intent, 0x7a)
	file := testMaterializationFile(t, session, intent, 0x7b, "done.bin", uint64(len(payload)), rootAdm)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, _ := start.Transaction()
	if err := tx.WriteRange(ctx, 0, payload); err != nil {
		t.Fatal(err)
	}
	if settled, err := tx.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
		t.Fatalf("commit = (%v, %v)", settled.Kind(), err)
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
	reRootAdm := testAdmitRoot(t, reSession, reIntent, 0x7a)
	reFile := testMaterializationFile(t, reSession, reIntent, 0x7b, "done.bin", uint64(len(payload)), reRootAdm)
	reStart, err := reSession.BeginFile(ctx, reFile)
	if err != nil {
		t.Fatal(err)
	}
	settled, ok := reStart.ImmediateSettlement()
	if !ok || settled.Kind() != transfer.FilePreviouslyPublished {
		t.Fatalf("reopen published = (%+v, ok=%t)", settled, ok)
	}
	res, _ := reIntent.MaterializationPlan().DestinationReservation()
	actual, err := os.ReadFile(filepath.Join(root, res.PhysicalName(), "done.bin"))
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("reused final = %q, %v", actual, err)
	}
	if _, err := reSession.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
}

func assertRepeatedBlockFile(t *testing.T, path string, block []byte, count uint64) {
	t.Helper()
	expected := sha256.New()
	for range count {
		_, _ = expected.Write(block)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	actual := sha256.New()
	written, copyErr := io.Copy(actual, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written != int64(uint64(len(block))*count) ||
		!bytes.Equal(actual.Sum(nil), expected.Sum(nil)) {
		t.Fatalf("streamed final = bytes %d, copy %v, close %v", written, copyErr, closeErr)
	}
}
