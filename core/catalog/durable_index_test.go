package catalog

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileNodeIndexReadsAddressedRecordWithoutScanningEarlierChildren(t *testing.T) {
	ctx := context.Background()
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: t.TempDir(), ShareInstance: idValue[ShareInstance](10)})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Destroy()
	directoryID := idValue[DirectoryID](11)
	directory := backendDirectoryRecord(t, directoryID, idValue[DirectoryID](9), "indexed", 1)
	const count = 96
	children := make([]NodeRecord, count)
	for i := range children {
		children[i], err = wideScannedFile(t, i).nodeRecord(directoryID)
		if err != nil {
			t.Fatal(err)
		}
	}
	meter := backendTestMeter(t)
	defer meter.Close()
	tx := stageBackendGeneration(t, backend, backend.share, directory, idValue[DirectoryGeneration](12), children, meter)
	prepared, err := tx.Prepare(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := backend.LoadNode(ctx, children[count-1].NodeID()); err != nil || found {
		t.Fatalf("staged node became visible: found=%v err=%v", found, err)
	}
	if _, err := tx.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	usage, err := backend.Recover(ctx)
	if err != nil || usage != prepared.Usage {
		t.Fatalf("index accounting after recovery = %+v want=%+v err=%v", usage, prepared.Usage, err)
	}
	for _, want := range append(children, directory) {
		got, found, err := backend.LoadNode(ctx, want.NodeID())
		if err != nil || !found || got != want {
			t.Fatalf("indexed record changed: found=%v err=%v", found, err)
		}
	}
	// A point read must not inspect earlier records in the same generation.
	file, err := os.OpenFile(filepath.Join(backend.directoryPath(directoryID), fileCatalogChildrenName), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteAt([]byte{0, 0, 0, 0}, 0)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	got, found, err := backend.LoadNode(ctx, children[count-1].NodeID())
	if err != nil || !found || got != children[count-1] {
		t.Fatalf("point lookup scanned the prefix: found=%v err=%v", found, err)
	}
	if _, _, err := backend.LoadNode(ctx, children[0].NodeID()); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("addressed corrupt record accepted: %v", err)
	}
}

func TestFileNodeIndexSkipsUnrelatedGenerations(t *testing.T) {
	_, backend, unrelatedPath, _ := durableRecoveryFixture(t)
	defer backend.Destroy()
	directoryID := idValue[DirectoryID](110)
	directory := backendDirectoryRecord(t, directoryID, idValue[DirectoryID](109), "other", 1)
	child, err := scannedFile(t, 111, "target", 1).nodeRecord(directoryID)
	if err != nil {
		t.Fatal(err)
	}
	meter := backendTestMeter(t)
	defer meter.Close()
	publishBackendGeneration(t, stageBackendGeneration(t, backend, backend.share, directory, idValue[DirectoryGeneration](112), []NodeRecord{child}, meter))
	// Removing unrelated metadata makes a catalog-wide scan fail deterministically;
	// the addressed generation remains readable through its own published index.
	if err := os.Remove(filepath.Join(unrelatedPath, fileCatalogMetaName)); err != nil {
		t.Fatal(err)
	}
	got, found, err := backend.LoadNode(context.Background(), child.NodeID())
	if err != nil || !found || got != child {
		t.Fatalf("lookup opened an unrelated generation: found=%v err=%v", found, err)
	}
}

func TestFileNodeIndexRechecksForeignIdentityAtPublication(t *testing.T) {
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: t.TempDir(), ShareInstance: idValue[ShareInstance](1)})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Destroy()
	makeTx := func(id byte) (BackendTransaction, NodeRecord) {
		dirID := idValue[DirectoryID](id)
		dir := backendDirectoryRecord(t, dirID, idValue[DirectoryID](2), "directory", 1)
		child, err := scannedFile(t, 50, "shared-id", 1).nodeRecord(dirID)
		if err != nil {
			t.Fatal(err)
		}
		meter := backendTestMeter(t)
		t.Cleanup(meter.Close)
		tx := stageBackendGeneration(t, backend, backend.share, dir, idValue[DirectoryGeneration](id+10), []NodeRecord{child}, meter)
		t.Cleanup(func() { _ = tx.Abort() })
		if _, err := tx.Prepare(context.Background()); err != nil {
			t.Fatal(err)
		}
		return tx, child
	}
	first, _ := makeTx(3)
	second, want := makeTx(4)
	if _, err := second.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Publish(context.Background()); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("publication admitted duplicate node identity: %v", err)
	}
	got, found, err := backend.LoadNode(context.Background(), want.NodeID())
	if err != nil || !found || got != want {
		t.Fatalf("failed publication changed index: found=%v err=%v", found, err)
	}
	if _, found, err := backend.LoadDirectory(context.Background(), idValue[DirectoryID](3)); err != nil || found {
		t.Fatalf("conflicting generation became visible: found=%v err=%v", found, err)
	}
}

func TestFileNodeIndexRecoveryRejectsDamagedMembershipAndOffsets(t *testing.T) {
	for _, damage := range []string{"missing", "truncated", "membership", "offset"} {
		t.Run(damage, func(t *testing.T) {
			root, backend, path, meta := durableRecoveryFixture(t)
			defer backend.Destroy()
			indexPath := filepath.Join(path, fileCatalogNodeIndexName)
			switch damage {
			case "missing":
				if err := os.Remove(indexPath); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				if err := os.Truncate(indexPath, 1); err != nil {
					t.Fatal(err)
				}
			default:
				file, err := os.OpenFile(indexPath, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				if damage == "membership" {
					var value [1]byte
					if _, err := file.ReadAt(value[:], 0); err != nil {
						t.Fatal(err)
					}
					value[0] ^= 1
					if _, err := file.WriteAt(value[:], 0); err != nil {
						t.Fatal(err)
					}
				} else {
					index := backend.nodeIndexes[meta.directory]
					id := idValue[NodeID](92)
					position, _, err := index.findSlot(context.Background(), file, id, nodeIndexHash(id))
					if err != nil {
						t.Fatal(err)
					}
					var offset [8]byte
					binary.BigEndian.PutUint64(offset[:], nodeIndexDirectoryOffset)
					if _, err := file.WriteAt(offset[:], position+IdentityBytes); err != nil {
						t.Fatal(err)
					}
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				if damage == "offset" {
					if _, _, err := backend.LoadNode(context.Background(), idValue[NodeID](92)); !errors.Is(err, ErrCorruptCatalogStorage) {
						t.Fatalf("misaddressed index was trusted: %v", err)
					}
				}
			}
			reopened, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: root, ShareInstance: backend.share})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if _, err := reopened.Recover(context.Background()); err == nil {
				t.Fatal("recovered damaged node index")
			}
			if _, _, err := reopened.LoadNode(context.Background(), idValue[NodeID](92)); err == nil {
				t.Fatal("lazy index load accepted damaged generation")
			}
		})
	}
}

func TestFileNodeIndexReloadsPublishedGenerationWithoutExplicitRecovery(t *testing.T) {
	root, backend, _, _ := durableRecoveryFixture(t)
	defer backend.Destroy()
	reopened, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: root, ShareInstance: backend.share})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for range 2 {
		got, found, err := reopened.LoadNode(context.Background(), idValue[NodeID](92))
		if err != nil || !found || got.Entry().Name() != "child" {
			t.Fatalf("lazy index load failed: found=%v err=%v", found, err)
		}
	}
}
