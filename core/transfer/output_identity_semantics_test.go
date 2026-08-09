package transfer

import (
	"errors"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

func TestSelectionObservationByteBoundaryOwnsItsInput(t *testing.T) {
	observationBytes := make([]byte, SelectionObservationV1Bytes)
	observationBytes[0] = 21
	observation, err := SelectionObservationV1FromBytes(observationBytes)
	if err != nil {
		t.Fatal(err)
	}
	observationBytes[0] = 22
	observationCopy := observation.Bytes()
	observationCopy[0] = 23
	if observation.Bytes()[0] != 21 {
		t.Fatal("selection observation retained caller-owned byte storage")
	}
	if _, err := SelectionObservationV1FromBytes(make([]byte, SelectionObservationV1Bytes-1)); !errors.Is(err, ErrInvalidSelectionObservation) {
		t.Fatalf("short selection observation error = %v", err)
	}
	if _, err := SelectionObservationV1FromBytes(make([]byte, SelectionObservationV1Bytes)); !errors.Is(err, ErrInvalidSelectionObservation) {
		t.Fatalf("zero selection observation error = %v", err)
	}
}

func TestSelectionSpecIsOrderedAndImmutable(t *testing.T) {
	share := transferID[catalog.ShareInstance](41)
	root := transferID[catalog.DirectoryID](42)
	rulesA, err := NewPathSelectionRules([]string{"z.bin", "a.bin"})
	if err != nil {
		t.Fatal(err)
	}
	rulesB, err := NewPathSelectionRules([]string{"a.bin", "z.bin"})
	if err != nil {
		t.Fatal(err)
	}
	requestA, err := NewSelectionSpec(share, root, rulesA)
	if err != nil {
		t.Fatal(err)
	}
	requestB, err := NewSelectionSpec(share, root, rulesB)
	if err != nil || !slices.Equal(requestA.Bytes(), requestB.Bytes()) {
		t.Fatalf("path-order normalization failed: %v", err)
	}
	requestBytes := requestA.Bytes()
	requestBytes[0] ^= 0xff
	if slices.Equal(requestBytes, requestA.Bytes()) {
		t.Fatal("canonical request exposed internal byte storage")
	}

	invalidRules := SelectionRules{}
	if _, err := NewSelectionSpec(share, root, invalidRules); !errors.Is(err, ErrInvalidSelectionRules) {
		t.Fatalf("invalid canonical rules error = %v", err)
	}
}

func TestMaterializationBindingAccessorsPreserveCompleteAuthority(t *testing.T) {
	binding, _ := outputLifecycleFixture(t)
	target := binding.Target()
	descriptor := target.Descriptor()
	if target.OutputSessionID() != binding.OutputSessionID() ||
		target.ShareInstance() != binding.ShareInstance() ||
		target.FileID() != binding.FileID() ||
		target.FileRevision() != binding.FileRevision() ||
		target.ExactSize() != binding.ExactSize() ||
		target.Locator() != binding.Locator() ||
		binding.Descriptor() != descriptor {
		t.Fatalf("target/binding authority diverged: target=%+v binding=%+v", target, binding)
	}
	if _, err := BindFileMaterializationTarget(FileMaterializationTarget{}, binding.ObjectIdentity()); !errors.Is(err, ErrInvalidOutputBinding) {
		t.Fatalf("zero target binding error = %v", err)
	}

	short, err := content.NewRangeSet([]content.Range{{Offset: 0, End: 5}})
	if err != nil {
		t.Fatal(err)
	}
	long, err := content.NewRangeSet([]content.Range{{Offset: 0, End: 10}})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := MergeRanges(short, long)
	if err != nil || !slices.Equal(merged.Ranges(), []content.Range{{Offset: 0, End: 10}}) {
		t.Fatalf("same-origin range dominance = %v, error = %v", merged.Ranges(), err)
	}
	reversed, err := MergeRanges(long, short)
	if err != nil || !slices.Equal(reversed.Ranges(), merged.Ranges()) {
		t.Fatalf("range merge depended on caller order: %v, error = %v", reversed.Ranges(), err)
	}
	idempotent, err := MergeRanges(short, short)
	if err != nil || !slices.Equal(idempotent.Ranges(), short.Ranges()) {
		t.Fatalf("duplicate range merge = %v, error = %v", idempotent.Ranges(), err)
	}
}
