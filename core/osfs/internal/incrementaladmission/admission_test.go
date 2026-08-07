package incrementaladmission

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
)

func TestValidateDirectoryRejectsUnboundShapes(t *testing.T) {
	intent := testIntent(t)
	generation := testGeneration(t, 0x31)
	root := transfer.OutputDirectory{
		DirectoryID: intent.SyntheticRoot(),
		Generation:  generation,
	}
	parent, err := transfer.NewDirectoryAdmissionWithSecret(bytes.Repeat([]byte{0x41}, sha256.Size), root)
	if err != nil {
		t.Fatal(err)
	}
	child := transfer.OutputDirectory{
		DirectoryID:     testDirectoryID(t, 0x42),
		Generation:      testGeneration(t, 0x43),
		ParentAdmission: parent,
		Path:            "nested",
	}

	for _, test := range []struct {
		name string
		dir  transfer.OutputDirectory
		want error
	}{
		{name: "root", dir: root},
		{name: "child", dir: child},
		{name: "zero directory", dir: transfer.OutputDirectory{Generation: generation}, want: transfer.ErrInvalidDirectoryAdmission},
		{name: "zero generation", dir: transfer.OutputDirectory{DirectoryID: root.DirectoryID}, want: transfer.ErrInvalidDirectoryAdmission},
		{name: "root parent", dir: transfer.OutputDirectory{DirectoryID: root.DirectoryID, Generation: generation, ParentAdmission: parent}, want: transfer.ErrDirectoryAdmissionMismatch},
		{name: "wrong root", dir: transfer.OutputDirectory{DirectoryID: child.DirectoryID, Generation: generation}, want: transfer.ErrDirectoryAdmissionMismatch},
		{name: "child parent", dir: transfer.OutputDirectory{DirectoryID: child.DirectoryID, Generation: child.Generation, Path: child.Path}, want: transfer.ErrInvalidDirectoryAdmission},
		{name: "child noncanonical", dir: transfer.OutputDirectory{DirectoryID: child.DirectoryID, Generation: child.Generation, ParentAdmission: parent, Path: "nested/../escape"}, want: transfer.ErrInvalidDirectoryAdmission},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateDirectory(intent, test.dir); !errors.Is(err, test.want) {
				t.Fatalf("ValidateDirectory() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestSameDirectoryComparesTheCommittedCatalogIdentity(t *testing.T) {
	modified, err := catalog.NewModifiedTime(123, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	directory := transfer.OutputDirectory{
		DirectoryID:  testDirectoryID(t, 0x51),
		Generation:   testGeneration(t, 0x52),
		Path:         "nested",
		ModifiedTime: modified,
	}
	parentDirectory := transfer.OutputDirectory{
		DirectoryID: testDirectoryID(t, 0x54), Generation: testGeneration(t, 0x55),
	}
	firstParent, err := transfer.NewDirectoryAdmissionWithSecret(
		bytes.Repeat([]byte{0x56}, sha256.Size), parentDirectory,
	)
	if err != nil {
		t.Fatal(err)
	}
	secondParent, err := transfer.NewDirectoryAdmissionWithSecret(
		bytes.Repeat([]byte{0x57}, sha256.Size), parentDirectory,
	)
	if err != nil {
		t.Fatal(err)
	}
	directory.ParentAdmission = firstParent
	if !SameDirectory(directory, directory) {
		t.Fatal("identical directory commitments did not compare equal")
	}
	childAdmission, err := transfer.NewDirectoryAdmissionWithSecret(
		bytes.Repeat([]byte{0x58}, sha256.Size), directory,
	)
	if err != nil {
		t.Fatal(err)
	}
	projected := directory
	projected.ParentAdmission = childAdmission.Parent()
	if !SameDirectory(directory, projected) {
		t.Fatal("token-only parent projection did not preserve authority equality")
	}
	changed := directory
	changed.Generation = testGeneration(t, 0x53)
	if SameDirectory(directory, changed) {
		t.Fatal("different directory generations compared equal")
	}
	changed = directory
	changed.ParentAdmission = secondParent
	if SameDirectory(directory, changed) {
		t.Fatal("different parent authority compared equal")
	}
}

func TestNewSecretRequiresFreshNonzeroEntropy(t *testing.T) {
	want := bytes.Repeat([]byte{0x61}, sha256.Size)
	secret, err := NewSecret(bytes.NewReader(want))
	if err != nil || !bytes.Equal(secret[:], want) {
		t.Fatalf("NewSecret() = %x, %v", secret, err)
	}
	for _, test := range []struct {
		name   string
		random *bytes.Reader
	}{
		{name: "nil"},
		{name: "short", random: bytes.NewReader([]byte{1})},
		{name: "zero", random: bytes.NewReader(make([]byte, sha256.Size))},
	} {
		t.Run(test.name, func(t *testing.T) {
			var source = interface{ Read([]byte) (int, error) }(test.random)
			if test.random == nil {
				source = nil
			}
			if _, err := NewSecret(source); err == nil {
				t.Fatal("NewSecret() accepted invalid entropy")
			}
		})
	}
}

func testIntent(t *testing.T) transfer.TransferIntent {
	t.Helper()
	share, err := catalog.ShareInstanceFromBytes(bytes.Repeat([]byte{0x11}, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	root := testDirectoryID(t, 0x21)
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := transfer.NewOutputBackendID("incremental-admission-test")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewFilesystemTransferIntent(share, root, rules, t.TempDir(), backend, transfer.OutputNativeTree)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func testDirectoryID(t *testing.T, fill byte) catalog.DirectoryID {
	t.Helper()
	id, err := catalog.DirectoryIDFromBytes(bytes.Repeat([]byte{fill}, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func testGeneration(t *testing.T, fill byte) catalog.DirectoryGeneration {
	t.Helper()
	generation, err := catalog.DirectoryGenerationFromBytes(bytes.Repeat([]byte{fill}, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	return generation
}
