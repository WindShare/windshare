package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func crossRuntimeID[T ~[16]byte](seed byte) T {
	var value T
	value[0], value[len(value)-1] = seed, seed^0xff
	return value
}

func crossRuntimeOpaque(seed byte) []byte {
	value := make([]byte, receivecontract.AuthorityRefBytes)
	value[0], value[len(value)-1] = seed, seed^0xff
	return value
}

func TestReceiveIntentCrossRuntimeCanonicalGolden(t *testing.T) {
	share := crossRuntimeID[catalog.ShareInstance](1)
	root := crossRuntimeID[catalog.DirectoryID](2)
	rules, err := NewSelectionRules(true, []SelectionOverride{
		{FileID: crossRuntimeID[catalog.FileID](5), Selected: false},
		{DirectoryID: crossRuntimeID[catalog.DirectoryID](4), Selected: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	operationBytes := crossRuntimeID[receivecontract.OperationID](10)
	operation, err := receivecontract.OperationIDFromBytes(operationBytes[:])
	if err != nil {
		t.Fatal(err)
	}
	reservationBytes := crossRuntimeID[receivecontract.DestinationReservationID](11)
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(reservationBytes[:])
	if err != nil {
		t.Fatal(err)
	}
	authority, err := receivecontract.AuthorityRefFromBytes(crossRuntimeOpaque(12))
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeContainerRootReservation(
		operation, reservationID, artifact, authority,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	const expectedDigest = "_-zVJFYomTUJqymN56HiUL1q6NyF_a69IEmAdVUMLl0"
	if encoded := base64.RawURLEncoding.EncodeToString(intent.Digest().Bytes()); encoded != expectedDigest {
		t.Fatalf("digest=%s want=%s", encoded, expectedDigest)
	}
}

func TestReceiveIntentCanonicalBindsSelectionArtifactAndPlan(t *testing.T) {
	share := transferID[catalog.ShareInstance](31)
	root := transferID[catalog.DirectoryID](32)
	file := transferID[catalog.FileID](33)
	directory := transferID[catalog.DirectoryID](34)
	rulesA, err := NewSelectionRules(false, []SelectionOverride{
		{DirectoryID: directory, Selected: true},
		{FileID: file, Selected: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	rulesB, err := NewSelectionRules(false, []SelectionOverride{
		{FileID: file, Selected: true},
		{DirectoryID: directory, Selected: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	left := receiveIntentFixture(t, share, root, rulesA, 1)
	right := receiveIntentFixture(t, share, root, rulesB, 1)
	if !left.EqualCanonical(right) || left.Digest() != right.Digest() {
		t.Fatal("equivalent selection maps produced different ReceiveIntent identity")
	}

	expected := append([]byte(receiveIntentDomain), 0, ReceiveIntentV3)
	expected = appendCanonicalField(expected, left.SelectionSpec().CanonicalBytes())
	expected = appendCanonicalField(expected, left.ArtifactSpec().CanonicalBytes())
	expected = appendCanonicalField(expected, left.MaterializationPlan().CanonicalBytes())
	if !bytes.Equal(left.CanonicalBytes(), expected) {
		t.Fatalf("canonical bytes=%x want=%x", left.CanonicalBytes(), expected)
	}
	sum := sha256.Sum256(expected)
	if left.Digest() != ReceiveIntentDigest(sum) {
		t.Fatalf("digest=%x want=%x", left.Digest(), sum)
	}
	if left.OperationID() != left.MaterializationPlan().OperationID() ||
		left.BindingDigest() != left.MaterializationPlan().BindingDigest() {
		t.Fatal("intent did not project its frozen plan identity")
	}

	differentOperation := receiveIntentFixture(t, share, root, rulesA, 2)
	if left.EqualCanonical(differentOperation) || left.Digest() == differentOperation.Digest() {
		t.Fatal("different stable operations shared ReceiveIntent identity")
	}
}

func TestReceiveIntentRejectsMismatchedOrIncompleteContracts(t *testing.T) {
	share := transferID[catalog.ShareInstance](41)
	root := transferID[catalog.DirectoryID](42)
	rules, err := NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	valid := receiveIntentFixture(t, share, root, rules, 3)
	if _, err := NewReceiveIntent(SelectionSpec{}, valid.ArtifactSpec(), valid.MaterializationPlan()); !errors.Is(err, ErrInvalidReceiveIntent) {
		t.Fatalf("zero selection error=%v", err)
	}
	otherArtifact, err := receivecontract.NewSingleFileDirectoryTree(
		transferID[catalog.FileID](43), "one.bin", "one.bin",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewReceiveIntent(valid.SelectionSpec(), otherArtifact, valid.MaterializationPlan()); !errors.Is(err, ErrInvalidReceiveIntent) {
		t.Fatalf("mismatched artifact/plan error=%v", err)
	}
	if _, err := NewReceiveIntent(valid.SelectionSpec(), valid.ArtifactSpec(), receivecontract.MaterializationPlan{}); !errors.Is(err, ErrInvalidReceiveIntent) {
		t.Fatalf("zero plan error=%v", err)
	}

	malformed := valid
	malformed.encoded = append([]byte(nil), valid.encoded...)
	malformed.encoded[len(malformed.encoded)-1] ^= 1
	if malformed.EqualCanonical(valid) || valid.EqualCanonical(malformed) ||
		malformed.EqualCanonical(malformed) || (ReceiveIntent{}).EqualCanonical(ReceiveIntent{}) {
		t.Fatal("malformed ReceiveIntent compared as canonical authority")
	}
}

func TestReceiveIntentAndRunIdentityAccessorsAreDefensive(t *testing.T) {
	share := transferID[catalog.ShareInstance](51)
	root := transferID[catalog.DirectoryID](52)
	rules, _ := NewSelectionRules(true, nil)
	intent := receiveIntentFixture(t, share, root, rules, 4)
	canonical := intent.CanonicalBytes()
	canonical[0] ^= 0xff
	if bytes.Equal(canonical, intent.CanonicalBytes()) {
		t.Fatal("canonical bytes leaked mutable storage")
	}
	digestBytes := intent.Digest().Bytes()
	digestBytes[0] ^= 0xff
	if bytes.Equal(digestBytes, intent.Digest().Bytes()) {
		t.Fatal("digest bytes leaked mutable storage")
	}
	jobID, err := NewTransferJobID()
	if err != nil || jobID.IsZero() || len(jobID.Bytes()) != TransferJobIdentityBytes {
		t.Fatalf("job ID=%x error=%v", jobID, err)
	}
	for _, invalid := range [][]byte{
		nil,
		make([]byte, ReceiveIntentDigestBytes-1),
		make([]byte, ReceiveIntentDigestBytes),
		make([]byte, ReceiveIntentDigestBytes+1),
	} {
		if _, err := ReceiveIntentDigestFromBytes(invalid); !errors.Is(err, ErrInvalidReceiveIntent) {
			t.Fatalf("digest length=%d error=%v", len(invalid), err)
		}
	}
}

func receiveIntentFixture(
	t *testing.T,
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules SelectionRules,
	identity byte,
) ReceiveIntent {
	t.Helper()
	selection, err := NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	operation, err := receivecontract.OperationIDFromBytes(
		bytes.Repeat([]byte{identity}, receivecontract.StableIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(
		bytes.Repeat([]byte{identity + 16}, receivecontract.StableIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := receivecontract.AuthorityRefFromBytes(
		bytes.Repeat([]byte{identity + 32}, receivecontract.AuthorityRefBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeContainerRootReservation(
		operation, reservationID, artifact, authority,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}
