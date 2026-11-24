//go:build windows || linux || darwin

package output

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestSummarizeNameAdjustmentsFiltersUnpublishedAndCapsDeterministically(t *testing.T) {
	t.Parallel()
	sel := testSelectionSpec(t, 0x91)
	opID := testID16[receivecontract.OperationID](0x92)
	resID := testID16[receivecontract.DestinationReservationID](0x93)
	var authRaw [receivecontract.AuthorityRefBytes]byte
	authRaw[0] = 0x94
	authRef, err := receivecontract.AuthorityRefFromBytes(authRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	layout, err := receivecontract.NewCompleteDirectoryResultRoot(testID16[catalog.DirectoryID](0x95), "project")
	if err != nil {
		t.Fatal(err)
	}
	art, err := receivecontract.NewResultRootDirectoryTree(layout)
	if err != nil {
		t.Fatal(err)
	}
	res, err := receivecontract.NewNativeNamedEntryReservation(opID, resID, art, authRef, "project", "project", 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(art, res)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(sel, art, plan)
	if err != nil {
		t.Fatal(err)
	}

	if got := summarizeNameAdjustments(intent, nil, nil, false); got.Count != 0 || len(got.Samples) != 0 {
		t.Fatalf("empty mappings = %+v, want zero", got)
	}

	// Include one repaired directory, 25 delivered files (to exercise the cap
	// and lexicographic sort), and one unpublished file that must be excluded.
	mappings := []NameMapping{
		{LogicalPath: "project/PRN", PhysicalComponent: "entry-PRN.windshare-dir0", Directory: true},
		{LogicalPath: "project/unpublished.bad", PhysicalComponent: "unpublished.windshare-skip.bad", Directory: false},
	}
	delivered := make(map[string]struct{}, 25)
	for i := 24; i >= 0; i-- {
		logical := fmt.Sprintf("project/PRN/file-%02d:bad.txt", i)
		phys := fmt.Sprintf("file-%02d-bad.windshare-%02x.txt", i, i)
		mappings = append(mappings, NameMapping{LogicalPath: logical, PhysicalComponent: phys})
		delivered[logical] = struct{}{}
	}

	got := summarizeNameAdjustments(intent, mappings, delivered, true)
	if got.Count != 26 || len(got.Samples) != MaxNameAdjustmentSamples {
		t.Fatalf("summary count=%d samples=%d, want 26 and %d", got.Count, len(got.Samples), MaxNameAdjustmentSamples)
	}
	if got.Samples[0] != (NameAdjustment{LogicalPath: "PRN", PhysicalPath: "entry-PRN.windshare-dir0"}) {
		t.Fatalf("first sample = %+v", got.Samples[0])
	}
	wantSecond := NameAdjustment{
		LogicalPath:  "PRN/file-00:bad.txt",
		PhysicalPath: "entry-PRN.windshare-dir0/file-00-bad.windshare-00.txt",
	}
	if got.Samples[1] != wantSecond {
		t.Fatalf("second sample = %+v, want %+v", got.Samples[1], wantSecond)
	}
	for _, sample := range got.Samples {
		if strings.Contains(sample.LogicalPath, "unpublished") {
			t.Fatalf("unpublished file appeared in summary: %+v", sample)
		}
	}
}

func TestNameAdjustmentsSurviveRestartAndControlDirRetirement(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sel := testSelectionSpec(t, 0x99)

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
	// ResultRoot named ".windshare-tmp" collides with WindShare's control
	// directory on every OS, so top-level compatible-name repair always triggers.
	// Combine it with a pause/restart cycle to verify the summary survives both
	// LookupDelivery skips and control-dir retirement.
	rootDirID := testID16[catalog.DirectoryID](0x9b)
	layout, err := receivecontract.NewCompleteDirectoryResultRoot(rootDirID, ControlDirName)
	if err != nil {
		t.Fatal(err)
	}
	art, err := receivecontract.NewResultRootDirectoryTree(layout)
	if err != nil {
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	res, _ := intent.MaterializationPlan().DestinationReservation()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}

	admitReservedRoot := func(s transfer.DirectTreeSession, in transfer.ReceiveIntent) (transfer.DirectoryAdmission, transfer.DirectoryMaterializationRequest) {
		t.Helper()
		dirSrc, err := ordinaryoutput.NewSourceCatalogPath(ControlDirName)
		if err != nil {
			t.Fatal(err)
		}
		dirReq, err := transfer.NewDirectoryMaterializationRequest(
			in,
			transfer.AuthenticatedSourceDirectory{
				DirectoryID: rootDirID,
				Generation:  testID16[catalog.DirectoryGeneration](0x9c),
				SourcePath:  dirSrc,
			},
			ordinaryoutput.SourceNodeSelected,
			transfer.MaterializedDirectoryClaim{},
		)
		if err != nil {
			t.Fatal(err)
		}
		dirAdm, err := s.AdmitDirectory(ctx, dirReq)
		if err != nil {
			t.Fatal(err)
		}
		return dirAdm, dirReq
	}
	buildChildFile := func(s transfer.DirectTreeSession, in transfer.ReceiveIntent, dirAdm transfer.DirectoryAdmission, dirReq transfer.DirectoryMaterializationRequest, fileID catalog.FileID, revSeed byte, name string, size uint64) transfer.MaterializationFile {
		t.Helper()
		claim, err := transfer.NewMaterializedDirectoryClaim(dirAdm, dirReq)
		if err != nil {
			t.Fatal(err)
		}
		parent, err := transfer.NewDirectoryMaterializationFileParent(
			dirReq.Source().DirectoryID, dirReq.Source().Generation, dirReq.Source().SourcePath, dirAdm, claim,
		)
		if err != nil {
			t.Fatal(err)
		}
		geom, err := content.NewFileGeometry(size, catalog.DefaultChunkSize)
		if err != nil {
			t.Fatal(err)
		}
		desc, err := content.NewFileRevisionDescriptor(
			in.ShareInstance(), fileID, testID16[content.FileRevision](revSeed), geom, catalog.ModifiedTime{},
		)
		if err != nil {
			t.Fatal(err)
		}
		src, err := ordinaryoutput.NewSourceCatalogPath(ControlDirName + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := transfer.NewMaterializationRootRelativePath(dirAdm.Path() + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := transfer.NewMaterializationFile(in, src, rel, desc, s.SessionID(), parent)
		if err != nil {
			t.Fatal(err)
		}
		return file
	}

	dirAdm, dirReq := admitReservedRoot(session, intent)
	firstID := testID16[catalog.FileID](0x9d)
	firstFile := buildChildFile(session, intent, dirAdm, dirReq, firstID, 0x9e, "first.txt", 4)
	startFirst, err := session.BeginFile(ctx, firstFile)
	if err != nil {
		t.Fatal(err)
	}
	txFirst, _, ok := startFirst.Transaction()
	if !ok {
		t.Fatal("expected transaction for first file")
	}
	if err := txFirst.WriteRange(ctx, 0, []byte("done")); err != nil {
		t.Fatal(err)
	}
	if _, err := txFirst.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	secondID := testID16[catalog.FileID](0xa1)
	secondFile := buildChildFile(session, intent, dirAdm, dirReq, secondID, 0xa2, "second.txt", 8)
	startSecond, err := session.BeginFile(ctx, secondFile)
	if err != nil {
		t.Fatal(err)
	}
	txSecond, _, ok := startSecond.Transaction()
	if !ok {
		t.Fatal("expected transaction for second file")
	}
	if err := txSecond.WriteRange(ctx, 0, []byte("part")); err != nil {
		t.Fatal(err)
	}
	if _, err := txSecond.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	pausedSummary := op.NameAdjustments()
	if pausedSummary.Count != 1 || len(pausedSummary.Samples) != 1 ||
		pausedSummary.Samples[0].LogicalPath != ControlDirName ||
		pausedSummary.Samples[0].PhysicalPath != res.PhysicalName() {
		t.Fatalf("paused adjustments = %+v", pausedSummary)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	// Run 2 skips first.txt via LookupDelivery, finishes second.txt, and deletes
	// .windshare-tmp on FinalizeTree(Success); the summary must remain readable.
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
	reOp := reLookup.Operation()
	reIntent, _ := reOp.ReceiveIntent()
	reSession, err := reopen.OpenOperation(ctx, reOp)
	if err != nil {
		t.Fatal(err)
	}
	reDirAdm, reDirReq := admitReservedRoot(reSession, reIntent)
	history, ok := reSession.(transfer.DeliveryHistory)
	if !ok {
		t.Fatal("reopened session does not implement transfer.DeliveryHistory")
	}
	if _, delivered, err := history.LookupDelivery(ctx, firstID, firstFile.MaterializationRelativePath().String()); err != nil || !delivered {
		t.Fatalf("LookupDelivery = (%t, %v)", delivered, err)
	}
	reSecond := buildChildFile(reSession, reIntent, reDirAdm, reDirReq, secondID, 0xa2, "second.txt", 8)
	reStart, err := reSession.BeginFile(ctx, reSecond)
	if err != nil {
		t.Fatal(err)
	}
	reTx, _, ok := reStart.Transaction()
	if !ok {
		t.Fatal("expected resumed transaction")
	}
	if err := reTx.WriteRange(ctx, 4, []byte("done")); err != nil {
		t.Fatal(err)
	}
	if _, err := reTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := reSession.FinalizeDirectory(ctx, reDirAdm); err != nil {
		t.Fatal(err)
	}
	if _, err := reSession.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
		t.Fatal(err)
	}

	finalSummary := reOp.NameAdjustments()
	if finalSummary.Count != 1 || len(finalSummary.Samples) != 1 {
		t.Fatalf("final adjustments = %+v", finalSummary)
	}
	sample := finalSummary.Samples[0]
	if sample.LogicalPath != ControlDirName || sample.PhysicalPath != res.PhysicalName() {
		t.Fatalf("final sample = %+v, want %q -> %q", sample, ControlDirName, res.PhysicalName())
	}
	if got, err := os.ReadFile(filepath.Join(root, sample.PhysicalPath, "first.txt")); err != nil || string(got) != "done" {
		t.Fatalf("repaired physical file = (%q, %v)", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, sample.PhysicalPath, "second.txt")); err != nil || string(got) != "partdone" {
		t.Fatalf("resumed physical file = (%q, %v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, ControlDirName)); !os.IsNotExist(err) {
		t.Fatalf("control directory survived completion: %v", err)
	}
}
