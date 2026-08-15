package ordinaryoutput

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestAuthenticatedSourceAndProjectionPublicValuesPreserveSemanticRoles(t *testing.T) {
	root := EmptySourceCatalogPath()
	child, err := NewSourceCatalogPath("folder/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	directoryID := projectionID[catalog.DirectoryID](0x91)
	fileID := projectionID[catalog.FileID](0x92)

	directory, err := NewAuthenticatedDirectorySourceNode(
		directoryID, root, SourceNodeConnectsSelection,
	)
	if err != nil || !directory.valid() ||
		directory.Kind() != catalog.NodeKindDirectory ||
		directory.NodeID() != directoryID.NodeID() ||
		!directory.SourcePath().IsRoot() ||
		directory.Role() != SourceNodeConnectsSelection {
		t.Fatalf("directory source = (%+v, %v)", directory, err)
	}
	file, err := NewAuthenticatedFileSourceNode(fileID, child, SourceNodeSelected)
	if err != nil || !file.valid() ||
		file.Kind() != catalog.NodeKindFile ||
		file.NodeID() != fileID.NodeID() ||
		file.SourcePath() != child ||
		file.Role() != SourceNodeSelected {
		t.Fatalf("file source = (%+v, %v)", file, err)
	}
	if _, err := NewAuthenticatedDirectorySourceNode(
		catalog.DirectoryID{}, root, SourceNodeSelected,
	); !errors.Is(err, ErrInvalidAuthenticatedSource) {
		t.Fatalf("zero directory source error = %v", err)
	}
	if _, err := NewAuthenticatedFileSourceNode(
		catalog.FileID{}, child, SourceNodeSelected,
	); !errors.Is(err, ErrInvalidAuthenticatedSource) {
		t.Fatalf("zero file source error = %v", err)
	}
	if _, err := NewAuthenticatedFileSourceNode(
		fileID, root, SourceNodeSelected,
	); !errors.Is(err, ErrInvalidAuthenticatedSource) {
		t.Fatalf("root file source error = %v", err)
	}

	traverse := TraverseOnlyProjection()
	if traverse.Kind() != ArtifactTraverseOnly {
		t.Fatalf("traverse projection kind = %d", traverse.Kind())
	}
	if _, ok := traverse.ArtifactPath(); ok || traverse.RejectReason() != ArtifactRejectNone {
		t.Fatal("traverse-only projection exposed artifact authority")
	}
	artifact, err := NewArtifactPath("folder/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	materialize, err := MaterializeArtifactProjection(artifact)
	if err != nil || materialize.Kind() != ArtifactMaterialize {
		t.Fatalf("materialize projection = (%+v, %v)", materialize, err)
	}
	if projected, ok := materialize.ArtifactPath(); !ok || projected != artifact {
		t.Fatalf("materialized artifact = (%+v, %t)", projected, ok)
	}
	if _, err := MaterializeArtifactProjection(ArtifactPath{}); !errors.Is(err, ErrInvalidArtifactPath) {
		t.Fatalf("zero artifact projection error = %v", err)
	}
}

func TestShapeSelectionValueAccessorsExposeOnlyFrozenProofInputs(t *testing.T) {
	fileID := projectionID[catalog.FileID](0xa1)
	directoryID := projectionID[catalog.DirectoryID](0xa2)
	single, err := NewSingleFileShape(fileID, "folder/file.bin")
	if err != nil || single.FileID() != fileID || !single.DirectoryID().IsZero() ||
		single.Kind() != ShapeSingleFile || single.SourcePath() != "folder/file.bin" ||
		single.PreferredName() != "file.bin" || !single.Valid() {
		t.Fatalf("single shape = (%+v, %v)", single, err)
	}
	complete, err := NewCompleteDirectoryShape(directoryID, "folder")
	if err != nil || complete.DirectoryID() != directoryID || !complete.FileID().IsZero() ||
		complete.Kind() != ShapeCompleteDirectory || !complete.Valid() {
		t.Fatalf("complete shape = (%+v, %v)", complete, err)
	}
	partial, err := NewPartialDirectoryShape(directoryID, "folder")
	if err != nil || partial.Kind() != ShapePartialDirectory || !partial.Valid() {
		t.Fatalf("partial shape = (%+v, %v)", partial, err)
	}

	directoryTarget, err := NewOpaqueDirectoryTarget(directoryID, true)
	if err != nil || directoryTarget.Kind() != catalog.NodeKindDirectory ||
		directoryTarget.NodeID() != directoryID.NodeID() || !directoryTarget.Selected() {
		t.Fatalf("opaque directory target = (%+v, %v)", directoryTarget, err)
	}
	fileTarget, err := NewOpaqueFileTarget(fileID, false)
	if err != nil || fileTarget.Kind() != catalog.NodeKindFile ||
		fileTarget.NodeID() != fileID.NodeID() || fileTarget.Selected() {
		t.Fatalf("opaque file target = (%+v, %v)", fileTarget, err)
	}
	if _, err := NewOpaqueDirectoryTarget(catalog.DirectoryID{}, true); !errors.Is(err, ErrInvalidShapeResolution) {
		t.Fatalf("zero opaque directory error = %v", err)
	}
	if _, err := NewOpaqueFileTarget(catalog.FileID{}, true); !errors.Is(err, ErrInvalidShapeResolution) {
		t.Fatalf("zero opaque file error = %v", err)
	}

	share := projectionID[catalog.ShareInstance](0xa3)
	synthetic := projectionID[catalog.DirectoryID](0xa4)
	var digest [32]byte
	digest[0] = 1
	selection, err := NewWholeShareSelection(share, synthetic, digest)
	if err != nil || selection.Digest() != digest {
		t.Fatalf("selection digest = (%x, %v)", selection.Digest(), err)
	}
}
