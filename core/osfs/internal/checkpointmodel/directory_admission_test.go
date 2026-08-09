package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestAdmittedDirectoryRoundTripRetainsIdentityWithoutPersistingCapability(t *testing.T) {
	intent, admission, owned, _ := admittedDirectoryFixture(t, 0x31, catalog.ModifiedTime{})
	record, err := NewAdmittedDirectory(intent, admission, owned)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeAdmittedDirectory(record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, admission.Bytes()) {
		t.Fatal("durable directory record persisted its runtime HMAC capability")
	}
	restored, err := DecodeAdmittedDirectory(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if restored.OperationID() != intent.OperationID() || restored.ReceiveIntentDigest() != admission.ReceiveIntentDigest() ||
		restored.DirectoryID() != admission.DirectoryID() || restored.Generation() != admission.Generation() ||
		restored.OwnedObjectID() != owned || restored.AdmissionDigest() != AggregateDigest(sha256.Sum256(admission.Bytes())) {
		t.Fatalf("restored admitted directory = %+v", restored)
	}
	for name, corrupt := range map[string][]byte{
		"truncated":    encoded[:len(encoded)-1],
		"trailing":     append(append([]byte(nil), encoded...), 0),
		"wrong-domain": append([]byte{'x'}, encoded[1:]...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeAdmittedDirectory(corrupt); !errors.Is(err, ErrInvalidAdmittedDirectory) {
				t.Fatalf("decode error = %v", err)
			}
		})
	}
}

func TestAdmittedDirectoryRequiresParentForCanonicalChild(t *testing.T) {
	intent, root, owned, scope := admittedDirectoryFixture(t, 0x41, catalog.ModifiedTime{})
	childID, _ := catalog.DirectoryIDFromBytes(bytes.Repeat([]byte{0x51}, catalog.IdentityBytes))
	generation, _ := catalog.DirectoryGenerationFromBytes(bytes.Repeat([]byte{0x52}, catalog.IdentityBytes))
	child, err := transfer.NewDirectoryAdmissionWithSecret(
		bytes.Repeat([]byte{0x53}, sha256.Size), scope,
		transfer.MaterializationDirectory{
			DirectoryID: childID, Generation: generation, ParentAdmission: root, Path: "child",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewAdmittedDirectory(intent, child, owned)
	if err != nil || record.ParentAdmissionDigest().IsZero() || record.CanonicalPath() != "child" {
		t.Fatalf("child record = (%+v, %v)", record, err)
	}
}

func admittedDirectoryFixture(
	t *testing.T,
	fill byte,
	modified catalog.ModifiedTime,
) (
	transfer.ReceiveIntent,
	transfer.DirectoryAdmission,
	transfer.OwnedObjectID,
	transfer.DirectoryAdmissionScope,
) {
	t.Helper()
	intent, root := directoryAdmissionIntent(t, fill)
	scope, err := transfer.NewDirectoryAdmissionScope(intent)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := catalog.DirectoryGenerationFromBytes(bytes.Repeat([]byte{fill + 1}, catalog.IdentityBytes))
	admission, err := transfer.NewDirectoryAdmissionWithSecret(
		bytes.Repeat([]byte{fill + 2}, sha256.Size), scope,
		transfer.MaterializationDirectory{DirectoryID: root, Generation: generation, ModifiedTime: modified},
	)
	if err != nil {
		t.Fatal(err)
	}
	owned, _ := transfer.OwnedObjectIDFromBytes(bytes.Repeat([]byte{fill + 3}, transfer.OwnedObjectIdentityBytes))
	return intent, admission, owned, scope
}

func directoryAdmissionIntent(t *testing.T, fill byte) (transfer.ReceiveIntent, catalog.DirectoryID) {
	t.Helper()
	share, _ := catalog.ShareInstanceFromBytes(bytes.Repeat([]byte{fill + 4}, catalog.IdentityBytes))
	root, _ := catalog.DirectoryIDFromBytes(bytes.Repeat([]byte{fill + 5}, catalog.IdentityBytes))
	rules, _ := transfer.NewSelectionRules(true, nil)
	selection, _ := transfer.NewSelectionSpec(share, root, rules)
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	operation, _ := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{fill + 6}, receivecontract.StableIdentityBytes))
	reservationID, _ := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat([]byte{fill + 7}, receivecontract.StableIdentityBytes))
	authority, _ := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{fill + 8}, receivecontract.AuthorityRefBytes))
	reservation, _ := receivecontract.NewNativeContainerRootReservation(operation, reservationID, artifact, authority)
	plan, _ := receivecontract.NewDirectTreePlan(artifact, reservation)
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	return intent, root
}
