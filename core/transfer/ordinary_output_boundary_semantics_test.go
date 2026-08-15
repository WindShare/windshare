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
		ordinaryoutput.ArtifactPathProjector{},
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
	projector, err := OrdinaryOutputArtifactPathProjector(intent)
	if err != nil {
		t.Fatal(err)
	}
	scope := admissionTestScope(t, intent)
	secret := admissionTestSequence(0x31, directoryAdmissionSecretBytes)
	rootSource := admissionTestDirectory(
		t, rootID, ordinaryID[catalog.DirectoryGeneration](0xb4),
		DirectoryAdmission{}, "", catalog.ModifiedTime{},
	)
	rootAdmission, err := NewDirectoryAdmissionWithSecret(secret, scope, rootSource)
	if err != nil {
		t.Fatal(err)
	}
	childSource := admissionTestDirectory(
		t, ordinaryID[catalog.DirectoryID](0xb5), ordinaryID[catalog.DirectoryGeneration](0xb6),
		rootAdmission, "folder", catalog.ModifiedTime{},
	)
	request, err := NewDirectoryMaterializationRequest(
		projector, childSource, ordinaryoutput.SourceNodeSelected, MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	artifact, materialized := request.Projection().ArtifactPath()
	if !materialized || artifact.String() != "folder" ||
		!DirectoryMaterializationMatchesIntent(intent, request) ||
		DirectoryMaterializationMatchesProjector(ordinaryoutput.ArtifactPathProjector{}, request) {
		t.Fatalf("closed directory projection = (%q, %t)", artifact.String(), materialized)
	}

	fileSource, err := ordinaryoutput.NewSourceCatalogPath("folder/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := jobDescriptor(t, share, ordinaryID[catalog.FileID](0xb7), 0xb8, 17)
	file, err := NewMaterializationFile(
		projector, fileSource, descriptor, newJobOutput(share).session,
		rootAdmission, MaterializedDirectoryClaim{},
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
		for index := range typeOf.NumField() {
			if typeOf.Field(index).IsExported() {
				t.Fatalf("%s retains caller-settable field %s", typeOf, typeOf.Field(index).Name)
			}
		}
	}
	fileType := reflect.TypeOf(MaterializationFile{})
	destinationType := reflect.TypeOf(OutputDestinationPath{})
	for index := range fileType.NumField() {
		if fileType.Field(index).Type == destinationType {
			t.Fatalf("%s retains executor destination field %s", fileType, fileType.Field(index).Name)
		}
	}
	for _, retiredMethod := range []string{"BindDestination", "DestinationPath"} {
		if _, present := fileType.MethodByName(retiredMethod); present {
			t.Fatalf("%s retains executor destination method %s", fileType, retiredMethod)
		}
	}
}
