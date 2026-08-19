package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestCheckpointLineageCanonicalFramingSeparatesEveryCoordinate(t *testing.T) {
	baseRecord := mustCanonicalRecord(t, canonicalRecordSpec(t))
	base, err := baseRecord.CheckpointLineageSpec()
	if err != nil {
		t.Fatal(err)
	}
	baseID, err := DeriveCheckpointLineageID(base)
	if err != nil {
		t.Fatal(err)
	}

	changedOperation, _ := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{0x22}, receivecontract.StableIdentityBytes))
	changedIntent, _ := transfer.ReceiveIntentDigestFromBytes(bytes.Repeat([]byte{0x32}, sha256.Size))
	changedBinding, _ := receivecontract.BindingDigestFromBytes(bytes.Repeat([]byte{0x3a}, sha256.Size))
	changedFile, _ := catalog.FileIDFromBytes(bytes.Repeat([]byte{0x11}, catalog.IdentityBytes))
	changedAuthority, _ := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{0x42}, receivecontract.AuthorityRefBytes))

	changes := map[string]func(CheckpointLineageSpec) CheckpointLineageSpec{
		"operation": func(spec CheckpointLineageSpec) CheckpointLineageSpec {
			spec.OperationID = changedOperation
			return spec
		},
		"receive intent": func(spec CheckpointLineageSpec) CheckpointLineageSpec {
			spec.ReceiveIntentDigest = changedIntent
			return spec
		},
		"materialization binding": func(spec CheckpointLineageSpec) CheckpointLineageSpec {
			spec.MaterializationBindingDigest = changedBinding
			return spec
		},
		"file": func(spec CheckpointLineageSpec) CheckpointLineageSpec {
			spec.FileID = changedFile
			return spec
		},
		"canonical path": func(spec CheckpointLineageSpec) CheckpointLineageSpec {
			spec.CanonicalPath = "folder/other.bin"
			return spec
		},
		"materializer": func(spec CheckpointLineageSpec) CheckpointLineageSpec {
			spec.MaterializerKind = MaterializerFSATree
			return spec
		},
		"authority": func(spec CheckpointLineageSpec) CheckpointLineageSpec {
			spec.AuthorityRef = changedAuthority
			return spec
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			changed := change(base)
			changedID, deriveErr := DeriveCheckpointLineageID(changed)
			if deriveErr != nil {
				t.Fatal(deriveErr)
			}
			if changedID == baseID || SameCheckpointLineageSpec(base, changed) {
				t.Fatalf("%s did not separate the lineage", name)
			}
		})
	}

	canonical, err := CanonicalCheckpointLineageBytes(base)
	if err != nil {
		t.Fatal(err)
	}
	expectedPath := append(lineageRawU64(2), lineageFrame([]byte("folder"))...)
	expectedPath = append(expectedPath, lineageFrame([]byte("file.bin"))...)
	expected := append([]byte(CheckpointLineageDomain), lineageFrame(base.OperationID.Bytes())...)
	expected = append(expected, lineageFrame(base.ReceiveIntentDigest.Bytes())...)
	expected = append(expected, lineageFrame(base.MaterializationBindingDigest.Bytes())...)
	expected = append(expected, lineageFrame(base.FileID.Bytes())...)
	expected = append(expected, lineageFrame(expectedPath)...)
	expected = append(expected, lineageFrame([]byte{byte(base.MaterializerKind)})...)
	expected = append(expected, lineageFrame(base.AuthorityRef.Bytes())...)
	if !bytes.Equal(canonical, expected) {
		t.Fatalf("lineage preimage does not use the frozen u64be framing\n got %x\nwant %x", canonical, expected)
	}

	segmentedA := base
	segmentedA.CanonicalPath = "a/bc"
	segmentedB := base
	segmentedB.CanonicalPath = "ab/c"
	aID, _ := DeriveCheckpointLineageID(segmentedA)
	bID, _ := DeriveCheckpointLineageID(segmentedB)
	if aID == bID {
		t.Fatal("path segment framing is ambiguous")
	}
}

func TestCheckpointLineageIsOrthogonalToPhysicalRevisionAndProgress(t *testing.T) {
	baseSpec := canonicalRecordSpec(t)
	base := mustCanonicalRecord(t, baseSpec)
	baseLineage, err := base.CheckpointLineageID()
	if err != nil {
		t.Fatal(err)
	}

	changedRevision, _ := content.FileRevisionFromBytes(bytes.Repeat([]byte{0x61}, content.IdentityBytes))
	variants := map[string]struct {
		changeRecordID bool
		mutate         func(*RecordSpec)
	}{
		"revision": {true, func(spec *RecordSpec) { spec.FileRevision = changedRevision }},
		"size":     {true, func(spec *RecordSpec) { spec.ExactSize = 96 }},
		"owned object": {true, func(spec *RecordSpec) {
			spec.OwnedObjectID = bytes.Repeat([]byte{0x52}, sha256.Size)
		}},
		"ranges": {false, func(spec *RecordSpec) {
			spec.VerifiedRanges = []Range{{Offset: 0, End: 8}, {Offset: 24, End: 64}}
		}},
		"generations": {false, func(spec *RecordSpec) {
			spec.StateGeneration = 2
			spec.CheckpointGeneration = 2
		}},
		"lifecycle": {false, func(spec *RecordSpec) { spec.Phase = PhasePaused }},
	}
	for name, variant := range variants {
		t.Run(name, func(t *testing.T) {
			spec := baseSpec
			spec.VerifiedRanges = append([]Range(nil), baseSpec.VerifiedRanges...)
			spec.AuthorityRef = append([]byte(nil), baseSpec.AuthorityRef...)
			spec.OwnedObjectID = append([]byte(nil), baseSpec.OwnedObjectID...)
			variant.mutate(&spec)
			record := mustCanonicalRecord(t, spec)
			lineage, lineageErr := record.CheckpointLineageID()
			if lineageErr != nil {
				t.Fatal(lineageErr)
			}
			if lineage != baseLineage || !SameCheckpointLineage(base, record) {
				t.Fatalf("%s moved the logical lineage", name)
			}
			if (record.RecordID() != base.RecordID()) != variant.changeRecordID {
				t.Fatalf("%s RecordID orthogonality changed", name)
			}
		})
	}
}

func TestCheckpointLineageRejectsInvalidSpecsAndDefensivelyCopiesIDs(t *testing.T) {
	base, err := mustCanonicalRecord(t, canonicalRecordSpec(t)).CheckpointLineageSpec()
	if err != nil {
		t.Fatal(err)
	}
	invalid := []CheckpointLineageSpec{
		{},
		func() CheckpointLineageSpec {
			value := base
			value.OperationID = receivecontract.OperationID{}
			return value
		}(),
		func() CheckpointLineageSpec {
			value := base
			value.ReceiveIntentDigest = transfer.ReceiveIntentDigest{}
			return value
		}(),
		func() CheckpointLineageSpec {
			value := base
			value.MaterializationBindingDigest = receivecontract.BindingDigest{}
			return value
		}(),
		func() CheckpointLineageSpec { value := base; value.FileID = catalog.FileID{}; return value }(),
		func() CheckpointLineageSpec { value := base; value.CanonicalPath = "folder/../file.bin"; return value }(),
		func() CheckpointLineageSpec { value := base; value.MaterializerKind = 0; return value }(),
		func() CheckpointLineageSpec {
			value := base
			value.AuthorityRef = receivecontract.AuthorityRef{}
			return value
		}(),
	}
	for index, spec := range invalid {
		if _, deriveErr := DeriveCheckpointLineageID(spec); deriveErr == nil {
			t.Fatalf("invalid lineage spec %d was accepted", index)
		}
	}
	if SameCheckpointLineageSpec(CheckpointLineageSpec{}, CheckpointLineageSpec{}) {
		t.Fatal("invalid lineage specs compared equal")
	}
	if _, err := (Record{}).CheckpointLineageID(); err == nil {
		t.Fatal("zero record derived a lineage")
	}

	id, err := DeriveCheckpointLineageID(base)
	if err != nil {
		t.Fatal(err)
	}
	raw := id.Bytes()
	raw[0] ^= 0xff
	if bytes.Equal(raw, id.Bytes()) || id.IsZero() {
		t.Fatal("lineage ID bytes alias internal state")
	}
}

func TestClassifyCheckpointLineageFreezesMixedConflictPrecedence(t *testing.T) {
	revisionA, _ := content.FileRevisionFromBytes(bytes.Repeat([]byte{0x61}, content.IdentityBytes))
	revisionB, _ := content.FileRevisionFromBytes(bytes.Repeat([]byte{0x62}, content.IdentityBytes))
	objectA, _ := ObjectIDFromBytes(bytes.Repeat([]byte{0x71}, sha256.Size))
	objectB, _ := ObjectIDFromBytes(bytes.Repeat([]byte{0x72}, sha256.Size))
	request := CheckpointLineageRequest{FileRevision: revisionA, ExactSize: 64}
	exact := CheckpointLineageEvidence{FileRevision: revisionA, ExactSize: 64, OwnedObjectID: objectA}
	otherRevision := CheckpointLineageEvidence{FileRevision: revisionB, ExactSize: 64, OwnedObjectID: objectA}
	invalidSize := CheckpointLineageEvidence{FileRevision: revisionA, ExactSize: 65, OwnedObjectID: objectA}
	otherObject := CheckpointLineageEvidence{FileRevision: revisionA, ExactSize: 64, OwnedObjectID: objectB}

	tests := []struct {
		name          string
		request       CheckpointLineageRequest
		evidence      []CheckpointLineageEvidence
		crossConflict bool
		want          CheckpointLineageDecision
	}{
		{"absent", request, nil, false, CheckpointLineageDecisionAbsent},
		{"exact", request, []CheckpointLineageEvidence{exact}, false, CheckpointLineageDecisionExact},
		{"duplicate physical observation", request, []CheckpointLineageEvidence{exact, exact}, false, CheckpointLineageDecisionExact},
		{"revision conflict", request, []CheckpointLineageEvidence{otherRevision}, false, CheckpointLineageDecisionRevisionConflict},
		{"invalid size", request, []CheckpointLineageEvidence{invalidSize}, false, CheckpointLineageDecisionInvalid},
		{"invalid precedes revision", request, []CheckpointLineageEvidence{otherRevision, invalidSize}, false, CheckpointLineageDecisionInvalid},
		{"ownership conflict", request, []CheckpointLineageEvidence{exact, otherObject}, false, CheckpointLineageDecisionOwnershipConflict},
		{"revision precedes ownership", request, []CheckpointLineageEvidence{exact, otherObject, otherRevision}, false, CheckpointLineageDecisionRevisionConflict},
		{"cross-lineage ownership", request, []CheckpointLineageEvidence{exact}, true, CheckpointLineageDecisionOwnershipConflict},
		{"cross-lineage flag cannot occupy an absent slot", request, nil, true, CheckpointLineageDecisionAbsent},
		{"invalid request", CheckpointLineageRequest{}, []CheckpointLineageEvidence{exact}, false, CheckpointLineageDecisionInvalid},
		{"invalid evidence", request, []CheckpointLineageEvidence{{FileRevision: revisionA, ExactSize: 64}}, false, CheckpointLineageDecisionInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyCheckpointLineage(test.request, test.evidence, test.crossConflict); got != test.want {
				t.Fatalf("decision = %s, want %s", got, test.want)
			}
		})
	}

	for decision := CheckpointLineageDecisionAbsent; decision <= CheckpointLineageDecisionInvalid; decision++ {
		if !decision.Valid() || decision.String() == "" {
			t.Fatalf("decision %d is not a closed value", decision)
		}
	}
	if CheckpointLineageDecision(0).Valid() || CheckpointLineageDecision(99).Valid() || CheckpointLineageDecision(99).String() != "" {
		t.Fatal("lineage decision vocabulary is open")
	}
}

func lineageRawU64(value uint64) []byte {
	encoded := make([]byte, 8)
	binary.BigEndian.PutUint64(encoded, value)
	return encoded
}

func lineageFrame(value []byte) []byte {
	return append(lineageRawU64(uint64(len(value))), value...)
}
