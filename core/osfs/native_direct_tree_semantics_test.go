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
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestNativeDirectTreeHostSemantics(t *testing.T) {
	t.Run("zero byte publishes without a public placeholder", func(t *testing.T) {
		root := t.TempDir()
		intent, session, rootAdmission := openNativeDirectTreeTestSession(t, root, 0xb1)
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
		tree, err := session.FinalizeTree(context.Background(), transfer.DirectTreeOutcomePublished)
		if err != nil || tree.Kind() != transfer.DirectTreeSettlementPublished {
			t.Fatalf("zero-byte tree = (%d, %v)", tree.Kind(), err)
		}
		info, err := os.Stat(filepath.Join(root, "empty.bin"))
		if err != nil || info.Size() != 0 {
			t.Fatalf("zero-byte output = (%v, %v)", info, err)
		}
		if names := nativeDirectTreePublicNames(t, root); !slices.Equal(names, []string{"empty.bin"}) {
			t.Fatalf("zero-byte public entries = %v", names)
		}
	})

	t.Run("collision is isolated and successful prefix remains partial", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "collision.bin"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		intent, session, rootAdmission := openNativeDirectTreeTestSession(t, root, 0xc1)
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
		tree, err := session.FinalizeTree(context.Background(), transfer.DirectTreeOutcomePartialDirectory)
		if err != nil || tree.Kind() != transfer.DirectTreeSettlementPartialDirectory {
			t.Fatalf("partial tree = (%d, %v)", tree.Kind(), err)
		}
		if content, err := os.ReadFile(filepath.Join(root, "success.bin")); err != nil || string(content) != "good" {
			t.Fatalf("successful prefix = (%q, %v)", content, err)
		}
		if content, err := os.ReadFile(filepath.Join(root, "collision.bin")); err != nil || string(content) != "keep" {
			t.Fatalf("collision target = (%q, %v)", content, err)
		}
		if names := nativeDirectTreePublicNames(t, root); !slices.Equal(names, []string{"collision.bin", "success.bin"}) {
			t.Fatalf("partial public entries = %v", names)
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
	intent := coverageC6FilesystemIntent(t, authority, seed)
	layout, directoryTree := intent.ArtifactSpec().DirectoryTree()
	reservation, reserved := intent.MaterializationPlan().DestinationReservation()
	if !directoryTree || layout.Kind() != receivecontract.DirectoryTreeCatalogRoot ||
		intent.MaterializationPlan().Kind() != receivecontract.PlanDirectTree || !reserved ||
		reservation.AuthorityKind() != receivecontract.AuthorityNativeContainer ||
		reservation.Guarantees().Profile() != receivecontract.GuaranteeNativeTree {
		t.Fatalf("native CLI layout was not explicit: artifact=%v plan=%v", intent.ArtifactSpec().Kind(), intent.MaterializationPlan().Kind())
	}
	session, err := authority.OpenDirectTree(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	rootAdmission, err := session.AdmitDirectory(context.Background(), transfer.MaterializationDirectory{
		DirectoryID: intent.SyntheticRoot(),
		Generation:  coverageC6Identity[catalog.DirectoryGeneration](seed + 1),
	})
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
	locator, err := transfer.NewPathMaterializationLocator(path)
	if err != nil {
		t.Fatal(err)
	}
	target, err := transfer.NewFileMaterializationTarget(session.SessionID(), descriptor, locator)
	if err != nil {
		t.Fatal(err)
	}
	return transfer.MaterializationFile{
		Path: path, ExpectedSize: exactSize, Descriptor: descriptor,
		Target: target, ParentAdmission: parent,
	}
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
