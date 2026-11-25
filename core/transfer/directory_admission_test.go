package transfer

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
)

func TestDirectoryAdmissionValidatesHierarchyAndEquality(t *testing.T) {
	root := admissionTestDirectoryID(t, 0x20)
	rootDirectory := admissionTestDirectory(
		t, root, admissionTestGeneration(t, 0x50), DirectoryAdmission{}, "", catalog.ModifiedTime{},
	)
	rootAdmission, err := NewDirectoryAdmission(admissionTestMaterializationDirectory(t, rootDirectory, "windshare"))
	if err != nil {
		t.Fatal(err)
	}
	if rootAdmission.IsZero() || rootAdmission.DirectoryID() != root ||
		rootAdmission.Path() != "windshare" || rootAdmission.HasParent() {
		t.Fatalf("root admission=%+v", rootAdmission)
	}

	modified, err := catalog.NewModifiedTime(-2, 123_000_000, catalog.TimePrecisionMilliseconds)
	if err != nil {
		t.Fatal(err)
	}
	childDirectory := admissionTestDirectory(
		t, admissionTestDirectoryID(t, 0x30), admissionTestGeneration(t, 0x60),
		rootAdmission, "photos", modified,
	)
	childMaterialization := admissionTestMaterializationDirectory(t, childDirectory, "windshare/photos")
	childAdmission, err := NewDirectoryAdmission(childMaterialization)
	if err != nil {
		t.Fatal(err)
	}
	if childAdmission.IsZero() || childAdmission.DirectoryID() != childDirectory.DirectoryID ||
		childAdmission.Generation() != childDirectory.Generation ||
		childAdmission.Path() != "windshare/photos" || !childAdmission.HasParent() ||
		childAdmission.ParentPath() != "windshare" || childAdmission.ModifiedTime() != modified {
		t.Fatalf("child admission=%+v", childAdmission)
	}
	if !childAdmission.Equal(childAdmission) || childAdmission.Equal(rootAdmission) ||
		(DirectoryAdmission{}).Equal(DirectoryAdmission{}) {
		t.Fatal("directory admission equality semantics drifted")
	}
	if err := validateDirectoryAdmission(childAdmission, childMaterialization); err != nil {
		t.Fatalf("valid admission mismatch: %v", err)
	}
	if err := validateDirectoryAdmission(rootAdmission, childMaterialization); !errors.Is(err, ErrDirectoryAdmissionMismatch) {
		t.Fatalf("mismatched admission error=%v", err)
	}

	// Non-immediate parent must be rejected.
	grandchild := admissionTestDirectory(
		t, admissionTestDirectoryID(t, 0x36), admissionTestGeneration(t, 0x66),
		rootAdmission, "child/grandchild", catalog.ModifiedTime{},
	)
	if _, err := NewDirectoryAdmission(admissionTestMaterializationDirectory(t, grandchild, "windshare/child/grandchild")); !errors.Is(err, ErrInvalidDirectoryAdmission) {
		t.Fatalf("non-immediate parent error=%v", err)
	}
	// Parent on top-level result root path must be rejected.
	parentedRoot := admissionTestDirectory(
		t, root, admissionTestGeneration(t, 0x51), rootAdmission, "", catalog.ModifiedTime{},
	)
	if _, err := NewDirectoryAdmission(admissionTestMaterializationDirectory(t, parentedRoot, "windshare")); !errors.Is(err, ErrInvalidDirectoryAdmission) {
		t.Fatalf("parented root error=%v", err)
	}
	// Empty materialization path must be rejected.
	if _, err := NewMaterializationRootRelativePath(""); !errors.Is(err, ErrInvalidOutputBinding) {
		t.Fatalf("empty materialization path error=%v", err)
	}
}

func admissionTestIntent(t *testing.T, root catalog.DirectoryID, targetFirst byte) ReceiveIntent {
	t.Helper()
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	return testReceiveIntent(t, admissionTestShare(t, targetFirst), root, rules)
}

func admissionTestDirectory(
	t *testing.T,
	directory catalog.DirectoryID,
	generation catalog.DirectoryGeneration,
	parent DirectoryAdmission,
	path string,
	modified catalog.ModifiedTime,
) AuthenticatedSourceDirectory {
	t.Helper()
	sourcePath := ordinaryoutput.EmptySourceCatalogPath()
	if path != "" {
		var err error
		sourcePath, err = ordinaryoutput.NewSourceCatalogPath(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	return AuthenticatedSourceDirectory{
		DirectoryID: directory, Generation: generation, ParentAdmission: parent,
		SourcePath: sourcePath, ModifiedTime: modified,
	}
}

func admissionTestMaterializationDirectory(
	t *testing.T,
	source AuthenticatedSourceDirectory,
	paths ...string,
) MaterializationDirectory {
	t.Helper()
	path := source.SourcePath.String()
	if len(paths) > 1 {
		t.Fatal("at most one materialization path is allowed")
	}
	if len(paths) == 1 {
		path = paths[0]
	}
	relative, err := NewMaterializationRootRelativePath(path)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := NewMaterializationDirectory(
		source.DirectoryID, source.Generation, relative, source.ParentAdmission, source.ModifiedTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

func admissionTestShare(t *testing.T, first byte) catalog.ShareInstance {
	t.Helper()
	value, err := catalog.ShareInstanceFromBytes(admissionTestSequence(first, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func admissionTestDirectoryID(t *testing.T, first byte) catalog.DirectoryID {
	t.Helper()
	value, err := catalog.DirectoryIDFromBytes(admissionTestSequence(first, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func admissionTestGeneration(t *testing.T, first byte) catalog.DirectoryGeneration {
	t.Helper()
	value, err := catalog.DirectoryGenerationFromBytes(admissionTestSequence(first, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func admissionTestSequence(first byte, count int) []byte {
	value := make([]byte, count)
	for index := range value {
		value[index] = first + byte(index)
	}
	return value
}
