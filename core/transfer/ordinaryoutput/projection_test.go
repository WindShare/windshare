package ordinaryoutput

import (
	"bytes"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestProjectionContractsAreClosed(t *testing.T) {
	if !SourceNodeSelected.Valid() || !SourceNodeConnectsSelection.Valid() ||
		SourceNodeRole(0).Valid() || SourceNodeRole(3).Valid() {
		t.Fatal("source node role is not closed")
	}
	if !ArtifactTraverseOnly.Valid() || !ArtifactReject.Valid() ||
		ArtifactProjectionKind(0).Valid() || ArtifactProjectionKind(4).Valid() {
		t.Fatal("projection kind is not closed")
	}
	if ArtifactRejectNone.Valid() || !ArtifactRejectInvalidSource.Valid() ||
		!ArtifactRejectUnrelatedSource.Valid() || ArtifactRejectReason(6).Valid() {
		t.Fatal("reject reason is not closed")
	}
}

func TestArtifactPathProjectorThreeStateMatrix(t *testing.T) {
	root := projectionID[catalog.DirectoryID](1)
	anchor := projectionID[catalog.DirectoryID](2)
	file := projectionID[catalog.FileID](3)
	other := projectionID[catalog.DirectoryID](4)

	tests := []struct {
		name       string
		projector  ArtifactPathProjector
		node       AuthenticatedSourceNode
		wantKind   ArtifactProjectionKind
		wantPath   string
		wantReason ArtifactRejectReason
	}{
		{
			name:      "single synthetic root traverses",
			projector: singleProjector(t, root, file, "parent/file.bin"),
			node:      sourceNode(t, catalog.NodeKindDirectory, root.NodeID(), "", SourceNodeConnectsSelection),
			wantKind:  ArtifactTraverseOnly,
		},
		{
			name:      "single authenticated ancestor traverses",
			projector: singleProjector(t, root, file, "parent/file.bin"),
			node:      sourceNode(t, catalog.NodeKindDirectory, anchor.NodeID(), "parent", SourceNodeConnectsSelection),
			wantKind:  ArtifactTraverseOnly,
		},
		{
			name:      "single exact file materializes",
			projector: singleProjector(t, root, file, "parent/file.bin"),
			node:      sourceNode(t, catalog.NodeKindFile, file.NodeID(), "parent/file.bin", SourceNodeSelected),
			wantKind:  ArtifactMaterialize, wantPath: "file.bin",
		},
		{
			name:      "single path substitution rejects",
			projector: singleProjector(t, root, file, "parent/file.bin"),
			node:      sourceNode(t, catalog.NodeKindFile, projectionID[catalog.FileID](9).NodeID(), "parent/file.bin", SourceNodeSelected),
			wantKind:  ArtifactReject, wantReason: ArtifactRejectWrongIdentity,
		},
		{
			name:      "real anchor ancestor traverses",
			projector: realRootProjector(t, root, anchor, "parent/photos", "photos"),
			node:      sourceNode(t, catalog.NodeKindDirectory, other.NodeID(), "parent", SourceNodeConnectsSelection),
			wantKind:  ArtifactTraverseOnly,
		},
		{
			name:      "real anchor materializes named root",
			projector: realRootProjector(t, root, anchor, "parent/photos", "photos"),
			node:      sourceNode(t, catalog.NodeKindDirectory, anchor.NodeID(), "parent/photos", SourceNodeSelected),
			wantKind:  ArtifactMaterialize, wantPath: "photos",
		},
		{
			name:      "real connecting descendant materializes",
			projector: realRootProjector(t, root, anchor, "parent/photos", "photos"),
			node:      sourceNode(t, catalog.NodeKindDirectory, other.NodeID(), "parent/photos/nested", SourceNodeConnectsSelection),
			wantKind:  ArtifactMaterialize, wantPath: "photos/nested",
		},
		{
			name:      "real selected descendant materializes",
			projector: realRootProjector(t, root, anchor, "parent/photos", "photos"),
			node:      sourceNode(t, catalog.NodeKindFile, file.NodeID(), "parent/photos/image.jpg", SourceNodeSelected),
			wantKind:  ArtifactMaterialize, wantPath: "photos/image.jpg",
		},
		{
			name:      "real sibling rejects",
			projector: realRootProjector(t, root, anchor, "parent/photos", "photos"),
			node:      sourceNode(t, catalog.NodeKindFile, file.NodeID(), "parent/video.mp4", SourceNodeSelected),
			wantKind:  ArtifactReject, wantReason: ArtifactRejectUnrelatedSource,
		},
		{
			name:      "real wrong anchor ID rejects",
			projector: realRootProjector(t, root, anchor, "parent/photos", "photos"),
			node:      sourceNode(t, catalog.NodeKindDirectory, other.NodeID(), "parent/photos", SourceNodeSelected),
			wantKind:  ArtifactReject, wantReason: ArtifactRejectWrongIdentity,
		},
		{
			name:      "synthetic root materializes",
			projector: syntheticProjector(t, root),
			node:      sourceNode(t, catalog.NodeKindDirectory, root.NodeID(), "", SourceNodeConnectsSelection),
			wantKind:  ArtifactMaterialize, wantPath: "windshare",
		},
		{
			name:      "synthetic descendant materializes",
			projector: syntheticProjector(t, root),
			node:      sourceNode(t, catalog.NodeKindFile, file.NodeID(), "photos/image.jpg", SourceNodeSelected),
			wantKind:  ArtifactMaterialize, wantPath: "windshare/photos/image.jpg",
		},
		{
			name:      "legacy root traverses",
			projector: catalogRootProjector(t, root),
			node:      sourceNode(t, catalog.NodeKindDirectory, root.NodeID(), "", SourceNodeConnectsSelection),
			wantKind:  ArtifactTraverseOnly,
		},
		{
			name:      "legacy descendant preserves source coordinate",
			projector: catalogRootProjector(t, root),
			node:      sourceNode(t, catalog.NodeKindFile, file.NodeID(), "photos/image.jpg", SourceNodeSelected),
			wantKind:  ArtifactMaterialize, wantPath: "photos/image.jpg",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.projector.Project(test.node)
			if got.Kind() != test.wantKind || got.RejectReason() != test.wantReason {
				t.Fatalf("projection kind/reason=%d/%d want=%d/%d",
					got.Kind(), got.RejectReason(), test.wantKind, test.wantReason)
			}
			path, materialized := got.ArtifactPath()
			if test.wantKind == ArtifactMaterialize {
				if !materialized || path.String() != test.wantPath {
					t.Fatalf("artifact path=%q materialized=%v want=%q", path.String(), materialized, test.wantPath)
				}
				return
			}
			if materialized || path.Valid() {
				t.Fatalf("non-materialized projection retained path=%q", path.String())
			}
		})
	}
}

func TestArtifactPathProjectorIsStableAcrossReservedNameAlias(t *testing.T) {
	root := projectionID[catalog.DirectoryID](1)
	anchor := projectionID[catalog.DirectoryID](2)
	file := projectionID[catalog.FileID](3)
	artifactLayout, _ := receivecontract.NewCompleteDirectoryResultRoot(anchor, "parent/photos")
	artifact, _ := receivecontract.NewResultRootDirectoryTree(artifactLayout)
	operation := projectionContractID[receivecontract.OperationID](4)
	reservationID := projectionContractID[receivecontract.DestinationReservationID](5)
	authority, _ := receivecontract.AuthorityRefFromBytes(
		bytes.Repeat([]byte{6}, receivecontract.AuthorityRefBytes),
	)
	reservedName, _ := receivecontract.CollisionName(operation, "photos", 1, false)
	reservation, err := receivecontract.NewNativeNamedEntryReservation(
		operation, reservationID, artifact, authority, reservedName, 1,
	)
	if err != nil || reservation.ReservedName() == "photos" {
		t.Fatalf("reservation=%+v err=%v", reservation, err)
	}
	projector, err := NewArtifactPathProjector(root, artifact)
	if err != nil {
		t.Fatal(err)
	}
	projection := projector.Project(sourceNode(
		t, catalog.NodeKindFile, file.NodeID(), "parent/photos/image.jpg", SourceNodeSelected,
	))
	path, ok := projection.ArtifactPath()
	if !ok || path.String() != "photos/image.jpg" ||
		strings.HasPrefix(path.String(), reservation.ReservedName()+"/") {
		t.Fatalf("projection=%+v path=%q reserved=%q", projection, path.String(), reservation.ReservedName())
	}
}

func TestArtifactPathProjectorRejectsUnsafeAndRoleConfusion(t *testing.T) {
	root := projectionID[catalog.DirectoryID](1)
	anchor := projectionID[catalog.DirectoryID](2)
	file := projectionID[catalog.FileID](3)
	projector := realRootProjector(t, root, anchor, "parent/photos", "photos")

	if _, err := NewSourceCatalogPath("../photos"); err == nil {
		t.Fatal("unsafe source path accepted")
	}
	if _, err := NewArtifactPath("photos/../escape"); err == nil {
		t.Fatal("unsafe artifact path accepted")
	}
	oversized := strings.Repeat("a", receivecontract.MaxResultComponentBytes+1)
	if canonicalArtifactComponent(oversized) {
		t.Fatal("oversized artifact component accepted")
	}
	if _, err := NewArtifactPath("photos/" + oversized); err == nil {
		t.Fatal("oversized artifact path component accepted")
	}
	invalid := AuthenticatedSourceNode{}
	if got := projector.Project(invalid); got.Kind() != ArtifactReject ||
		got.RejectReason() != ArtifactRejectInvalidSource {
		t.Fatalf("invalid source projection=%+v", got)
	}
	connectingFile := sourceNode(
		t, catalog.NodeKindFile, file.NodeID(), "parent/photos/image.jpg", SourceNodeConnectsSelection,
	)
	if got := projector.Project(connectingFile); got.Kind() != ArtifactMaterialize {
		t.Fatalf("connecting descendant file projection=%+v", got)
	}

	single := singleProjector(t, root, file, "parent/file.bin")
	wrongRole := sourceNode(
		t, catalog.NodeKindFile, file.NodeID(), "parent/file.bin", SourceNodeConnectsSelection,
	)
	if got := single.Project(wrongRole); got.Kind() != ArtifactReject ||
		got.RejectReason() != ArtifactRejectWrongRole {
		t.Fatalf("single connecting file projection=%+v", got)
	}
}

func FuzzRealResultRootProjectionCannotEscapeAnchor(f *testing.F) {
	f.Add("parent/photos/image.jpg")
	f.Add("parent/video.mp4")
	f.Add("parent/photos")
	f.Add("parent")
	root := projectionID[catalog.DirectoryID](1)
	anchor := projectionID[catalog.DirectoryID](2)
	file := projectionID[catalog.FileID](3)
	projector := realRootProjectorForFuzz(root, anchor, "parent/photos", "photos")

	f.Fuzz(func(t *testing.T, candidate string) {
		path, err := NewSourceCatalogPath(candidate)
		if err != nil {
			return
		}
		node, err := newAuthenticatedSourceNode(
			catalog.NodeKindFile, file.NodeID(), path, SourceNodeSelected,
		)
		if err != nil {
			return
		}
		projection := projector.Project(node)
		artifact, materialized := projection.ArtifactPath()
		if !materialized {
			return
		}
		if artifact.String() != "photos" &&
			len(artifact.String()) > len("photos/") &&
			artifact.String()[:len("photos/")] != "photos/" {
			t.Fatalf("artifact path escaped root: %q", artifact.String())
		}
		if candidate != "parent/photos" &&
			len(candidate) >= len("parent/photos/") &&
			candidate[:len("parent/photos/")] != "parent/photos/" {
			t.Fatalf("unrelated source materialized: %q -> %q", candidate, artifact.String())
		}
	})
}

func singleProjector(
	t *testing.T,
	root catalog.DirectoryID,
	file catalog.FileID,
	path string,
) ArtifactPathProjector {
	t.Helper()
	artifact, err := receivecontract.NewSingleFileDirectoryTree(
		file,
		path,
		sourceLeaf(path),
	)
	return artifactProjector(t, root, artifact, err)
}

func realRootProjector(
	t *testing.T,
	root catalog.DirectoryID,
	anchor catalog.DirectoryID,
	path string,
	name string,
) ArtifactPathProjector {
	t.Helper()
	leaf := sourceLeaf(path)
	var layout receivecontract.ResultRootLayout
	var err error
	switch name {
	case leaf:
		layout, err = receivecontract.NewCompleteDirectoryResultRoot(anchor, path)
	case leaf + receivecontract.PartialSelectionSuffix:
		layout, err = receivecontract.NewDirectorySelectionResultRoot(anchor, path)
	default:
		t.Fatalf("test name %q is not derived from source path %q", name, path)
	}
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(layout)
	return artifactProjector(t, root, artifact, err)
}

func realRootProjectorForFuzz(
	root catalog.DirectoryID,
	anchor catalog.DirectoryID,
	path string,
	_ string,
) ArtifactPathProjector {
	layout, _ := receivecontract.NewCompleteDirectoryResultRoot(anchor, path)
	artifact, _ := receivecontract.NewResultRootDirectoryTree(layout)
	projector, _ := NewArtifactPathProjector(root, artifact)
	return projector
}

func syntheticProjector(t *testing.T, root catalog.DirectoryID) ArtifactPathProjector {
	t.Helper()
	artifact, err := receivecontract.NewResultRootDirectoryTree(
		receivecontract.NewSyntheticSelectionResultRoot(),
	)
	return artifactProjector(t, root, artifact, err)
}

func catalogRootProjector(t *testing.T, root catalog.DirectoryID) ArtifactPathProjector {
	t.Helper()
	return artifactProjector(
		t,
		root,
		receivecontract.NewCatalogRootDirectoryTree(),
		nil,
	)
}

func artifactProjector(
	t *testing.T,
	root catalog.DirectoryID,
	artifact receivecontract.ArtifactSpec,
	artifactErr error,
) ArtifactPathProjector {
	t.Helper()
	if artifactErr != nil {
		t.Fatal(artifactErr)
	}
	projector, err := NewArtifactPathProjector(root, artifact)
	if err != nil {
		t.Fatal(err)
	}
	return projector
}

func sourceNode(
	t *testing.T,
	kind catalog.NodeKind,
	node catalog.NodeID,
	path string,
	role SourceNodeRole,
) AuthenticatedSourceNode {
	t.Helper()
	source := EmptySourceCatalogPath()
	var err error
	if path != "" {
		source, err = NewSourceCatalogPath(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := newAuthenticatedSourceNode(kind, node, source, role)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func projectionContractID[T ~[receivecontract.StableIdentityBytes]byte](seed byte) T {
	var value T
	for index := range value {
		value[index] = seed
	}
	return value
}

func projectionID[T ~[catalog.IdentityBytes]byte](seed byte) T {
	var value T
	for index := range value {
		value[index] = seed
	}
	return value
}
