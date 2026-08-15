package ordinaryoutput

import (
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestProjectionConstructorsRejectContradictoryCoordinates(t *testing.T) {
	root := projectionID[catalog.DirectoryID](0xc1)
	other := projectionID[catalog.DirectoryID](0xc2)
	file := projectionID[catalog.FileID](0xc3)
	source, err := NewSourceCatalogPath("parent/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSourceCatalogPath(""); !errors.Is(err, ErrInvalidSourceCatalogPath) {
		t.Fatalf("empty source path = %v", err)
	}
	if _, err := NewArtifactPath(""); !errors.Is(err, ErrInvalidArtifactPath) {
		t.Fatalf("empty artifact path = %v", err)
	}
	if _, err := MaterializeArtifactProjection(ArtifactPath{}); !errors.Is(err, ErrInvalidArtifactPath) {
		t.Fatalf("zero materialization = %v", err)
	}
	if _, err := NewAuthenticatedDirectorySourceNode(
		catalog.DirectoryID{}, source, SourceNodeSelected,
	); !errors.Is(err, ErrInvalidAuthenticatedSource) {
		t.Fatalf("zero directory source = %v", err)
	}
	if _, err := NewAuthenticatedFileSourceNode(
		catalog.FileID{}, source, SourceNodeSelected,
	); !errors.Is(err, ErrInvalidAuthenticatedSource) {
		t.Fatalf("zero file source = %v", err)
	}
	if _, err := newSingleFileProjectionLayout(
		root, file, source, "nested/name",
	); !errors.Is(err, ErrInvalidArtifactProjector) {
		t.Fatalf("nested preferred file name = %v", err)
	}
	if _, err := newRealResultRootProjectionLayout(
		root, other, source, "unrelated",
	); !errors.Is(err, ErrInvalidArtifactProjector) {
		t.Fatalf("unrelated result name = %v", err)
	}
	if _, err := newSyntheticResultRootProjectionLayout(
		root, strings.Repeat("x", receivecontract.MaxResultComponentBytes+1),
	); !errors.Is(err, ErrInvalidArtifactProjector) {
		t.Fatalf("oversized synthetic name = %v", err)
	}
	if _, err := newCatalogRootProjectionLayout(catalog.DirectoryID{}); !errors.Is(
		err, ErrInvalidArtifactProjector,
	) {
		t.Fatalf("zero catalog root = %v", err)
	}
	if _, err := newArtifactPathProjector(projectionLayout{}); !errors.Is(
		err, ErrInvalidArtifactProjector,
	) {
		t.Fatalf("zero projection layout = %v", err)
	}
	if _, err := NewArtifactPathProjector(root, receivecontract.ArtifactSpec{}); !errors.Is(
		err, ErrInvalidArtifactProjector,
	) {
		t.Fatalf("zero artifact = %v", err)
	}
	if (projectionLayout{kind: projectionLayoutKind(0xff), syntheticRoot: root,
		anchorID: other.NodeID(), sourcePath: source, preferredName: "file.bin"}).valid() {
		t.Fatal("unknown projection layout became valid")
	}
}

func TestProjectorRejectsEveryAuthoritySubstitution(t *testing.T) {
	root := projectionID[catalog.DirectoryID](0xd1)
	anchor := projectionID[catalog.DirectoryID](0xd2)
	other := projectionID[catalog.DirectoryID](0xd3)
	file := projectionID[catalog.FileID](0xd4)
	cases := []struct {
		name      string
		projector ArtifactPathProjector
		node      AuthenticatedSourceNode
		reason    ArtifactRejectReason
	}{
		{"single-root-substitution", singleProjector(t, root, file, "parent/file.bin"),
			sourceNode(t, catalog.NodeKindDirectory, other.NodeID(), "", SourceNodeConnectsSelection),
			ArtifactRejectWrongIdentity},
		{"single-directory-unrelated", singleProjector(t, root, file, "parent/file.bin"),
			sourceNode(t, catalog.NodeKindDirectory, other.NodeID(), "sibling", SourceNodeConnectsSelection),
			ArtifactRejectUnrelatedSource},
		{"single-file-unrelated", singleProjector(t, root, file, "parent/file.bin"),
			sourceNode(t, catalog.NodeKindFile, file.NodeID(), "parent/other.bin", SourceNodeSelected),
			ArtifactRejectUnrelatedSource},
		{"real-root-substitution", realRootProjector(t, root, anchor, "parent/photos", "photos"),
			sourceNode(t, catalog.NodeKindDirectory, other.NodeID(), "", SourceNodeConnectsSelection),
			ArtifactRejectWrongIdentity},
		{"real-ancestor-selected", realRootProjector(t, root, anchor, "parent/photos", "photos"),
			sourceNode(t, catalog.NodeKindDirectory, other.NodeID(), "parent", SourceNodeSelected),
			ArtifactRejectWrongRole},
		{"real-anchor-as-file", realRootProjector(t, root, anchor, "parent/photos", "photos"),
			sourceNode(t, catalog.NodeKindFile, file.NodeID(), "parent/photos", SourceNodeSelected),
			ArtifactRejectWrongKind},
		{"synthetic-root-substitution", syntheticProjector(t, root),
			sourceNode(t, catalog.NodeKindDirectory, other.NodeID(), "", SourceNodeConnectsSelection),
			ArtifactRejectWrongIdentity},
		{"synthetic-root-id-as-child", syntheticProjector(t, root),
			sourceNode(t, catalog.NodeKindDirectory, root.NodeID(), "child", SourceNodeSelected),
			ArtifactRejectWrongIdentity},
		{"catalog-root-substitution", catalogRootProjector(t, root),
			sourceNode(t, catalog.NodeKindDirectory, other.NodeID(), "", SourceNodeConnectsSelection),
			ArtifactRejectWrongIdentity},
		{"catalog-root-id-as-child", catalogRootProjector(t, root),
			sourceNode(t, catalog.NodeKindDirectory, root.NodeID(), "child", SourceNodeSelected),
			ArtifactRejectWrongIdentity},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := test.projector.Project(test.node)
			if got.Kind() != ArtifactReject || got.RejectReason() != test.reason {
				t.Fatalf("projection = (%d, %d)", got.Kind(), got.RejectReason())
			}
		})
	}
	if got := (ArtifactPathProjector{layout: projectionLayout{
		kind: projectionLayoutKind(0xff), syntheticRoot: root, anchorID: anchor.NodeID(),
		sourcePath: func() SourceCatalogPath {
			path, _ := NewSourceCatalogPath("anchor")
			return path
		}(), preferredName: "anchor",
	}}).Project(sourceNode(
		t, catalog.NodeKindDirectory, anchor.NodeID(), "anchor", SourceNodeSelected,
	)); got.RejectReason() != ArtifactRejectInvalidSource {
		t.Fatalf("unknown layout projection = %+v", got)
	}
}

func TestShapeSelectionConstructorsRemainBoundedAndUnambiguous(t *testing.T) {
	share := projectionID[catalog.ShareInstance](0xe1)
	root := projectionID[catalog.DirectoryID](0xe2)
	directory := projectionID[catalog.DirectoryID](0xe3)
	target, err := NewOpaqueDirectoryTarget(directory, true)
	if err != nil {
		t.Fatal(err)
	}
	var digest [32]byte
	digest[0] = 1
	if _, err := NewWholeShareSelection(catalog.ShareInstance{}, root, digest); !errors.Is(
		err, ErrInvalidShapeResolution,
	) {
		t.Fatalf("zero whole-share identity = %v", err)
	}
	for name, paths := range map[string][]string{
		"empty":     nil,
		"duplicate": {"folder", "folder"},
		"unsafe":    {"../escape"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewCatalogPathSelection(share, root, digest, paths); !errors.Is(
				err, ErrInvalidShapeResolution,
			) {
				t.Fatalf("catalog path selection = %v", err)
			}
		})
	}
	for name, targets := range map[string][]OpaqueSelectionTarget{
		"empty":     nil,
		"zero":      {{}},
		"duplicate": {target, target},
		"none-selected": {func() OpaqueSelectionTarget {
			excluded, _ := NewOpaqueDirectoryTarget(directory, false)
			return excluded
		}()},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewOpaqueNodeSelection(share, root, digest, false, targets); !errors.Is(
				err, ErrInvalidShapeResolution,
			) {
				t.Fatalf("opaque selection = %v", err)
			}
		})
	}
	if _, err := NewSingleFileShape(catalog.FileID{}, "file.bin"); !errors.Is(err, ErrInvalidShapeResolution) {
		t.Fatalf("zero single-file shape = %v", err)
	}
	if _, err := NewCompleteDirectoryShape(catalog.DirectoryID{}, "folder"); !errors.Is(
		err, ErrInvalidShapeResolution,
	) {
		t.Fatalf("zero complete-directory shape = %v", err)
	}
	if _, err := NewPartialDirectoryShape(catalog.DirectoryID{}, "folder"); !errors.Is(
		err, ErrInvalidShapeResolution,
	) {
		t.Fatalf("zero partial-directory shape = %v", err)
	}
	if (ShapeDecision{kind: ShapeKind(0xff)}).Valid() {
		t.Fatal("unknown shape decision became valid")
	}
	budget, _ := NewShapeProbeBudget(1, 1, 1, 1, 1)
	if _, err := ResolveShape(nil, nil, Selection{}, budget, nil); !errors.Is(
		err, ErrInvalidShapeResolution,
	) {
		t.Fatalf("unbound shape resolution = %v", err)
	}
}
