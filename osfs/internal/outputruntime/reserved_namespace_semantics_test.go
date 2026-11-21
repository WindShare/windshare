package outputruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputportable"
)

func TestPortableOutputSeparatesResultRootAndContainerReservedNamespaces(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	authority, err := New(Config{RootPath: root, PlatformFactory: outputportable.Open})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	if _, err := authority.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}

	anchor := incrementalTestIdentity16[catalog.DirectoryID](0x21)
	layout, err := receivecontract.NewCompleteDirectoryResultRoot(anchor, "proj")
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(layout)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(ctx, nativeReservationTestSelection(t, 0x22))
	if err != nil {
		t.Fatal(err)
	}
	operation, err := authority.CreateOperation(ctx, lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("missing intent")
	}
	session, err := authority.OpenOperation(ctx, operation)
	if err != nil {
		t.Fatal(err)
	}

	rootSource, rootAdmission, rootClaim := admitTestDirectory(t, ctx, session, intent, anchor, 0x23, "proj", transfer.DirectoryAdmission{}, transfer.MaterializedDirectoryClaim{})
	wsDirSource, wsDirAdmission, wsDirClaim := admitTestDirectory(t, ctx, session, intent, incrementalTestIdentity16[catalog.DirectoryID](0x24), 0x25, "proj/.wsresume-dir", rootAdmission, rootClaim)
	subSource, subAdmission, subClaim := admitTestDirectory(t, ctx, session, intent, incrementalTestIdentity16[catalog.DirectoryID](0x26), 0x27, "proj/sub", rootAdmission, rootClaim)

	type fileCase struct {
		path      string
		source    transfer.AuthenticatedSourceDirectory
		admission transfer.DirectoryAdmission
		parent    transfer.MaterializedDirectoryClaim
		repaired  bool
	}
	forwardedMarker := ".wsresume-" + strings.Repeat("0", 64) + ".identity"
	cases := []fileCase{
		{path: "proj/.wsresume-notes.txt", source: rootSource, admission: rootAdmission, parent: rootClaim, repaired: true},
		{path: "proj/.wſresume.txt", source: rootSource, admission: rootAdmission, parent: rootClaim, repaired: true},
		{path: "proj/" + forwardedMarker, source: rootSource, admission: rootAdmission, parent: rootClaim, repaired: true},
		{path: "proj/.windshare-output", source: rootSource, admission: rootAdmission, parent: rootClaim, repaired: false},
		{path: "proj/.wsresume-dir/inside.txt", source: wsDirSource, admission: wsDirAdmission, parent: wsDirClaim, repaired: true},
		{path: "proj/sub/.wsresume-notes.txt", source: subSource, admission: subAdmission, parent: subClaim, repaired: false},
	}

	resolved := make(map[string]string, len(cases))
	for index, tc := range cases {
		contents := []byte("payload:" + tc.path)
		fileID := incrementalTestIdentity16[catalog.FileID](byte(0x40 + index))
		geometry, err := content.NewFileGeometry(uint64(len(contents)), catalog.DefaultChunkSize)
		if err != nil {
			t.Fatal(err)
		}
		descriptor, err := content.NewFileRevisionDescriptor(
			intent.ShareInstance(), fileID, incrementalTestIdentity16[content.FileRevision](byte(0x50+index)),
			geometry, catalog.ModifiedTime{},
		)
		if err != nil {
			t.Fatal(err)
		}
		file := ordinaryResumeMaterializationFile(t, session, intent, descriptor, tc.path, tc.source, tc.admission, tc.parent)
		start, err := session.BeginFile(ctx, file)
		if err != nil {
			t.Fatalf("BeginFile(%q): %v", tc.path, err)
		}
		physical, err := operation.names.Resolve(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		resolved[tc.path] = physical
		tx, _, ok := start.Transaction()
		if !ok {
			t.Fatalf("%q unexpectedly settled", tc.path)
		}
		if err := tx.WriteRange(ctx, 0, contents); err != nil {
			t.Fatal(err)
		}
		if settlement, err := tx.Commit(ctx); err != nil || settlement.Kind() != transfer.FilePublished {
			t.Fatalf("Commit(%q): %v, %v", tc.path, settlement.Kind(), err)
		}
	}

	for _, admission := range []transfer.DirectoryAdmission{wsDirAdmission, subAdmission, rootAdmission} {
		if _, err := session.FinalizeDirectory(ctx, admission); err != nil {
			t.Fatal(err)
		}
	}
	if settlement, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil ||
		settlement.Kind() != transfer.DirectTreeSettlementSuccess {
		t.Fatalf("FinalizeTree: %v, %v", settlement.Kind(), err)
	}

	repairedCount, reportName := operation.CompatibleNameReport()
	// 3 direct .wsresume* files + 1 direct .wsresume-dir directory = 4 repaired mappings.
	if repairedCount != 4 || !strings.HasPrefix(reportName, "windshare-names-") {
		t.Fatalf("CompatibleNameReport() = (%d, %q), want (4, windshare-names-*.json)", repairedCount, reportName)
	}
	assertNativeNameReport(t, filepath.Join(root, reportName), "proj", "proj", 4)

	for _, tc := range cases {
		physical := resolved[tc.path]
		wantRelative := strings.TrimPrefix(tc.path, "proj/")
		if tc.repaired {
			if physical == wantRelative {
				t.Fatalf("%q was not repaired: %q", tc.path, physical)
			}
			first, _, _ := strings.Cut(physical, "/")
			if outputcap.ReservedRootName(outputcap.AuthorityCreatedRoot, first) {
				t.Fatalf("%q remained in reserved result-root namespace: %q", tc.path, physical)
			}
		} else if physical != wantRelative {
			t.Fatalf("%q was unexpectedly renamed to %q", tc.path, physical)
		}
		diskPath := filepath.Join(root, "proj", filepath.FromSlash(physical))
		actual, err := os.ReadFile(diskPath)
		if err != nil || string(actual) != "payload:"+tc.path {
			t.Fatalf("disk %q = %q, %v", diskPath, actual, err)
		}
	}
}

func TestPortableOutputRepairsTopLevelControlNameAndPreservesTopLevelMarkerName(t *testing.T) {
	t.Run("container control root name is repaired", func(t *testing.T) {
		ctx := context.Background()
		root := t.TempDir()
		authority, err := New(Config{RootPath: root, PlatformFactory: outputportable.Open})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = authority.Close() })
		if _, err := authority.BindDestination(ctx); err != nil {
			t.Fatal(err)
		}
		anchor := incrementalTestIdentity16[catalog.DirectoryID](0x61)
		layout, err := receivecontract.NewCompleteDirectoryResultRoot(anchor, ".windshare-output")
		if err != nil {
			t.Fatal(err)
		}
		artifact, err := receivecontract.NewResultRootDirectoryTree(layout)
		if err != nil {
			t.Fatal(err)
		}
		lookup, err := authority.LookupActive(ctx, nativeReservationTestSelection(t, 0x62))
		if err != nil {
			t.Fatal(err)
		}
		operation, err := authority.CreateOperation(ctx, lookup, artifact)
		if err != nil {
			t.Fatal(err)
		}
		physicalRoot := operation.topLevel.ReservedEntry().PhysicalName()
		if outputcap.ReservedRootName(outputcap.CallerProvidedContainer, physicalRoot) || !strings.Contains(physicalRoot, ".windshare-") {
			t.Fatalf("repaired root = %q", physicalRoot)
		}
		intent, _ := operation.ReceiveIntent()
		session, err := authority.OpenOperation(ctx, operation)
		if err != nil {
			t.Fatal(err)
		}
		rootSource, rootAdmission, rootClaim := admitTestDirectory(t, ctx, session, intent, anchor, 0x63, ".windshare-output", transfer.DirectoryAdmission{}, transfer.MaterializedDirectoryClaim{})
		publishSmallTestFile(t, ctx, session, intent, 0x64, ".windshare-output/ok.txt", rootSource, rootAdmission, rootClaim)
		if _, err := session.FinalizeDirectory(ctx, rootAdmission); err != nil {
			t.Fatal(err)
		}
		if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
			t.Fatal(err)
		}
		count, reportName := operation.CompatibleNameReport()
		if count != 1 || reportName == "" {
			t.Fatalf("CompatibleNameReport() = (%d, %q), want (1, non-empty)", count, reportName)
		}
		assertNativeNameReport(t, filepath.Join(root, reportName), ".windshare-output", physicalRoot, 0)
		if got, err := os.ReadFile(filepath.Join(root, physicalRoot, "ok.txt")); err != nil || string(got) != "ok" {
			t.Fatalf("delivered file = %q, %v", got, err)
		}
	})

	t.Run("top-level .wsresume single file stays verbatim", func(t *testing.T) {
		ctx := context.Background()
		root := t.TempDir()
		fixture := openNamedResumeFile(t, root, 0x71, 2, false, ".wsresume-notes.txt", outputportable.Open, nil)
		t.Cleanup(func() { _ = fixture.authority.Close() })
		if got := filepath.Base(fixture.finalPath); got != ".wsresume-notes.txt" {
			t.Fatalf("physical name = %q", got)
		}
		if err := fixture.transaction.WriteRange(ctx, 0, []byte("ok")); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.transaction.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(root, ".wsresume-notes.txt")); err != nil || string(got) != "ok" {
			t.Fatalf("saved file = %q, %v", got, err)
		}
		if reports, err := filepath.Glob(filepath.Join(root, "windshare-names-*.json")); err != nil || len(reports) != 0 {
			t.Fatalf("unexpected compatible-name reports: %v, %v", reports, err)
		}
	})

	t.Run("top-level .windshare-output single file is repaired", func(t *testing.T) {
		ctx := context.Background()
		root := t.TempDir()
		fixture := openNamedResumeFile(t, root, 0x81, 2, false, ".windshare-output", outputportable.Open, nil)
		t.Cleanup(func() { _ = fixture.authority.Close() })
		savedBase := filepath.Base(fixture.finalPath)
		if outputcap.ReservedRootName(outputcap.CallerProvidedContainer, savedBase) || !strings.Contains(savedBase, ".windshare-") {
			t.Fatalf("single-file control name was not repaired: %q", savedBase)
		}
		if err := fixture.transaction.WriteRange(ctx, 0, []byte("ok")); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.transaction.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(fixture.finalPath); err != nil || string(got) != "ok" {
			t.Fatalf("saved file = %q, %v", got, err)
		}
		reports, err := filepath.Glob(filepath.Join(root, "windshare-names-*.json"))
		if err != nil || len(reports) != 1 {
			t.Fatalf("name reports = %v, %v", reports, err)
		}
		assertNativeNameReport(t, reports[0], ".windshare-output", savedBase, 0)
	})
}

func admitTestDirectory(
	t *testing.T,
	ctx context.Context,
	session transfer.DirectTreeSession,
	intent transfer.ReceiveIntent,
	directoryID catalog.DirectoryID,
	genSeed byte,
	path string,
	parentAdmission transfer.DirectoryAdmission,
	parentClaim transfer.MaterializedDirectoryClaim,
) (transfer.AuthenticatedSourceDirectory, transfer.DirectoryAdmission, transfer.MaterializedDirectoryClaim) {
	t.Helper()
	sourcePath, err := ordinaryoutput.NewSourceCatalogPath(path)
	if err != nil {
		t.Fatal(err)
	}
	source := transfer.AuthenticatedSourceDirectory{
		DirectoryID:     directoryID,
		Generation:      incrementalTestIdentity16[catalog.DirectoryGeneration](genSeed),
		ParentAdmission: parentAdmission,
		SourcePath:      sourcePath,
	}
	request, err := transfer.NewDirectoryMaterializationRequest(intent, source, ordinaryoutput.SourceNodeSelected, parentClaim)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := session.AdmitDirectory(ctx, request)
	if err != nil {
		t.Fatalf("AdmitDirectory(%q): %v", path, err)
	}
	claim, err := transfer.NewMaterializedDirectoryClaim(admission, request)
	if err != nil {
		t.Fatal(err)
	}
	return source, admission, claim
}

func publishSmallTestFile(
	t *testing.T,
	ctx context.Context,
	session transfer.DirectTreeSession,
	intent transfer.ReceiveIntent,
	seed byte,
	path string,
	parentSource transfer.AuthenticatedSourceDirectory,
	parentAdmission transfer.DirectoryAdmission,
	parentClaim transfer.MaterializedDirectoryClaim,
) {
	t.Helper()
	fileID := incrementalTestIdentity16[catalog.FileID](seed)
	geometry, err := content.NewFileGeometry(2, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(), fileID, incrementalTestIdentity16[content.FileRevision](seed+1),
		geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	file := ordinaryResumeMaterializationFile(t, session, intent, descriptor, path, parentSource, parentAdmission, parentClaim)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatalf("BeginFile(%q): %v", path, err)
	}
	tx, _, _ := start.Transaction()
	if err := tx.WriteRange(ctx, 0, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
