package incrementaladmission

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestValidateDirectoryRejectsUnboundShapes(t *testing.T) {
	intent := testIntent(t)
	scope, err := transfer.NewDirectoryAdmissionScope(intent)
	if err != nil {
		t.Fatal(err)
	}
	generation := testGeneration(t, 0x31)
	root := transfer.MaterializationDirectory{
		DirectoryID: intent.SyntheticRoot(),
		Generation:  generation,
	}
	parent, err := transfer.NewDirectoryAdmissionWithSecret(
		bytes.Repeat([]byte{0x41}, sha256.Size), scope, root,
	)
	if err != nil {
		t.Fatal(err)
	}
	child := transfer.MaterializationDirectory{
		DirectoryID:     testDirectoryID(t, 0x42),
		Generation:      testGeneration(t, 0x43),
		ParentAdmission: parent,
		Path:            "nested",
	}

	for _, test := range []struct {
		name string
		dir  transfer.MaterializationDirectory
		want error
	}{
		{name: "root", dir: root},
		{name: "child", dir: child},
		{name: "zero directory", dir: transfer.MaterializationDirectory{Generation: generation}, want: transfer.ErrInvalidDirectoryAdmission},
		{name: "zero generation", dir: transfer.MaterializationDirectory{DirectoryID: root.DirectoryID}, want: transfer.ErrInvalidDirectoryAdmission},
		{name: "root parent", dir: transfer.MaterializationDirectory{DirectoryID: root.DirectoryID, Generation: generation, ParentAdmission: parent}, want: transfer.ErrDirectoryAdmissionMismatch},
		{name: "wrong root", dir: transfer.MaterializationDirectory{DirectoryID: child.DirectoryID, Generation: generation}, want: transfer.ErrDirectoryAdmissionMismatch},
		{name: "child parent", dir: transfer.MaterializationDirectory{DirectoryID: child.DirectoryID, Generation: child.Generation, Path: child.Path}, want: transfer.ErrInvalidDirectoryAdmission},
		{name: "child noncanonical", dir: transfer.MaterializationDirectory{DirectoryID: child.DirectoryID, Generation: child.Generation, ParentAdmission: parent, Path: "nested/../escape"}, want: transfer.ErrInvalidDirectoryAdmission},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateDirectory(intent, test.dir); !errors.Is(err, test.want) {
				t.Fatalf("ValidateDirectory() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestSameDirectoryComparesTheCommittedCatalogIdentity(t *testing.T) {
	intent := testIntent(t)
	scope, err := transfer.NewDirectoryAdmissionScope(intent)
	if err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(123, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	directory := transfer.MaterializationDirectory{
		DirectoryID:  testDirectoryID(t, 0x51),
		Generation:   testGeneration(t, 0x52),
		Path:         "nested",
		ModifiedTime: modified,
	}
	parentDirectory := transfer.MaterializationDirectory{
		DirectoryID: intent.SyntheticRoot(), Generation: testGeneration(t, 0x55),
	}
	firstParent, err := transfer.NewDirectoryAdmissionWithSecret(
		bytes.Repeat([]byte{0x56}, sha256.Size), scope, parentDirectory,
	)
	if err != nil {
		t.Fatal(err)
	}
	secondParent, err := transfer.NewDirectoryAdmissionWithSecret(
		bytes.Repeat([]byte{0x57}, sha256.Size), scope, parentDirectory,
	)
	if err != nil {
		t.Fatal(err)
	}
	directory.ParentAdmission = firstParent
	if !SameDirectory(directory, directory) {
		t.Fatal("identical directory commitments did not compare equal")
	}
	childAdmission, err := transfer.NewDirectoryAdmissionWithSecret(
		bytes.Repeat([]byte{0x58}, sha256.Size), scope, directory,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(childAdmission.ParentToken(), firstParent.Bytes()) {
		t.Fatal("child admission omitted its exact parent token")
	}
	projected := directory
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

func testIntent(t *testing.T) transfer.ReceiveIntent {
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
	selection, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	operation, err := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{0x31}, receivecontract.StableIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat([]byte{0x41}, receivecontract.StableIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{0x51}, receivecontract.AuthorityRefBytes))
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeContainerRootReservation(operation, reservationID, artifact, authority)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
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
