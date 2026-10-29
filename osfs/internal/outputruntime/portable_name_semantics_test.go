package outputruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/outputportable"
)

func TestPortableOutputPublishesDistinctDottedINames(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	authority, err := New(Config{RootPath: root, PlatformFactory: outputportable.Open})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	if _, err := authority.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	selection := nativeReservationTestSelection(t, 0x51)
	anchor := incrementalTestIdentity16[catalog.DirectoryID](0x54)
	layout, err := receivecontract.NewCompleteDirectoryResultRoot(anchor, "docs")
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
	sourcePath, _ := ordinaryoutput.NewSourceCatalogPath("docs")
	source := transfer.AuthenticatedSourceDirectory{
		DirectoryID: anchor, Generation: incrementalTestIdentity16[catalog.DirectoryGeneration](0x55),
		SourcePath: sourcePath,
	}
	request, err := transfer.NewDirectoryMaterializationRequest(
		intent, source, ordinaryoutput.SourceNodeSelected, transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := session.AdmitDirectory(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := transfer.NewMaterializedDirectoryClaim(admission, request)
	if err != nil {
		t.Fatal(err)
	}
	var entries []catalog.Entry
	for index, name := range []string{"i.txt", "İ.txt"} {
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
		file := ordinaryResumeMaterializationFile(t, session, intent, descriptor, "docs/"+name, source, admission, parent)
		start, err := session.BeginFile(ctx, file)
		if err != nil {
			t.Fatalf("admit %q: %v", name, err)
		}
		transaction, _, ok := start.Transaction()
		if !ok {
			t.Fatalf("%q unexpectedly settled before download", name)
		}
		if err := transaction.WriteRange(ctx, 0, contents); err != nil {
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
	if settlement, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil ||
		settlement.Kind() != transfer.DirectTreeSettlementSuccess {
		t.Fatalf("finish tree: %v, %v", settlement.Kind(), err)
	}
	for _, name := range []string{"i.txt", "İ.txt"} {
		if actual, err := os.ReadFile(filepath.Join(root, "docs", name)); err != nil || string(actual) != name {
			t.Fatalf("published %q: %q, %v", name, actual, err)
		}
	}
}
