package receivecontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func contractID[T ~[16]byte](seed byte) T {
	var value T
	value[0], value[len(value)-1] = seed, seed^0xff
	return value
}

func contractOpaque(seed byte) []byte {
	value := make([]byte, AuthorityRefBytes)
	value[0], value[len(value)-1] = seed, seed^0xff
	return value
}

func contractStable(seed byte) []byte {
	value := make([]byte, StableIdentityBytes)
	value[0], value[len(value)-1] = seed, seed^0xff
	return value
}

func mustIdentity[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func TestIdentityValuesRejectMissingAuthorityAndDefendBytes(t *testing.T) {
	operation := mustIdentity(OperationIDFromBytes(contractStable(1)))
	reservation := mustIdentity(DestinationReservationIDFromBytes(contractStable(2)))
	authority := mustIdentity(AuthorityRefFromBytes(contractOpaque(5)))
	artifact := mustIdentity(ArtifactDigestFromBytes(contractOpaque(7)))
	binding := mustIdentity(BindingDigestFromBytes(contractOpaque(8)))
	values := [][]byte{
		operation.Bytes(), reservation.Bytes(),
		authority.Bytes(), artifact.Bytes(), binding.Bytes(),
	}
	for index, value := range values {
		value[0] ^= 0xff
		if valuesAgain := [][]byte{
			operation.Bytes(), reservation.Bytes(),
			authority.Bytes(), artifact.Bytes(), binding.Bytes(),
		}; bytes.Equal(value, valuesAgain[index]) {
			t.Fatalf("identity %d exposed mutable bytes", index)
		}
	}
	if operation.IsZero() || reservation.IsZero() ||
		authority.IsZero() || artifact.IsZero() || binding.IsZero() {
		t.Fatal("valid identity reported zero")
	}
	if !(OperationID{}).IsZero() || !(DestinationReservationID{}).IsZero() ||
		!(AuthorityRef{}).IsZero() ||
		!(ArtifactDigest{}).IsZero() || !(BindingDigest{}).IsZero() {
		t.Fatal("zero identity reported valid")
	}

	invalid := [][]byte{nil, make([]byte, StableIdentityBytes), make([]byte, StableIdentityBytes+1)}
	for _, raw := range invalid {
		if _, err := OperationIDFromBytes(raw); !errors.Is(err, ErrInvalidReceiveContract) {
			t.Fatalf("invalid operation error=%v", err)
		}
	}
	if _, err := AuthorityRefFromBytes(make([]byte, AuthorityRefBytes)); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("zero authority error=%v", err)
	}
	if _, err := AuthorityRefFromBytes(contractOpaque(8)[:AuthorityRefBytes-1]); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("short authority error=%v", err)
	}
	if _, err := ArtifactDigestFromBytes(nil); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("missing artifact digest error=%v", err)
	}
	if _, err := BindingDigestFromBytes(make([]byte, AuthorityRefBytes)); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("zero binding digest error=%v", err)
	}

	// Text round-trip and rejection of zero/malformed hex.
	text, err := operation.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	var decodedOp OperationID
	if err := decodedOp.UnmarshalText(text); err != nil || decodedOp != operation {
		t.Fatalf("operation text round-trip = (%x, %v)", decodedOp, err)
	}
	if _, err := (OperationID{}).MarshalText(); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("zero operation marshal error=%v", err)
	}
	if err := decodedOp.UnmarshalText([]byte("zz")); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("invalid hex unmarshal error=%v", err)
	}
}

func TestArtifactVariantsPreserveDistinctDigestsAndJSONRoundTrip(t *testing.T) {
	file := contractID[catalog.FileID](10)
	directory := contractID[catalog.DirectoryID](11)
	single, err := NewSingleFileDirectoryTree(file, "docs/report.txt", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	complete, err := NewCompleteDirectoryResultRoot(directory, "docs")
	if err != nil {
		t.Fatal(err)
	}
	completeTree, err := NewResultRootDirectoryTree(complete)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := NewDirectorySelectionResultRoot(directory, "docs")
	if err != nil {
		t.Fatal(err)
	}
	resultTree, err := NewResultRootDirectoryTree(selected)
	if err != nil {
		t.Fatal(err)
	}
	synthetic := NewSyntheticSelectionResultRoot()
	syntheticTree, err := NewResultRootDirectoryTree(synthetic)
	if err != nil {
		t.Fatal(err)
	}
	catalogTree := NewCatalogRootDirectoryTree()

	for name, artifact := range map[string]ArtifactSpec{
		"single":    single,
		"complete":  completeTree,
		"selection": resultTree,
		"synthetic": syntheticTree,
		"catalog":   catalogTree,
	} {
		t.Run(name, func(t *testing.T) {
			if artifact.Kind() != ArtifactDirectoryTree || artifact.Digest().IsZero() || artifact.IsZero() {
				t.Fatalf("invalid artifact=%+v", artifact)
			}
			encoded, err := json.Marshal(artifact)
			if err != nil {
				t.Fatal(err)
			}
			var decoded ArtifactSpec
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Digest() != artifact.Digest() || decoded.IsZero() {
				t.Fatalf("json round-trip digest=%x want=%x", decoded.Digest(), artifact.Digest())
			}
		})
	}

	singleLayout, ok := single.DirectoryTree()
	if !ok || singleLayout.Kind() != DirectoryTreeSingleFile {
		t.Fatalf("single layout=%+v ok=%v", singleLayout, ok)
	}
	if value, ok := singleLayout.SingleFile(); !ok || value.FileID != file ||
		value.SourcePath != "docs/report.txt" || value.SuggestedName != "report.txt" {
		t.Fatalf("single file=%+v ok=%v", value, ok)
	}
	resultLayout, ok := resultTree.DirectoryTree()
	if root, rootOK := resultLayout.ResultRoot(); !ok || !rootOK || root.Class() != ResultRootDirectorySelection ||
		root.AnchorKind() != ResultRootDirectoryAnchor || root.DirectoryID() != directory ||
		root.SourcePath() != "docs" || root.Name() != "docs-selection" {
		t.Fatalf("result root=%+v layoutOK=%v rootOK=%v", root, ok, rootOK)
	}
	if synthetic.Class() != ResultRootSyntheticSelection || synthetic.AnchorKind() != ResultRootSyntheticAnchor ||
		synthetic.Name() != DefaultResultRootName || synthetic.DirectoryID() != (catalog.DirectoryID{}) {
		t.Fatalf("synthetic=%+v", synthetic)
	}
	if resultTree.Digest() == syntheticTree.Digest() || completeTree.Digest() == resultTree.Digest() {
		t.Fatal("distinct artifact variants collided on digest")
	}

	longName := strings.Repeat("界", 84)
	protected, err := AppendProtectedSuffix(longName, PartialSelectionSuffix)
	if err != nil || len([]byte(protected)) > MaxResultComponentBytes || !strings.HasSuffix(protected, PartialSelectionSuffix) {
		t.Fatalf("protected=%q bytes=%d err=%v", protected, len([]byte(protected)), err)
	}
	invalidSingles := []struct{ path, name string }{
		{"docs/report.txt", "other.txt"}, {"docs/../report.txt", "report.txt"},
		{"docs/a\u202eb", "a\u202eb"}, {"", ""},
	}
	for _, test := range invalidSingles {
		if _, err := NewSingleFileDirectoryTree(file, test.path, test.name); !errors.Is(err, ErrInvalidReceiveContract) {
			t.Fatalf("invalid single %+v error=%v", test, err)
		}
	}
	if _, err := NewSingleFileDirectoryTree(file, "docs/.wsresume-file", ".wsresume-file"); err != nil {
		t.Fatalf("valid logical marker-shaped single file rejected: %v", err)
	}
	if _, err := NewSingleFileDirectoryTree(catalog.FileID{}, "report.txt", "report.txt"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("zero single-file error=%v", err)
	}
	if _, err := NewCompleteDirectoryResultRoot(catalog.DirectoryID{}, "docs"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("zero directory error=%v", err)
	}
	if _, err := NewResultRootDirectoryTree(ResultRootLayout{}); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("zero result-root error=%v", err)
	}
	if _, err := AppendProtectedSuffix("name", ""); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("empty suffix error=%v", err)
	}
	if _, ok := (ArtifactSpec{}).DirectoryTree(); ok || !(ArtifactSpec{}).IsZero() {
		t.Fatal("zero artifact exposed a variant")
	}
}

func TestReservationsBindingsAndPlansAreClosed(t *testing.T) {
	operation := mustIdentity(OperationIDFromBytes(contractStable(20)))
	reservationID := mustIdentity(DestinationReservationIDFromBytes(contractStable(22)))
	authority := mustIdentity(AuthorityRefFromBytes(contractOpaque(23)))
	file := contractID[catalog.FileID](27)
	directory := contractID[catalog.DirectoryID](28)
	single, _ := NewSingleFileDirectoryTree(file, "report.txt", "report.txt")
	root, _ := NewDirectorySelectionResultRoot(directory, "docs")
	tree, _ := NewResultRootDirectoryTree(root)
	catalogTree := NewCatalogRootDirectoryTree()

	nativeRoot, err := NewNativeContainerRootReservation(operation, reservationID, catalogTree, authority)
	if err != nil {
		t.Fatal(err)
	}
	singleName, _ := CollisionName(operation, "report.txt", 1, true)
	nativeEntry, err := NewNativeNamedEntryReservation(
		operation, reservationID, single, authority, singleName, singleName, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	resultName, _ := CollisionName(operation, "docs-selection", 2, false)
	resultEntry, err := NewNativeNamedEntryReservation(
		operation, reservationID, tree, authority, resultName, resultName, 2,
	)
	if err != nil {
		t.Fatal(err)
	}

	if nativeRoot.Kind() != ReservationContainerRoot || nativeRoot.OperationID() != operation ||
		nativeRoot.ID() != reservationID || nativeRoot.AuthorityRef() != authority ||
		nativeRoot.ArtifactDigest() != catalogTree.Digest() || nativeRoot.Digest().IsZero() {
		t.Fatalf("native root=%+v", nativeRoot)
	}
	if nativeEntry.Kind() != ReservationNamedContainerEntry || nativeEntry.EntryKind() != ContainerEntrySingleFile ||
		nativeEntry.RequestedName() != "report.txt" || nativeEntry.LogicalReservedName() != singleName ||
		nativeEntry.PhysicalName() != singleName || nativeEntry.CollisionIndex() != 1 {
		t.Fatal("reservation variant fields mismatch")
	}

	cases := []struct {
		name        string
		artifact    ArtifactSpec
		reservation DestinationReservation
	}{
		{"catalog-root", catalogTree, nativeRoot},
		{"single-file", single, nativeEntry},
		{"result-root", tree, resultEntry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := NewDirectTreePlan(tc.artifact, tc.reservation)
			if err != nil {
				t.Fatal(err)
			}
			if plan.IsZero() || plan.Kind() != PlanDirectTree ||
				plan.OperationID() != operation || plan.ArtifactDigest() != tc.artifact.Digest() ||
				plan.BindingDigest() != tc.reservation.Digest() {
				t.Fatalf("plan=%+v", plan)
			}
			raw, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := UnmarshalMaterializationPlanJSON(raw, tc.artifact)
			if err != nil || decoded.BindingDigest() != plan.BindingDigest() ||
				decoded.OperationID() != plan.OperationID() {
				t.Fatalf("plan json round-trip=(%+v, %v)", decoded, err)
			}
		})
	}

	if unchanged, err := CollisionName(operation, "report.txt", 0, true); err != nil || unchanged != "report.txt" {
		t.Fatalf("zero collision=%q err=%v", unchanged, err)
	}
	if _, err := CollisionName(OperationID{}, "report.txt", 1, true); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("zero-operation collision error=%v", err)
	}
	if _, err := NewNativeContainerRootReservation(operation, reservationID, tree, authority); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("result tree accepted as container root: %v", err)
	}
	if _, err := NewNativeNamedEntryReservation(
		operation, reservationID, single, authority, "wrong.txt", "wrong.txt", 1,
	); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("wrong collision name accepted: %v", err)
	}
	if !(MaterializationPlan{}).IsZero() || (MaterializationPlan{}).OperationID() != (OperationID{}) ||
		(MaterializationPlan{}).ArtifactDigest() != (ArtifactDigest{}) ||
		(MaterializationPlan{}).BindingDigest() != (BindingDigest{}) {
		t.Fatal("zero plan exposed authority")
	}
}

func TestReceiveContractArtifactEdgeCases(t *testing.T) {
	file := contractID[catalog.FileID](27)
	directory := contractID[catalog.DirectoryID](28)

	if _, err := AppendProtectedSuffix("", ".zip"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("empty base allowed")
	}
	if _, err := AppendProtectedSuffix("..", ".zip"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("relative base allowed")
	}
	if got, err := AppendProtectedSuffix(".wsresume", ".zip"); err != nil || got != ".wsresume.zip" {
		t.Fatalf("marker-shaped logical base = %q, %v", got, err)
	}
	if _, err := AppendProtectedSuffix("name", ""); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("empty suffix allowed")
	}
	if _, err := AppendProtectedSuffix("name", "\xff\xfe"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("invalid utf8 suffix allowed")
	}
	if _, err := AppendProtectedSuffix("name", strings.Repeat("a", MaxResultComponentBytes)); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("oversized suffix allowed")
	}

	if _, err := NewCompleteDirectoryResultRoot(catalog.DirectoryID{}, "folder"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("zero directory id allowed")
	}
	if _, err := NewDirectorySelectionResultRoot(catalog.DirectoryID{}, "folder"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("zero directory id allowed")
	}
	if _, err := NewSingleFileDirectoryTree(catalog.FileID{}, "file.txt", "file.txt"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("zero file id single allowed")
	}
	if _, err := NewSingleFileDirectoryTree(file, "folder/file.txt", "wrong.txt"); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatal("mismatched output name allowed")
	}

	root, err := NewDirectorySelectionResultRoot(directory, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if root.Class() != ResultRootDirectorySelection || root.AnchorKind() != ResultRootDirectoryAnchor ||
		root.DirectoryID() != directory || root.SourcePath() != "docs" || root.Name() != "docs-selection" {
		t.Fatalf("result root getters drifted: %+v", root)
	}
	synthetic := NewSyntheticSelectionResultRoot()
	if synthetic.Class() != ResultRootSyntheticSelection || synthetic.AnchorKind() != ResultRootSyntheticAnchor {
		t.Fatalf("synthetic root getters drifted: %+v", synthetic)
	}

	// Malformed JSON inputs must fail closed.
	for _, raw := range []string{
		`{}`,
		`{"layout":"unknown"}`,
		`{"layout":"catalog_root","extra":true}`,
		`{"layout":"catalog_root"} trailing`,
		`{"layout":"single_file","single_file":{"file_id":"bad","source_path":"a","suggested_name":"a"}}`,
		`{"layout":"result_root","result_root":{"class":"synthetic_selection","source_path":"x"}}`,
	} {
		var dst ArtifactSpec
		if err := json.Unmarshal([]byte(raw), &dst); err == nil {
			t.Fatalf("malformed artifact json %s accepted", raw)
		}
	}
}
