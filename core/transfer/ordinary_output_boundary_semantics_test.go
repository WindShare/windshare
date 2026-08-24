package transfer

import (
	"errors"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
)

func TestOrdinaryOutputBoundaryRejectsUnfrozenAuthority(t *testing.T) {
	if _, err := (SelectionSpec{}).OrdinaryOutputSelection(); !errors.Is(err, ErrInvalidSelectionRules) {
		t.Fatalf("zero selection = %v", err)
	}
	valid, err := NewSelectionSpec(
		ordinaryID[catalog.ShareInstance](0xa1),
		ordinaryID[catalog.DirectoryID](0xa2),
		mustSelectionRules(t, true, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	invalidMode := valid
	invalidMode.rules.mode = SelectionMode(0xff)
	if _, err := invalidMode.OrdinaryOutputSelection(); !errors.Is(err, ErrInvalidSelectionRules) {
		t.Fatalf("unknown selection mode = %v", err)
	}
	if _, err := OrdinaryOutputArtifactPathProjector(ReceiveIntent{}); !errors.Is(
		err, ordinaryoutput.ErrInvalidArtifactProjector,
	) {
		t.Fatalf("zero intent projector = %v", err)
	}

	path, err := ordinaryoutput.NewSourceCatalogPath("folder")
	if err != nil {
		t.Fatal(err)
	}
	directory := ordinaryID[catalog.DirectoryID](0xa3)
	file := ordinaryID[catalog.FileID](0xa4)
	for name, input := range map[string]struct {
		kind      catalog.NodeKind
		directory catalog.DirectoryID
		file      catalog.FileID
	}{
		"directory-with-file": {catalog.NodeKindDirectory, directory, file},
		"file-with-directory": {catalog.NodeKindFile, directory, file},
		"unknown-kind":        {catalog.NodeKind(0xff), catalog.DirectoryID{}, catalog.FileID{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := OrdinaryOutputSourceNode(
				input.kind, input.directory, input.file, path, ordinaryoutput.SourceNodeSelected,
			); !errors.Is(err, ordinaryoutput.ErrInvalidAuthenticatedSource) {
				t.Fatalf("invalid source node = %v", err)
			}
		})
	}
	if _, err := NewDirectoryMaterializationRequest(
		admissionTestIntent(t, directory, 0xa5),
		AuthenticatedSourceDirectory{DirectoryID: directory},
		ordinaryoutput.SourceNodeSelected,
		MaterializedDirectoryClaim{},
	); !errors.Is(err, ordinaryoutput.ErrInvalidSourceCatalogPath) {
		t.Fatalf("invalid authenticated source path = %v", err)
	}
}

func TestOrdinaryOutputRequestsExposeNoSecondPathMappingAuthority(t *testing.T) {
	share := admissionTestShare(t, 0xb3)
	rootID := ordinaryID[catalog.DirectoryID](0xb2)
	intent := admissionTestIntent(t, rootID, 0xb3)
	scope := admissionTestScope(t, intent)
	secret := admissionTestSequence(0x31, directoryAdmissionSecretBytes)
	rootSource := admissionTestDirectory(
		t, rootID, ordinaryID[catalog.DirectoryGeneration](0xb4),
		DirectoryAdmission{}, "", catalog.ModifiedTime{},
	)
	rootRequest, err := NewDirectoryMaterializationRequest(
		intent, rootSource, ordinaryoutput.SourceNodeConnectsSelection, MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	rootDirectory, ok := rootRequest.Directory()
	if !ok {
		t.Fatal("catalog root did not project materialization authority")
	}
	rootAdmission, err := NewDirectoryAdmissionWithSecret(secret, scope, rootDirectory)
	if err != nil {
		t.Fatal(err)
	}
	childSource := admissionTestDirectory(
		t, ordinaryID[catalog.DirectoryID](0xb5), ordinaryID[catalog.DirectoryGeneration](0xb6),
		rootAdmission, "folder", catalog.ModifiedTime{},
	)
	request, err := NewDirectoryMaterializationRequest(
		intent, childSource, ordinaryoutput.SourceNodeSelected, MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	artifact, materialized := request.Projection().ArtifactPath()
	if !materialized || artifact.String() != "folder" ||
		!DirectoryMaterializationMatchesIntent(intent, request) {
		t.Fatalf("closed directory projection = (%q, %t)", artifact.String(), materialized)
	}
	childDirectory, ok := request.Directory()
	if !ok {
		t.Fatal("child directory did not project materialization authority")
	}
	childAdmission, err := NewDirectoryAdmissionWithSecret(secret, scope, childDirectory)
	if err != nil {
		t.Fatal(err)
	}
	childClaim, err := NewMaterializedDirectoryClaim(childAdmission, request)
	if err != nil {
		t.Fatal(err)
	}

	fileSource, err := ordinaryoutput.NewSourceCatalogPath("folder/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := jobDescriptor(t, share, ordinaryID[catalog.FileID](0xb7), 0xb8, 17)
	fileParent, err := NewDirectoryMaterializationFileParent(
		childSource.DirectoryID, childSource.Generation, childSource.SourcePath, childAdmission, childClaim,
	)
	if err != nil {
		t.Fatal(err)
	}
	relativePath, err := NewMaterializationRootRelativePath("folder/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	file, err := NewMaterializationFile(
		intent, fileSource, relativePath, descriptor, newJobOutput(share).session, fileParent,
	)
	if err != nil {
		t.Fatal(err)
	}
	if file.ArtifactPath().String() != "folder/file.bin" ||
		file.Target().Locator().CanonicalPath() != file.ArtifactPath().String() ||
		!MaterializationFileMatchesIntent(intent, file) {
		t.Fatalf("closed file projection = (%q, %+v)", file.ArtifactPath().String(), file.Target().Locator())
	}

	for _, value := range []any{DirectoryMaterializationRequest{}, MaterializationFile{}, MaterializedDirectoryClaim{}} {
		typeOf := reflect.TypeOf(value)
		for field := range typeOf.Fields() {
			if field.IsExported() {
				t.Fatalf("%s retains caller-settable field %s", typeOf, field.Name)
			}
		}
	}
	fileType := reflect.TypeFor[MaterializationFile]()
	destinationType := reflect.TypeFor[OutputDestinationPath]()
	for field := range fileType.Fields() {
		if field.Type == destinationType {
			t.Fatalf("%s retains executor destination field %s", fileType, field.Name)
		}
	}
	for _, retiredMethod := range []string{"BindDestination", "DestinationPath"} {
		if _, present := fileType.MethodByName(retiredMethod); present {
			t.Fatalf("%s retains executor destination method %s", fileType, retiredMethod)
		}
	}
}
