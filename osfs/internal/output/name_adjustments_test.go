//go:build windows || linux || darwin

package output

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/diagnostics"
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

func TestBindDirectoryPersistsMappingOnlyAfterCreationAndRollsBack(t *testing.T) {
	t.Parallel()
	opID := testID16[receivecontract.OperationID](0xb1)
	resID := testID16[receivecontract.DestinationReservationID](0xb2)
	var authRaw [receivecontract.AuthorityRefBytes]byte
	authRaw[0] = 0xb3
	authRef, err := receivecontract.AuthorityRefFromBytes(authRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	art := receivecontract.NewCatalogRootDirectoryTree()
	res, err := receivecontract.NewNativeContainerRootReservation(opID, resID, art, authRef)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("failed create skips persist and trace", func(t *testing.T) {
		t.Parallel()
		rootPath := t.TempDir()
		rootDir, _, err := OpenContainer(rootPath, false)
		if err != nil {
			t.Fatal(err)
		}
		defer rootDir.Close()

		var persisted []NameMapping
		var traced []nameTraceEvent
		mapper, err := newNameMapper(opID, res, nil, func(rec NameMapping) error {
			persisted = append(persisted, rec)
			return nil
		}, func(ev nameTraceEvent) {
			traced = append(traced, ev)
		})
		if err != nil {
			t.Fatal(err)
		}

		boom := errors.New("injected mkdir failure")
		_, err = mapper.bindDirectory(ControlDirName, rootDir, func(*Dir, string) (*Dir, error) {
			return nil, boom
		}, ensureSubdirNoAlias)
		if !errors.Is(err, boom) {
			t.Fatalf("bindDirectory error = %v, want %v", err, boom)
		}
		if len(persisted) != 0 || len(traced) != 0 || len(mapper.mappings) != 0 || len(mapper.claims) != 0 {
			t.Fatalf("failed create mutated state: persisted=%v traced=%v mappings=%v claims=%v",
				persisted, traced, mapper.mappings, mapper.claims)
		}
	})

	t.Run("create race advances to next repair attempt", func(t *testing.T) {
		t.Parallel()
		rootPath := t.TempDir()
		rootDir, _, err := OpenContainer(rootPath, false)
		if err != nil {
			t.Fatal(err)
		}
		defer rootDir.Close()

		wantAttempt1, err := SanitizeCandidate(opID.Bytes(), ControlDirName, 1)
		if err != nil {
			t.Fatal(err)
		}
		var persisted []NameMapping
		var traced []nameTraceEvent
		mapper, err := newNameMapper(opID, res, nil, func(rec NameMapping) error {
			persisted = append(persisted, rec)
			return nil
		}, func(ev nameTraceEvent) {
			traced = append(traced, ev)
		})
		if err != nil {
			t.Fatal(err)
		}

		calls := 0
		dir, err := mapper.bindDirectory(ControlDirName, rootDir, func(parent *Dir, name string) (*Dir, error) {
			calls++
			if calls == 1 {
				return nil, ErrNamespaceCollision
			}
			return createFreshSubdir(parent, name)
		}, ensureSubdirNoAlias)
		if err != nil {
			t.Fatalf("bindDirectory after race = %v", err)
		}
		defer dir.Close()

		if calls != 2 || len(persisted) != 1 || persisted[0].Attempt != 1 ||
			persisted[0].PhysicalComponent != wantAttempt1 || !persisted[0].Directory {
			t.Fatalf("persisted after race = %+v (calls=%d)", persisted, calls)
		}
		if len(traced) != 1 || !traced[0].directory || traced[0].reused || traced[0].attempt != 1 {
			t.Fatalf("traced after race = %+v", traced)
		}
		if info, statErr := os.Stat(filepath.Join(rootPath, wantAttempt1)); statErr != nil || !info.IsDir() {
			t.Fatalf("attempt 1 dir missing: info=%v err=%v", info, statErr)
		}
	})

	t.Run("persist failure rolls back newly created directory", func(t *testing.T) {
		t.Parallel()
		rootPath := t.TempDir()
		rootDir, _, err := OpenContainer(rootPath, false)
		if err != nil {
			t.Fatal(err)
		}
		defer rootDir.Close()

		wantAttempt0, err := SanitizeCandidate(opID.Bytes(), ControlDirName, 0)
		if err != nil {
			t.Fatal(err)
		}
		persistErr := errors.New("injected persist failure")
		var traced []nameTraceEvent
		mapper, err := newNameMapper(opID, res, nil, func(NameMapping) error {
			return persistErr
		}, func(ev nameTraceEvent) {
			traced = append(traced, ev)
		})
		if err != nil {
			t.Fatal(err)
		}

		_, err = mapper.bindDirectory(ControlDirName, rootDir, createFreshSubdir, ensureSubdirNoAlias)
		if !errors.Is(err, persistErr) {
			t.Fatalf("bindDirectory error = %v, want %v", err, persistErr)
		}
		if _, statErr := os.Stat(filepath.Join(rootPath, wantAttempt0)); !os.IsNotExist(statErr) {
			t.Fatalf("rolled-back directory still exists: %v", statErr)
		}
		if len(traced) != 0 || len(mapper.mappings) != 0 || len(mapper.claims) != 0 {
			t.Fatalf("failed persist mutated state: traced=%v mappings=%v claims=%v",
				traced, mapper.mappings, mapper.claims)
		}
	})

	t.Run("reused mapping traces only after reopen succeeds", func(t *testing.T) {
		t.Parallel()
		rootPath := t.TempDir()
		rootDir, _, err := OpenContainer(rootPath, false)
		if err != nil {
			t.Fatal(err)
		}
		defer rootDir.Close()

		comp, err := SanitizeCandidate(opID.Bytes(), ControlDirName, 0)
		if err != nil {
			t.Fatal(err)
		}
		existing := []NameMapping{{
			LogicalPath:       ControlDirName,
			PhysicalComponent: comp,
			Attempt:           0,
			Directory:         true,
		}}
		var traced []nameTraceEvent
		mapper, err := newNameMapper(opID, res, existing, nil, func(ev nameTraceEvent) {
			traced = append(traced, ev)
		})
		if err != nil {
			t.Fatal(err)
		}

		openErr := errors.New("injected reopen failure")
		if _, err := mapper.bindDirectory(ControlDirName, rootDir, createFreshSubdir, func(*Dir, string) (*Dir, error) {
			return nil, openErr
		}); !errors.Is(err, openErr) {
			t.Fatalf("failed reopen error = %v, want %v", err, openErr)
		}
		if len(traced) != 0 {
			t.Fatalf("failed reopen emitted trace: %+v", traced)
		}

		reopened, err := mapper.bindDirectory(ControlDirName, rootDir, createFreshSubdir, ensureSubdirNoAlias)
		if err != nil {
			t.Fatalf("reopen error = %v", err)
		}
		defer reopened.Close()
		if len(traced) != 1 || !traced[0].reused || !traced[0].directory || traced[0].physical != comp {
			t.Fatalf("reopen trace = %+v", traced)
		}
	})
}

func TestAdmitDirectoryFailureExcludesUncreatedDirectoryFromNameAdjustments(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sel := testSelectionSpec(t, 0xb8)
	stream, err := diagnostics.NewStream(32, nil)
	if err != nil {
		t.Fatal(err)
	}

	auth, err := New(Config{RootPath: root, CreateRoot: true, Diagnostics: stream.Sink()})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
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
	res, _ := intent.MaterializationPlan().DestinationReservation()
	destDir := filepath.Join(root, res.PhysicalName())

	// Pre-claim physical name "sub" via an undelivered file mapping so admitting
	// nested directory "sub" triggers compatible-name repair on every OS while
	// keeping the undelivered file excluded from NameAdjustments.
	if err := op.journal.recordName(NameMapping{
		LogicalPath:       res.RequestedName() + "/other",
		PhysicalComponent: "sub",
	}); err != nil {
		t.Fatal(err)
	}

	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}

	rootAdm := testAdmitRoot(t, session, intent, 0xb9)
	rootReq, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: rootAdm.DirectoryID(), Generation: rootAdm.Generation(),
			SourcePath: ordinaryoutput.EmptySourceCatalogPath(),
		},
		ordinaryoutput.SourceNodeSelected,
		transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	rootClaim, err := transfer.NewMaterializedDirectoryClaim(rootAdm, rootReq)
	if err != nil {
		t.Fatal(err)
	}
	childSrc, err := ordinaryoutput.NewSourceCatalogPath("sub")
	if err != nil {
		t.Fatal(err)
	}
	childReq, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: testID16[catalog.DirectoryID](0xba), Generation: testID16[catalog.DirectoryGeneration](0xbb),
			SourcePath: childSrc, ParentAdmission: rootAdm,
		},
		ordinaryoutput.SourceNodeSelected,
		rootClaim,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Failing name persistence must roll back the empty repaired directory and
	// leave NameAdjustments empty after PauseTree.
	op.journal.appendHook = func(kind string) error {
		if kind == "name" {
			return errors.New("injected journal failure")
		}
		return nil
	}
	if _, err := session.AdmitDirectory(ctx, childReq); err == nil {
		t.Fatal("AdmitDirectory succeeded despite journal failure")
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if summary := op.NameAdjustments(); summary.Count != 0 || len(summary.Samples) != 0 {
		t.Fatalf("failed directory appeared in NameAdjustments: %+v", summary)
	}
	entries, err := os.ReadDir(destDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rolled-back directory leaked in destDir: %v (err=%v)", entries, err)
	}

	// Clearing the fault and reopening admits and reports the repaired directory.
	op.journal.appendHook = nil
	reSession, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	reRootAdm := testAdmitRoot(t, reSession, intent, 0xb9)
	reRootClaim, err := transfer.NewMaterializedDirectoryClaim(reRootAdm, rootReq)
	if err != nil {
		t.Fatal(err)
	}
	reChildReq, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: testID16[catalog.DirectoryID](0xba), Generation: testID16[catalog.DirectoryGeneration](0xbb),
			SourcePath: childSrc, ParentAdmission: reRootAdm,
		},
		ordinaryoutput.SourceNodeSelected,
		reRootClaim,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reSession.AdmitDirectory(ctx, reChildReq); err != nil {
		t.Fatalf("retry AdmitDirectory: %v", err)
	}
	if _, err := reSession.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	summary := op.NameAdjustments()
	if summary.Count != 1 || len(summary.Samples) != 1 || summary.Samples[0].LogicalPath != "sub" {
		t.Fatalf("retry NameAdjustments = %+v, want 1 entry for %q", summary, "sub")
	}
	if info, err := os.Stat(filepath.Join(destDir, summary.Samples[0].PhysicalPath)); err != nil || !info.IsDir() {
		t.Fatalf("materialized repaired dir missing: info=%v err=%v", info, err)
	}

	stream.Close()
	dirTraces := 0
	for rec := range stream.Records() {
		if rec.Name() != "filesystem_output" {
			continue
		}
		if rec.Payload().Get("runtime_operation").Text() == "map_name" {
			if got := rec.Payload().Get("runtime_component").Text(); got != "directory" {
				t.Fatalf("map_name component = %q, want directory", got)
			}
			dirTraces++
		}
	}
	if dirTraces != 1 {
		t.Fatalf("map_name trace count = %d, want 1", dirTraces)
	}
}
