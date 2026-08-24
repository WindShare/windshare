package transfer

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestOrdinaryOutputSelectionPreservesCanonicalSelectionBytes(t *testing.T) {
	share := ordinaryID[catalog.ShareInstance](1)
	root := ordinaryID[catalog.DirectoryID](2)
	file := ordinaryID[catalog.FileID](3)
	directory := ordinaryID[catalog.DirectoryID](4)

	tests := []SelectionRules{
		mustSelectionRules(t, true, nil),
		mustPathRules(t, []string{"folder/file.bin"}),
		mustSelectionRules(t, false, []SelectionOverride{
			{FileID: file, Selected: true, Ancestors: []catalog.DirectoryID{directory}},
			{DirectoryID: directory, Selected: false},
		}),
	}
	for _, rules := range tests {
		selection, err := NewSelectionSpec(share, root, rules)
		if err != nil {
			t.Fatal(err)
		}
		before := selection.CanonicalBytes()
		input, err := selection.OrdinaryOutputSelection()
		if err != nil || input.Digest() != [32]byte(selection.Digest()) {
			t.Fatalf("input digest=%x err=%v", input.Digest(), err)
		}
		if !bytes.Equal(before, selection.CanonicalBytes()) {
			t.Fatal("shape lowering mutated canonical SelectionSpec bytes")
		}
	}
}

func TestOrdinaryOutputArtifactUsesExistingCanonicalConstructors(t *testing.T) {
	file := ordinaryID[catalog.FileID](3)
	directory := ordinaryID[catalog.DirectoryID](4)
	singleDecision, singleDecisionErr := ordinaryoutput.NewSingleFileShape(file, "folder/file.bin")
	singleArtifact, singleArtifactErr := receivecontract.NewSingleFileDirectoryTree(file, "folder/file.bin", "file.bin")
	completeDecision, completeDecisionErr := ordinaryoutput.NewCompleteDirectoryShape(directory, "folder/photos")
	completeLayout, completeLayoutErr := receivecontract.NewCompleteDirectoryResultRoot(directory, "folder/photos")
	partialDecision, partialDecisionErr := ordinaryoutput.NewPartialDirectoryShape(directory, "folder/photos")
	partialLayout, partialLayoutErr := receivecontract.NewDirectorySelectionResultRoot(directory, "folder/photos")
	syntheticDecision, syntheticDecisionErr := ordinaryoutput.NewSyntheticSelectionShape(
		ordinaryoutput.ShapeFallbackMultipleRoots,
	)
	tests := []struct {
		name     string
		decision ordinaryoutput.ShapeDecision
		want     receivecontract.ArtifactSpec
	}{
		{
			name:     "single",
			decision: mustShapeDecision(t, singleDecision, singleDecisionErr),
			want:     mustArtifact(t, singleArtifact, singleArtifactErr),
		},
		{
			name:     "complete",
			decision: mustShapeDecision(t, completeDecision, completeDecisionErr),
			want:     resultRootArtifact(t, completeLayout, completeLayoutErr),
		},
		{
			name:     "partial",
			decision: mustShapeDecision(t, partialDecision, partialDecisionErr),
			want:     resultRootArtifact(t, partialLayout, partialLayoutErr),
		},
		{
			name:     "synthetic",
			decision: mustShapeDecision(t, syntheticDecision, syntheticDecisionErr),
			want:     resultRootArtifactValue(t, receivecontract.NewSyntheticSelectionResultRoot()),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := MaterializeOrdinaryOutputShape(test.decision)
			if err != nil || !bytes.Equal(got.CanonicalBytes(), test.want.CanonicalBytes()) ||
				got.Digest() != test.want.Digest() {
				t.Fatalf("artifact digest=%x want=%x err=%v", got.Digest(), test.want.Digest(), err)
			}

			operation, _ := receivecontract.OperationIDFromBytes(
				bytes.Repeat([]byte{4}, receivecontract.StableIdentityBytes),
			)
			reservationID, _ := receivecontract.DestinationReservationIDFromBytes(
				bytes.Repeat([]byte{5}, receivecontract.StableIdentityBytes),
			)
			authority, _ := receivecontract.AuthorityRefFromBytes(
				bytes.Repeat([]byte{6}, receivecontract.AuthorityRefBytes),
			)
			reservedName, nameErr := receivecontract.CollisionName(
				operation,
				test.decision.PreferredName(),
				0,
				test.decision.Kind() == ordinaryoutput.ShapeSingleFile,
			)
			reservation, reservationErr := receivecontract.NewNativeNamedEntryReservation(
				operation, reservationID, got, authority, reservedName, 0,
			)
			plan, planErr := receivecontract.NewDirectTreePlan(got, reservation)
			if nameErr != nil || reservationErr != nil || planErr != nil || plan.IsZero() ||
				plan.ArtifactDigest() != got.Digest() {
				t.Fatalf(
					"one-time canonical materialization failed: name=%q nameErr=%v reservationErr=%v planErr=%v",
					reservedName, nameErr, reservationErr, planErr,
				)
			}
		})
	}
}

func TestMaterializeOrdinaryOutputShapeRejectsZeroDecision(t *testing.T) {
	if artifact, err := MaterializeOrdinaryOutputShape(ordinaryoutput.ShapeDecision{}); err == nil || !artifact.IsZero() {
		t.Fatalf("artifact=%+v err=%v", artifact, err)
	}
}

func TestOrdinaryOutputArtifactProjectorConsumesIntentWithoutChangingCanonicalBytes(t *testing.T) {
	share := ordinaryID[catalog.ShareInstance](1)
	root := ordinaryID[catalog.DirectoryID](2)
	file := ordinaryID[catalog.FileID](3)
	rules := mustSelectionRules(t, true, nil)
	selection, _ := NewSelectionSpec(share, root, rules)
	artifact, _ := receivecontract.NewSingleFileDirectoryTree(file, "folder/file.bin", "file.bin")
	operation, _ := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{4}, receivecontract.StableIdentityBytes))
	reservationID, _ := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat([]byte{5}, receivecontract.StableIdentityBytes))
	authority, _ := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{6}, receivecontract.AuthorityRefBytes))
	reservation, err := receivecontract.NewNativeNamedEntryReservation(
		operation, reservationID, artifact, authority, "file.bin", 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := receivecontract.NewDirectTreePlan(artifact, reservation)
	intent, _ := NewReceiveIntent(selection, artifact, plan)
	before := intent.CanonicalBytes()

	projector, err := OrdinaryOutputArtifactPathProjector(intent)
	if err != nil {
		t.Fatal(err)
	}
	sourcePath, _ := ordinaryoutput.NewSourceCatalogPath("folder/file.bin")
	node, err := OrdinaryOutputSourceNode(
		catalog.NodeKindFile, catalog.DirectoryID{}, file,
		sourcePath, ordinaryoutput.SourceNodeSelected,
	)
	if err != nil {
		t.Fatal(err)
	}
	projection := projector.Project(node)
	path, ok := projection.ArtifactPath()
	if !ok || path.String() != "file.bin" {
		t.Fatalf("projection=%+v path=%q", projection, path.String())
	}
	if !bytes.Equal(before, intent.CanonicalBytes()) {
		t.Fatal("projector construction mutated canonical ReceiveIntent bytes")
	}
}

func mustSelectionRules(
	t *testing.T,
	defaultSelected bool,
	overrides []SelectionOverride,
) SelectionRules {
	t.Helper()
	rules, err := NewSelectionRules(defaultSelected, overrides)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func mustPathRules(t *testing.T, paths []string) SelectionRules {
	t.Helper()
	rules, err := NewPathSelectionRules(paths)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func mustShapeDecision(
	t *testing.T,
	decision ordinaryoutput.ShapeDecision,
	err error,
) ordinaryoutput.ShapeDecision {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func mustArtifact(
	t *testing.T,
	artifact receivecontract.ArtifactSpec,
	err error,
) receivecontract.ArtifactSpec {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func resultRootArtifact(
	t *testing.T,
	layout receivecontract.ResultRootLayout,
	err error,
) receivecontract.ArtifactSpec {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return resultRootArtifactValue(t, layout)
}

func resultRootArtifactValue(
	t *testing.T,
	layout receivecontract.ResultRootLayout,
) receivecontract.ArtifactSpec {
	t.Helper()
	artifact, err := receivecontract.NewResultRootDirectoryTree(layout)
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func ordinaryID[T ~[catalog.IdentityBytes]byte](seed byte) T {
	var value T
	for index := range value {
		value[index] = seed
	}
	return value
}

func TestOrdinaryOutputSourceNodeValidators(t *testing.T) {
	dirID := ordinaryID[catalog.DirectoryID](1)
	fileID := ordinaryID[catalog.FileID](2)
	sourcePath, err := ordinaryoutput.NewSourceCatalogPath("folder/file.bin")
	if err != nil {
		t.Fatal(err)
	}

	// Directory node with invalid parameters
	if _, err := OrdinaryOutputSourceNode(catalog.NodeKindDirectory, catalog.DirectoryID{}, catalog.FileID{}, sourcePath, ordinaryoutput.SourceNodeSelected); !errors.Is(err, ordinaryoutput.ErrInvalidAuthenticatedSource) {
		t.Fatalf("zero directory id error = %v", err)
	}
	if _, err := OrdinaryOutputSourceNode(catalog.NodeKindDirectory, dirID, fileID, sourcePath, ordinaryoutput.SourceNodeSelected); !errors.Is(err, ordinaryoutput.ErrInvalidAuthenticatedSource) {
		t.Fatalf("directory with file id error = %v", err)
	}

	// File node with invalid parameters
	if _, err := OrdinaryOutputSourceNode(catalog.NodeKindFile, catalog.DirectoryID{}, catalog.FileID{}, sourcePath, ordinaryoutput.SourceNodeSelected); !errors.Is(err, ordinaryoutput.ErrInvalidAuthenticatedSource) {
		t.Fatalf("zero file id error = %v", err)
	}
	if _, err := OrdinaryOutputSourceNode(catalog.NodeKindFile, dirID, fileID, sourcePath, ordinaryoutput.SourceNodeSelected); !errors.Is(err, ordinaryoutput.ErrInvalidAuthenticatedSource) {
		t.Fatalf("file with directory id error = %v", err)
	}

	// Unknown node kind
	if _, err := OrdinaryOutputSourceNode(catalog.NodeKind(255), dirID, catalog.FileID{}, sourcePath, ordinaryoutput.SourceNodeSelected); !errors.Is(err, ordinaryoutput.ErrInvalidAuthenticatedSource) {
		t.Fatalf("unknown node kind error = %v", err)
	}
}

func TestOrdinaryOutputShapeDecisionAndProjectorValidation(t *testing.T) {
	// Invalid shape decision
	if _, err := MaterializeOrdinaryOutputShape(ordinaryoutput.ShapeDecision{}); !errors.Is(err, ordinaryoutput.ErrInvalidShapeResolution) {
		t.Fatalf("invalid shape decision error = %v", err)
	}

	// Zero intent projector
	if _, err := OrdinaryOutputArtifactPathProjector(ReceiveIntent{}); !errors.Is(err, ordinaryoutput.ErrInvalidArtifactProjector) {
		t.Fatalf("zero intent projector error = %v", err)
	}

	// Zero selection spec
	if _, err := (SelectionSpec{}).OrdinaryOutputSelection(); !errors.Is(err, ErrInvalidSelectionRules) {
		t.Fatalf("zero selection error = %v", err)
	}
}

func TestOrdinaryOutputMaterializationRequestAndClaimAccessors(t *testing.T) {
	// Zero claim accessors
	zeroClaim := MaterializedDirectoryClaim{}
	if zeroClaim.Valid() || !zeroClaim.Admission().IsZero() || zeroClaim.Path().Valid() {
		t.Fatalf("zero claim invalidity semantics drifted: %+v", zeroClaim)
	}

	// NewDirectoryMaterializationRequest with invalid source path
	intent := admissionTestIntent(t, ordinaryID[catalog.DirectoryID](1), 0x91)
	if _, err := NewDirectoryMaterializationRequest(intent, AuthenticatedSourceDirectory{}, ordinaryoutput.SourceNodeSelected, zeroClaim); err == nil {
		t.Fatal("invalid source path request succeeded")
	}

	// MatchesProjector and MatchesIntent with zero values
	if DirectoryMaterializationMatchesIntent(ReceiveIntent{}, DirectoryMaterializationRequest{}) {
		t.Fatal("zero intent matched materialization")
	}
	if MaterializationFileMatchesIntent(ReceiveIntent{}, MaterializationFile{}) {
		t.Fatal("zero intent matched file")
	}

	// File accessors on zero MaterializationFile
	zeroFile := MaterializationFile{}
	if zeroFile.SourcePath().Valid() || zeroFile.ArtifactPath().Valid() || zeroFile.ExpectedSize() != 0 ||
		!zeroFile.Descriptor().ShareInstance().IsZero() || zeroFile.Parent().Kind() != 0 ||
		zeroFile.ParentMaterialization().Valid() {
		t.Fatalf("zero file accessors drifted: %+v", zeroFile)
	}
}
