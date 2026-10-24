//go:build windows || linux

package osfs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestNativeDirectTreeHostSemantics(t *testing.T) {
	t.Run("zero byte publishes without a public placeholder", func(t *testing.T) {
		root := t.TempDir()
		intent, session, rootAdmission := openNativeDirectTreeTestSession(t, root, 0xb1)
		resultRoot := nativeDirectTreeResultRoot(t, root, intent)
		file := nativeDirectTreeTestFile(t, session, intent, 0xb3, "empty.bin", 0, rootAdmission)
		start, err := session.BeginFile(context.Background(), file)
		if err != nil {
			t.Fatal(err)
		}
		transaction, durable, ok := start.Transaction()
		if !ok || !durable.Ranges().IsEmpty() {
			t.Fatalf("zero-byte start = (transaction=%T, ranges=%v)", transaction, durable.Ranges().Ranges())
		}
		settlement, err := transaction.Commit(context.Background())
		if err != nil || settlement.Kind() != transfer.FilePublished {
			t.Fatalf("zero-byte commit = (%d, %v)", settlement.Kind(), err)
		}
		if _, err := session.FinalizeDirectory(context.Background(), rootAdmission); err != nil {
			t.Fatal(err)
		}
		tree, err := session.FinalizeTree(context.Background(), transfer.DirectTreeOutcomeSuccess)
		if err != nil || tree.Kind() != transfer.DirectTreeSettlementSuccess {
			t.Fatalf("zero-byte tree = (%d, %v)", tree.Kind(), err)
		}
		info, err := os.Stat(filepath.Join(resultRoot, "empty.bin"))
		if err != nil || info.Size() != 0 {
			t.Fatalf("zero-byte output = (%v, %v)", info, err)
		}
		if names := nativeDirectTreePublicNames(t, root); !slices.Equal(names, []string{filepath.Base(resultRoot)}) {
			t.Fatalf("zero-byte result roots = %v", names)
		}
		if names := nativeDirectTreePublicNames(t, resultRoot); !slices.Equal(names, []string{"empty.bin"}) {
			t.Fatalf("zero-byte result entries = %v", names)
		}
	})

	t.Run("collision is isolated and successful prefix remains partial", func(t *testing.T) {
		root := t.TempDir()
		intent, session, rootAdmission := openNativeDirectTreeTestSession(t, root, 0xc1)
		resultRoot := nativeDirectTreeResultRoot(t, root, intent)
		if err := os.WriteFile(filepath.Join(resultRoot, "collision.bin"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		success := nativeDirectTreeTestFile(t, session, intent, 0xc3, "success.bin", 4, rootAdmission)
		start, err := session.BeginFile(context.Background(), success)
		if err != nil {
			t.Fatal(err)
		}
		transaction, _, ok := start.Transaction()
		if !ok {
			t.Fatalf("successful member settled before transfer: %+v", start)
		}
		if err := transaction.WriteRange(context.Background(), 0, []byte("good")); err != nil {
			t.Fatal(err)
		}
		if settlement, err := transaction.Commit(context.Background()); err != nil ||
			settlement.Kind() != transfer.FilePublished {
			t.Fatalf("successful member commit = (%d, %v)", settlement.Kind(), err)
		}

		collision := nativeDirectTreeTestFile(t, session, intent, 0xc5, "collision.bin", 4, rootAdmission)
		collisionStart, err := session.BeginFile(context.Background(), collision)
		if err != nil {
			t.Fatal(err)
		}
		collisionSettlement, immediate := collisionStart.ImmediateSettlement()
		if !immediate || collisionSettlement.Kind() != transfer.FileCollision {
			t.Fatalf("collision settlement = (%d, immediate=%t)", collisionSettlement.Kind(), immediate)
		}
		if _, err := session.FinalizeDirectory(context.Background(), rootAdmission); err != nil {
			t.Fatal(err)
		}
		tree, err := session.FinalizeTree(context.Background(), transfer.DirectTreeOutcomePartial)
		if err != nil || tree.Kind() != transfer.DirectTreeSettlementPartial {
			t.Fatalf("partial tree = (%d, %v)", tree.Kind(), err)
		}
		if content, err := os.ReadFile(filepath.Join(resultRoot, "success.bin")); err != nil || string(content) != "good" {
			t.Fatalf("successful prefix = (%q, %v)", content, err)
		}
		if content, err := os.ReadFile(filepath.Join(resultRoot, "collision.bin")); err != nil || string(content) != "keep" {
			t.Fatalf("collision target = (%q, %v)", content, err)
		}
		if names := nativeDirectTreePublicNames(t, root); !slices.Equal(names, []string{filepath.Base(resultRoot)}) {
			t.Fatalf("partial result roots = %v", names)
		}
		if names := nativeDirectTreePublicNames(t, resultRoot); !slices.Equal(names, []string{"collision.bin", "success.bin"}) {
			t.Fatalf("partial result entries = %v", names)
		}
	})
}

func openNativeDirectTreeTestSession(
	t *testing.T,
	root string,
	seed byte,
) (transfer.ReceiveIntent, transfer.DirectTreeSession, transfer.DirectoryAdmission) {
	t.Helper()
	authority, err := NewFilesystemOutputAuthority(FilesystemOutputAuthorityConfig{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := authority.Close(); err != nil {
			t.Errorf("close native output authority: %v", err)
		}
	})
	intent := coverageC6FilesystemIntent(t, authority, seed)
	layout, directoryTree := intent.ArtifactSpec().DirectoryTree()
	resultRoot, namedRoot := layout.ResultRoot()
	reservation, reserved := intent.MaterializationPlan().DestinationReservation()
	if !directoryTree || layout.Kind() != receivecontract.DirectoryTreeResultRoot || !namedRoot ||
		resultRoot.Class() != receivecontract.ResultRootSyntheticSelection ||
		intent.MaterializationPlan().Kind() != receivecontract.PlanDirectTree || !reserved ||
		reservation.AuthorityKind() != receivecontract.AuthorityNativeContainer ||
		reservation.EntryKind() != receivecontract.ContainerEntryResultRoot ||
		reservation.Guarantees().Profile() != receivecontract.GuaranteeNativeTree {
		t.Fatalf("native CLI layout was not explicit: artifact=%v plan=%v", intent.ArtifactSpec().Kind(), intent.MaterializationPlan().Kind())
	}
	session, err := authority.OpenDirectTree(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	rootRequest, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: intent.SyntheticRoot(),
			Generation:  coverageC6Identity[catalog.DirectoryGeneration](seed + 1),
			SourcePath:  ordinaryoutput.EmptySourceCatalogPath(),
		},
		ordinaryoutput.SourceNodeSelected,
		transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	rootAdmission, err := session.AdmitDirectory(context.Background(), rootRequest)
	if err != nil {
		t.Fatal(err)
	}
	return intent, session, rootAdmission
}

func nativeDirectTreeTestFile(
	t *testing.T,
	session transfer.DirectTreeSession,
	intent transfer.ReceiveIntent,
	seed byte,
	path string,
	exactSize uint64,
	parent transfer.DirectoryAdmission,
) transfer.MaterializationFile {
	t.Helper()
	geometry, err := content.NewFileGeometry(exactSize, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(),
		coverageC6Identity[catalog.FileID](seed),
		coverageC6Identity[content.FileRevision](seed+1),
		geometry,
		catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	sourcePath, err := ordinaryoutput.NewSourceCatalogPath(path)
	if err != nil {
		t.Fatal(err)
	}
	parentRequest, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: parent.DirectoryID(), Generation: parent.Generation(),
			SourcePath: ordinaryoutput.EmptySourceCatalogPath(), ModifiedTime: parent.ModifiedTime(),
		},
		ordinaryoutput.SourceNodeSelected,
		transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	parentMaterialization, err := transfer.NewMaterializedDirectoryClaim(parent, parentRequest)
	if err != nil {
		t.Fatal(err)
	}
	parentSource := parentRequest.Source()
	fileParent, err := transfer.NewDirectoryMaterializationFileParent(
		parentSource.DirectoryID, parentSource.Generation, parentSource.SourcePath,
		parent, parentMaterialization,
	)
	if err != nil {
		t.Fatal(err)
	}
	materializationPath, err := transfer.NewMaterializationRootRelativePath(
		parent.Path() + "/" + filepath.Base(path),
	)
	if err != nil {
		t.Fatal(err)
	}
	file, err := transfer.NewMaterializationFile(
		intent, sourcePath, materializationPath, descriptor, session.SessionID(), fileParent,
	)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func nativeDirectTreeResultRoot(
	t *testing.T,
	root string,
	intent transfer.ReceiveIntent,
) string {
	t.Helper()
	reservation, ok := intent.MaterializationPlan().DestinationReservation()
	if !ok || reservation.EntryKind() != receivecontract.ContainerEntryResultRoot {
		t.Fatal("native result-root reservation is missing")
	}
	return filepath.Join(root, reservation.PhysicalName())
}

func nativeDirectTreePublicNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() != ".windshare-output" {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names
}
