package outputruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/outputnames"
	"github.com/windshare/windshare/osfs/internal/outputportable"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func TestPortableOutputPublishesDistinctDottedINames(t *testing.T) {
	testPortableOutputNames(t, "docs", "", []string{"i.txt", "İ.txt"})
}

func TestPortableOutputPreservesOrRepairsDestinationNames(t *testing.T) {
	for _, rootName := range []string{"docs", "folder?"} {
		t.Run(rootName, func(t *testing.T) {
			testPortableOutputNames(t, rootName, "subdir?", []string{
				"CON .txt", "LPT¹.log", "NUL ", "ordinary.txt", "question?.txt", "trailing.", "zzz-following.txt",
			})
		})
	}
}

func testPortableOutputNames(t *testing.T, rootName, subdir string, names []string) {
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
	selection := nativeReservationTestSelection(t, 0x51)
	anchor := incrementalTestIdentity16[catalog.DirectoryID](0x54)
	layout, err := receivecontract.NewCompleteDirectoryResultRoot(anchor, rootName)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(layout)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(ctx, selection)
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
	t.Cleanup(func() { _, _ = session.PauseTree(ctx, transfer.JobPauseInterrupted) })
	var source transfer.AuthenticatedSourceDirectory
	var admission, rootAdmission transfer.DirectoryAdmission
	var parent transfer.MaterializedDirectoryClaim
	var sourcePrefix string
	admitDirectories := func() {
		sourcePath, _ := ordinaryoutput.NewSourceCatalogPath(rootName)
		source = transfer.AuthenticatedSourceDirectory{
			DirectoryID: anchor, Generation: incrementalTestIdentity16[catalog.DirectoryGeneration](0x55),
			SourcePath: sourcePath,
		}
		request, err := transfer.NewDirectoryMaterializationRequest(
			intent, source, ordinaryoutput.SourceNodeSelected, transfer.MaterializedDirectoryClaim{},
		)
		if err != nil {
			t.Fatal(err)
		}
		admission, err = session.AdmitDirectory(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		parent, err = transfer.NewMaterializedDirectoryClaim(admission, request)
		if err != nil {
			t.Fatal(err)
		}
		rootAdmission = admission
		sourcePrefix = rootName
		if subdir != "" {
			sourcePrefix += "/" + subdir
			childPath, _ := ordinaryoutput.NewSourceCatalogPath(sourcePrefix)
			source = transfer.AuthenticatedSourceDirectory{
				DirectoryID:     incrementalTestIdentity16[catalog.DirectoryID](0x57),
				Generation:      incrementalTestIdentity16[catalog.DirectoryGeneration](0x58),
				ParentAdmission: admission, SourcePath: childPath,
			}
			childRequest, err := transfer.NewDirectoryMaterializationRequest(intent, source, ordinaryoutput.SourceNodeSelected, parent)
			if err != nil {
				t.Fatal(err)
			}
			admission, err = session.AdmitDirectory(ctx, childRequest)
			if err != nil {
				t.Fatal(err)
			}
			parent, err = transfer.NewMaterializedDirectoryClaim(admission, childRequest)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	admitDirectories()
	physicalRoot := operation.topLevel.ReservedEntry().PhysicalName()
	physicalNames := make(map[string]string)
	var entries []catalog.Entry
	for index, name := range names {
		contents := []byte(name)
		fileID := incrementalTestIdentity16[catalog.FileID](byte(0x60 + index))
		entry, err := catalog.NewFileEntry(fileID, name, uint64(len(contents)), catalog.ModifiedTime{})
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
		geometry, err := content.NewFileGeometry(uint64(len(contents)), catalog.DefaultChunkSize)
		if err != nil {
			t.Fatal(err)
		}
		descriptor, err := content.NewFileRevisionDescriptor(
			intent.ShareInstance(), fileID, incrementalTestIdentity16[content.FileRevision](byte(0x70+index)),
			geometry, catalog.ModifiedTime{},
		)
		if err != nil {
			t.Fatal(err)
		}
		file := ordinaryResumeMaterializationFile(t, session, intent, descriptor, sourcePrefix+"/"+name, source, admission, parent)
		start, err := session.BeginFile(ctx, file)
		if err == nil {
			physical, resolveErr := operation.names.Resolve(sourcePrefix + "/" + name)
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			physicalNames[name] = physical
		}
		if err != nil {
			t.Fatalf("admit %q: %v", name, err)
		}
		transaction, _, ok := start.Transaction()
		if !ok {
			t.Fatalf("%q unexpectedly settled before download", name)
		}
		offset := uint64(0)
		if subdir != "" && name == "question?.txt" {
			const prefixBytes = 2
			if err := transaction.WriteRange(ctx, 0, contents[:prefixBytes]); err != nil {
				t.Fatal(err)
			}
			if _, err := transaction.Checkpoint(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := transaction.Pause(ctx, transfer.FilePauseInterrupted); err != nil {
				t.Fatal(err)
			}
			if _, err := session.PauseTree(ctx, transfer.JobPauseInterrupted); err != nil {
				t.Fatal(err)
			}
			if err := authority.Close(); err != nil {
				t.Fatal(err)
			}

			repository, err := NewNativeResumeRepository(root, outputportable.Open, nil)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := repository.Acquire(ctx, intent.OperationID())
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := lease.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			foundPartial := false
			for _, item := range snapshot.Items() {
				if item.CanonicalPath() == sourcePrefix+"/"+name {
					foundPartial = item.State() == resumeauthority.ItemResumable
				}
			}
			if !foundPartial {
				t.Fatal("renamed partial disappeared from resume inventory")
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}

			authority, err = New(Config{RootPath: root, PlatformFactory: outputportable.Open})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := authority.BindDestination(ctx); err != nil {
				t.Fatal(err)
			}
			lookup, err := authority.LookupActive(ctx, selection)
			if err != nil || lookup.Kind() != ActiveLookupReopened {
				t.Fatalf("reopen = %v, %v", lookup.Kind(), err)
			}
			operation = lookup.Operation()
			resumedIntent, ok := operation.ReceiveIntent()
			if !ok || !resumedIntent.EqualCanonical(intent) {
				t.Fatal("resume changed the frozen result name")
			}
			session, err = authority.OpenOperation(ctx, operation)
			if err != nil {
				t.Fatal(err)
			}
			admitDirectories()
			file = ordinaryResumeMaterializationFile(t, session, intent, descriptor, sourcePrefix+"/"+name, source, admission, parent)
			start, err = session.BeginFile(ctx, file)
			if err != nil {
				t.Fatal(err)
			}
			var durable transfer.VerifiedRanges
			transaction, durable, ok = start.Transaction()
			ranges := durable.Ranges().Ranges()
			if !ok || len(ranges) != 1 || ranges[0] != (content.Range{Offset: 0, End: prefixBytes}) {
				t.Fatalf("resume lost verified bytes: %+v", ranges)
			}
			physical, err := operation.names.Resolve(sourcePrefix + "/" + name)
			if err != nil || physical != physicalNames[name] {
				t.Fatalf("resume changed filename: %q, %v", physical, err)
			}
			offset = prefixBytes
		}
		if err := transaction.WriteRange(ctx, offset, contents[offset:]); err != nil {
			t.Fatal(err)
		}
		if settlement, err := transaction.Commit(ctx); err != nil || settlement.Kind() != transfer.FilePublished {
			t.Fatalf("publish %q: %v, %v", name, settlement.Kind(), err)
		}
	}
	// The regression must remain reachable through authenticated catalog policy.
	if _, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: intent.ShareInstance(), DirectoryID: source.DirectoryID, Generation: source.Generation,
		Entries: entries, Terminal: true,
	}, catalog.PageCommitterFunc(func(catalog.PageCommitInput) (catalog.PageCommitment, error) {
		return catalog.PageCommitment{1}, nil
	})); err != nil {
		t.Fatalf("test names are not a valid catalog: %v", err)
	}
	if _, err := session.FinalizeDirectory(ctx, admission); err != nil {
		t.Fatal(err)
	}
	if subdir != "" {
		if _, err := session.FinalizeDirectory(ctx, rootAdmission); err != nil {
			t.Fatal(err)
		}
	}
	mappings := operation.names.Mappings()
	if settlement, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil ||
		settlement.Kind() != transfer.DirectTreeSettlementSuccess {
		t.Fatalf("finish tree: %v, %v", settlement.Kind(), err)
	}
	if runtime.GOOS == "windows" && subdir != "" && len(mappings) < 4 {
		for _, mapping := range mappings {
			t.Log(mapping.LogicalPath)
		}
		t.Fatalf("repair mapping count = %d", len(mappings))
	}
	if runtime.GOOS != "windows" && len(mappings) != 0 {
		t.Fatalf("supported source names were changed: %+v", mappings)
	}
	for _, name := range names {
		if runtime.GOOS == "windows" && !outputnames.PortableName(name) && !strings.Contains(physicalNames[name], ".windshare-") {
			t.Fatalf("restricted name not repaired: %q", physicalNames[name])
		}
		if actual, err := os.ReadFile(filepath.Join(root, physicalRoot, filepath.FromSlash(physicalNames[name]))); err != nil || string(actual) != name {
			t.Fatalf("published %q: %q, %v", name, actual, err)
		}
	}
	reports, err := filepath.Glob(filepath.Join(root, "windshare-names-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) == 0 && physicalRoot == rootName {
		if len(reports) != 0 {
			t.Fatalf("ordinary names produced a report: %v", reports)
		}
		return
	}
	if len(reports) != 1 {
		t.Fatalf("name reports = %v", reports)
	}
	encoded, err := os.ReadFile(reports[0])
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		OriginalRoot string
		SavedRoot    string
		Entries      []struct{ OriginalPath, SavedPath string }
	}
	if err := json.Unmarshal(encoded, &report); err != nil {
		t.Fatal(err)
	}
	if report.OriginalRoot != rootName || report.SavedRoot != physicalRoot || len(report.Entries) != len(mappings) {
		t.Fatalf("incomplete name report: %s", encoded)
	}
	for _, entry := range report.Entries {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(entry.SavedPath))); err != nil {
			t.Fatal(err)
		}
	}
}
