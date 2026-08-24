package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestRecordConstructorOwnsIdentityRangesAndLifecycleClaims(t *testing.T) {
	spec := canonicalRecordSpec(t)
	record := mustCanonicalRecord(t, spec)
	if !record.Valid() || record.SchemaVersion() != SchemaVersion ||
		record.OwnershipMarker() != OwnershipMarker || record.Namespace() != NamespaceName ||
		record.CanonicalPath() != spec.CanonicalPath || record.ExactSize() != spec.ExactSize ||
		record.OperationID() != spec.OperationID || record.ReceiveIntentDigest() != spec.ReceiveIntentDigest ||
		record.MaterializationBindingDigest() != spec.MaterializationBindingDigest ||
		record.MaterializerKind() != spec.MaterializerKind ||
		record.StateGeneration() != spec.StateGeneration ||
		record.CheckpointGeneration() != spec.CheckpointGeneration ||
		record.Phase() != spec.Phase || record.CommitState() != spec.CommitState ||
		record.RecordID().IsZero() || record.AuthorityRef().IsZero() ||
		record.OwnedObjectID().IsZero() || record.Checksum().IsZero() {
		t.Fatal("record accessors changed the canonical state")
	}
	ranges := record.VerifiedRanges()
	ranges[0].End = 1
	if record.VerifiedRanges()[0].End != 16 {
		t.Fatal("verified ranges leaked mutable storage")
	}

	for name, mutate := range map[string]func(*RecordSpec){
		"marker":       func(value *RecordSpec) { value.OwnershipMarker = "foreign" },
		"namespace":    func(value *RecordSpec) { value.Namespace = "foreign" },
		"operation":    func(value *RecordSpec) { value.OperationID = receivecontract.OperationID{} },
		"intent":       func(value *RecordSpec) { value.ReceiveIntentDigest = transfer.ReceiveIntentDigest{} },
		"binding":      func(value *RecordSpec) { value.MaterializationBindingDigest = receivecontract.BindingDigest{} },
		"file":         func(value *RecordSpec) { value.FileID = catalog.FileID{} },
		"revision":     func(value *RecordSpec) { value.FileRevision = content.FileRevision{} },
		"path":         func(value *RecordSpec) { value.CanonicalPath = "folder/../file.bin" },
		"size":         func(value *RecordSpec) { value.ExactSize = catalog.MaxFileSize + 1 },
		"materializer": func(value *RecordSpec) { value.MaterializerKind = 0 },
		"authority":    func(value *RecordSpec) { value.AuthorityRef = []byte{1} },
		"object":       func(value *RecordSpec) { value.OwnedObjectID = []byte{1} },
		"state":        func(value *RecordSpec) { value.StateGeneration = 0 },
		"phase":        func(value *RecordSpec) { value.Phase = Phase(99) },
		"commit":       func(value *RecordSpec) { value.CommitState = CommitState(99) },
		"range-order":  func(value *RecordSpec) { value.VerifiedRanges = []Range{{Offset: 16, End: 32}, {Offset: 8, End: 12}} },
		"range-adjacent": func(value *RecordSpec) {
			value.VerifiedRanges = []Range{{Offset: 0, End: 16}, {Offset: 16, End: 32}}
		},
		"range-outside": func(value *RecordSpec) { value.VerifiedRanges = []Range{{Offset: 0, End: 65}} },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := spec
			mutate(&invalid)
			if _, err := NewRecord(invalid); err == nil {
				t.Fatal("invalid record was accepted")
			}
		})
	}
	invalidMarker := spec
	invalidMarker.OwnershipMarker = "foreign"
	invalidNamespace := spec
	invalidNamespace.Namespace = "foreign"
	for _, invalid := range []RecordSpec{invalidMarker, invalidNamespace} {
		if _, err := NewRecord(invalid); !errors.Is(err, ErrInvalidRecord) ||
			errors.Is(err, ErrInvalidOwnership) {
			t.Fatalf("record namespace error crossed into root ownership: %v", err)
		}
	}

	canonical, err := CanonicalizeRanges([]Range{
		{Offset: 8, End: 12},
		{Offset: 0, End: 8},
		{Offset: 11, End: 16},
	})
	if err != nil || len(canonical) != 1 || canonical[0] != (Range{Offset: 0, End: 16}) {
		t.Fatalf("canonical ranges = %+v, %v", canonical, err)
	}
	if _, err := CanonicalizeRanges([]Range{{Offset: 2, End: 2}}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("empty range error = %v", err)
	}

	for name, parse := range map[string]func([]byte) error{
		"record": func(raw []byte) error { _, err := RecordIDFromBytes(raw); return err },
		"object": func(raw []byte) error { _, err := ObjectIDFromBytes(raw); return err },
		"sum":    func(raw []byte) error { _, err := ChecksumFromBytes(raw); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := parse([]byte{1}); !errors.Is(err, ErrRecordBinding) {
				t.Fatalf("short identity error = %v", err)
			}
			if err := parse(make([]byte, sha256.Size)); !errors.Is(err, ErrRecordBinding) {
				t.Fatalf("zero identity error = %v", err)
			}
		})
	}
}

func TestCurrentFSAMaterializerOwnsEmptyRootRelativeCoordinate(t *testing.T) {
	spec := canonicalRecordSpec(t)
	spec.MaterializerKind = MaterializerFSATree
	spec.CanonicalPath = ""

	record, err := NewRecord(spec)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeRecord(encoded)
	if err != nil || restored.CanonicalPath() != "" {
		t.Fatalf("root-relative checkpoint round trip = %q, %v", restored.CanonicalPath(), err)
	}
	lineage, err := record.CheckpointLineageSpec()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DeriveCheckpointLineageID(lineage); err != nil {
		t.Fatalf("root-relative lineage = %v", err)
	}

	legacy := spec
	legacy.MaterializerKind = MaterializerLegacyFSATree
	if _, err := NewRecord(legacy); !errors.Is(err, ErrRecordBinding) {
		t.Fatalf("legacy empty path error = %v", err)
	}
	legacyLineage := lineage
	legacyLineage.MaterializerKind = MaterializerLegacyFSATree
	if _, err := DeriveCheckpointLineageID(legacyLineage); !errors.Is(err, ErrRecordBinding) {
		t.Fatalf("legacy empty lineage error = %v", err)
	}
}

func TestRecordLifecycleClaimsAreClosed(t *testing.T) {
	valid := []RecordSpec{
		func() RecordSpec {
			spec := canonicalRecordSpec(t)
			spec.Phase = PhaseQuarantined
			spec.CommitState = CommitQuarantined
			spec.QuarantineReason = QuarantineAnchorMissing
			spec.QuarantineOrigin = QuarantineOriginWitnessed
			return spec
		}(),
		func() RecordSpec {
			spec := canonicalRecordSpec(t)
			spec.Phase = PhaseQuarantined
			spec.CommitState = CommitQuarantined
			spec.QuarantineReason = QuarantinePartialObjectCreation
			spec.QuarantineOrigin = QuarantineOriginRetiring
			spec.RetirementReason = RetirementPublished
			return spec
		}(),
		func() RecordSpec {
			spec := canonicalRecordSpec(t)
			spec.Phase = PhaseRetired
			spec.CommitState = CommitVerified
			spec.RetirementReason = RetirementPublished
			return spec
		}(),
	}
	for _, spec := range valid {
		if _, err := NewRecord(spec); err != nil {
			t.Fatalf("valid lifecycle claim = %v", err)
		}
	}

	for name, mutate := range map[string]func(*RecordSpec){
		"published-as-verified": func(spec *RecordSpec) {
			spec.Phase = PhasePublished
			spec.CommitState = CommitVerified
		},
		"active-as-published": func(spec *RecordSpec) {
			spec.CommitState = CommitPublished
		},
		"quarantined-as-candidate": func(spec *RecordSpec) {
			spec.Phase = PhaseQuarantined
			spec.QuarantineReason = QuarantineAnchorMissing
			spec.QuarantineOrigin = QuarantineOriginWitnessed
		},
		"retired-as-candidate": func(spec *RecordSpec) {
			spec.Phase = PhaseRetired
			spec.RetirementReason = RetirementPublished
		},
		"missing-quarantine": func(spec *RecordSpec) {
			spec.Phase = PhaseQuarantined
			spec.CommitState = CommitQuarantined
		},
		"invalid-history": func(spec *RecordSpec) {
			spec.Phase = PhaseQuarantined
			spec.CommitState = CommitQuarantined
			spec.QuarantineReason = QuarantineFinalMismatch
			spec.QuarantineOrigin = QuarantineOriginWitnessed
		},
		"quarantine-outside": func(spec *RecordSpec) {
			spec.QuarantineReason = QuarantineAnchorUnsafe
			spec.QuarantineOrigin = QuarantineOriginWitnessed
		},
		"retired-without-reason": func(spec *RecordSpec) {
			spec.Phase = PhaseRetired
			spec.CommitState = CommitVerified
		},
		"retirement-outside": func(spec *RecordSpec) {
			spec.RetirementReason = RetirementPublished
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := canonicalRecordSpec(t)
			mutate(&spec)
			if _, err := NewRecord(spec); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("lifecycle error = %v", err)
			}
		})
	}

	for _, reason := range []QuarantineReason{
		QuarantineAnchorMissing,
		QuarantineAnchorUnsafe,
		QuarantineStageMissing,
		QuarantineStageMismatch,
		QuarantineStageUnsafe,
		QuarantineFinalMismatch,
		QuarantineFinalUnsafe,
		QuarantinePartialObjectCreation,
		QuarantinePublicationHistory,
		QuarantineMetadataMismatch,
		QuarantineUpdateTemporary,
		QuarantineOutputObjectDuplicate,
	} {
		if !reason.Valid() {
			t.Fatalf("reason %d is invalid", reason)
		}
	}
	if QuarantineReason(0).Valid() || QuarantineReason(99).Valid() ||
		QuarantineOrigin(0).Valid() || QuarantineOrigin(99).Valid() ||
		RetirementReason(0).Valid() || RetirementReason(99).Valid() {
		t.Fatal("open lifecycle value set")
	}
}

func TestRecordCodecRejectsEveryUnauthenticatedCut(t *testing.T) {
	record := mustCanonicalRecord(t, canonicalRecordSpec(t))
	encoded, err := EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeRecord(encoded)
	if err != nil || !bytes.Equal(restored.CanonicalBytes(), record.CanonicalBytes()) {
		t.Fatalf("round trip = %v", err)
	}
	if _, err := EncodeRecord(Record{}); err == nil {
		t.Fatal("zero record encoded")
	}

	cases := map[string]struct {
		value []byte
		want  error
	}{
		"empty":     {nil, ErrInvalidRecord},
		"bad-magic": {append([]byte{0}, encoded[1:]...), ErrInvalidRecord},
		"truncated": {encoded[:len(recordMagic)+4], ErrInvalidRecord},
		"bad-checksum": {func() []byte {
			value := append([]byte(nil), encoded...)
			value[len(value)-1] ^= 1
			return value
		}(), ErrRecordChecksum},
		"bad-length": {func() []byte {
			value := append([]byte(nil), encoded...)
			value[len(recordMagic)+3] ^= 1
			return value
		}(), ErrInvalidRecord},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRecord(test.value); !errors.Is(err, test.want) {
				t.Fatalf("decode error = %v, want %v", err, test.want)
			}
		})
	}

	payload := record.CanonicalBytes()
	badDomain := append([]byte(nil), payload...)
	badDomain[0] ^= 1
	if _, err := DecodeRecord(recordEnvelope(badDomain)); !errors.Is(err, ErrRecordNonCanonical) {
		t.Fatalf("domain error = %v", err)
	}
	badVersion := append([]byte(nil), payload...)
	badVersion[len(recordDomain)+1] = 99
	if _, err := DecodeRecord(recordEnvelope(badVersion)); !errors.Is(err, ErrRecordNonCanonical) {
		t.Fatalf("version error = %v", err)
	}
	trailing := append(append([]byte(nil), payload...), 0)
	if _, err := DecodeRecord(recordEnvelope(trailing)); !errors.Is(err, ErrRecordNonCanonical) {
		t.Fatalf("trailing payload error = %v", err)
	}

	cursor := recordCursor{}
	if _, err := cursor.take(-1); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("negative take = %v", err)
	}
	if _, err := cursor.framedByte(); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("empty byte = %v", err)
	}
	if _, err := cursor.rawU64(); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("empty u64 = %v", err)
	}
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], 2)
	oversized := recordCursor{bytes: append(length[:], 'a', 'b')}
	if _, err := oversized.text(1); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("oversized string = %v", err)
	}
	binary.BigEndian.PutUint64(length[:], 1)
	invalidUTF8 := recordCursor{bytes: append(length[:], 0xff)}
	if _, err := invalidUTF8.text(1); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("invalid UTF-8 = %v", err)
	}
	rangeCount := make([]byte, 8)
	binary.BigEndian.PutUint64(rangeCount, maximumRanges+1)
	if _, err := decodeRecordRanges(&recordCursor{bytes: rangeCount}); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("range count error = %v", err)
	}
}

func TestRecordReducersOwnTransitionsSelectionAndCrashCuts(t *testing.T) {
	candidateSpec := canonicalRecordSpec(t)
	candidate := mustCanonicalRecord(t, candidateSpec)
	verified, err := Promote(candidate, PhaseActive, CommitVerified)
	if err != nil {
		t.Fatal(err)
	}
	if !Committed(verified) || InitialCandidate(verified) {
		t.Fatal("candidate promotion did not commit")
	}
	if _, err := Promote(verified, PhaseActive, CommitPublished); !errors.Is(err, ErrRecordCrashBoundary) {
		t.Fatalf("repeated promotion = %v", err)
	}
	if _, err := Promote(candidate, PhaseActive, CommitCandidate); !errors.Is(err, ErrRecordCrashBoundary) {
		t.Fatalf("candidate promotion target = %v", err)
	}

	nextCandidate, err := AdvanceGeneration(
		verified,
		[]Range{{Offset: 0, End: 16}, {Offset: 24, End: 64}},
		PhaseActive,
		CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	nextVerified, err := Promote(nextCandidate, PhaseActive, CommitVerified)
	if err != nil {
		t.Fatal(err)
	}
	for _, transition := range [][2]Record{
		{candidate, verified},
		{verified, nextCandidate},
		{nextCandidate, nextVerified},
	} {
		if err := ValidateTransition(transition[0], transition[1]); err != nil {
			t.Fatalf("valid transition = %v", err)
		}
	}
	paused, err := AdvanceState(
		nextVerified,
		nextVerified.StateGeneration()+1,
		PhasePaused,
		CommitVerified,
		0,
		0,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if paused.CheckpointGeneration() != nextVerified.CheckpointGeneration() ||
		!slicesEqualRanges(paused.VerifiedRanges(), nextVerified.VerifiedRanges()) {
		t.Fatal("lifecycle transition changed durable ranges")
	}
	if _, err := AdvanceState(nextVerified, nextVerified.StateGeneration(), PhasePaused, CommitVerified, 0, 0, 0); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("non-advancing state = %v", err)
	}

	foreignSpec := candidateSpec
	foreignSpec.AuthorityRef = bytes.Repeat([]byte{0x72}, sha256.Size)
	foreignSpec.CommitState = CommitVerified
	foreign := mustCanonicalRecord(t, foreignSpec)
	if IdentityEqual(candidate, foreign) || !errors.Is(ValidateTransition(candidate, foreign), ErrRecordBinding) {
		t.Fatal("foreign identity passed transition validation")
	}
	if !errors.Is(ValidateTransition(verified, candidate), ErrRecordGeneration) {
		t.Fatal("generation regression passed")
	}
	regressedSpec := candidateSpec
	regressedSpec.StateGeneration = 2
	regressedSpec.CheckpointGeneration = 2
	regressedSpec.CommitState = CommitVerified
	regressedSpec.VerifiedRanges = []Range{{Offset: 0, End: 8}, {Offset: 32, End: 64}}
	regressed := mustCanonicalRecord(t, regressedSpec)
	if !errors.Is(ValidateTransition(verified, regressed), ErrRecordGeneration) {
		t.Fatal("verified ranges regressed")
	}
	committedGenerationSpec := candidateSpec
	committedGenerationSpec.StateGeneration = verified.StateGeneration() + 1
	committedGenerationSpec.CheckpointGeneration = verified.CheckpointGeneration() + 1
	committedGenerationSpec.CommitState = CommitVerified
	committedGenerationSpec.VerifiedRanges = []Range{{Offset: 0, End: 16}, {Offset: 24, End: 64}}
	committedGeneration := mustCanonicalRecord(t, committedGenerationSpec)
	if !errors.Is(ValidateTransition(verified, committedGeneration), ErrRecordGeneration) {
		t.Fatal("new checkpoint generation bypassed the candidate cut")
	}
	phaseChangingCandidateSpec := committedGenerationSpec
	phaseChangingCandidateSpec.CommitState = CommitCandidate
	phaseChangingCandidateSpec.Phase = PhasePaused
	phaseChangingCandidate := mustCanonicalRecord(t, phaseChangingCandidateSpec)
	if !errors.Is(ValidateTransition(verified, phaseChangingCandidate), ErrRecordGeneration) {
		t.Fatal("new checkpoint generation changed lifecycle phase")
	}

	selected, err := SelectVerified(candidate, verified, nextCandidate, nextVerified)
	if err != nil || selected.CheckpointGeneration() != 2 {
		t.Fatalf("selected generation = %d, err = %v", selected.CheckpointGeneration(), err)
	}
	if _, err := SelectVerified(candidate, nextCandidate); !errors.Is(err, ErrRecordRecovery) {
		t.Fatalf("candidate-only selection = %v", err)
	}
	if _, err := SelectVerified(verified, foreign); !errors.Is(err, ErrRecordBinding) {
		t.Fatalf("foreign committed selection = %v", err)
	}
	conflictSpec := candidateSpec
	conflictSpec.CommitState = CommitVerified
	conflictSpec.Phase = PhasePaused
	conflict := mustCanonicalRecord(t, conflictSpec)
	if _, err := SelectVerified(verified, conflict); !errors.Is(err, ErrRecordCrashBoundary) {
		t.Fatalf("same-generation conflict = %v", err)
	}

	beforeCommit, err := Recover(&verified, &nextCandidate)
	if err != nil || beforeCommit.CheckpointGeneration() != 1 {
		t.Fatalf("before-commit recovery = %d, %v", beforeCommit.CheckpointGeneration(), err)
	}
	afterCommit, err := Recover(&verified, &nextVerified)
	if err != nil || afterCommit.CheckpointGeneration() != 2 {
		t.Fatalf("after-commit recovery = %d, %v", afterCommit.CheckpointGeneration(), err)
	}
	if got, err := Recover(&verified, &foreign); err != nil || got.RecordID() != verified.RecordID() {
		t.Fatalf("foreign candidate recovery = %v, %v", got.RecordID(), err)
	}
	if got, err := Recover(&nextVerified, &verified); err != nil || got.CheckpointGeneration() != 2 {
		t.Fatalf("older candidate recovery = %d, %v", got.CheckpointGeneration(), err)
	}
	if got, err := Recover(&verified, &conflict); err != nil || got.RecordID() != verified.RecordID() {
		t.Fatalf("ambiguous candidate recovery = %v, %v", got.RecordID(), err)
	}
	if _, err := Recover(nil, &candidate); !errors.Is(err, ErrRecordRecovery) {
		t.Fatalf("nil committed recovery = %v", err)
	}

	maxed := verified
	maxed.stateGeneration = ^uint64(0)
	maxed.checkpointGeneration = ^uint64(0)
	maxed.checksum = Checksum{}
	if _, err := AdvanceGeneration(maxed, nil, PhaseActive, CommitCandidate); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("overflow advance = %v", err)
	}
}

func TestLifecycleTransitionVocabularyIsClosed(t *testing.T) {
	valid := [][4]uint8{
		{uint8(PhaseReserved), uint8(CommitVerified), uint8(PhaseActive), uint8(CommitVerified)},
		{uint8(PhaseActive), uint8(CommitVerified), uint8(PhasePaused), uint8(CommitVerified)},
		{uint8(PhasePaused), uint8(CommitVerified), uint8(PhasePublishing), uint8(CommitVerified)},
		{uint8(PhasePublishing), uint8(CommitVerified), uint8(PhasePublished), uint8(CommitPublished)},
		{uint8(PhasePublished), uint8(CommitPublished), uint8(PhaseQuarantined), uint8(CommitQuarantined)},
		{uint8(PhaseRetired), uint8(CommitVerified), uint8(PhaseQuarantined), uint8(CommitQuarantined)},
	}
	for _, transition := range valid {
		if !ValidLifecycleTransition(
			Phase(transition[0]),
			CommitState(transition[1]),
			Phase(transition[2]),
			CommitState(transition[3]),
		) {
			t.Fatalf("valid transition rejected: %+v", transition)
		}
	}
	invalid := [][4]uint8{
		{uint8(PhaseActive), uint8(CommitVerified), uint8(PhaseReserved), uint8(CommitVerified)},
		{uint8(PhaseActive), uint8(CommitPublished), uint8(PhasePaused), uint8(CommitVerified)},
		{uint8(PhaseActive), uint8(CommitVerified), uint8(PhasePublished), uint8(CommitVerified)},
		{uint8(PhaseActive), uint8(CommitVerified), uint8(PhasePaused), uint8(CommitCandidate)},
	}
	for _, transition := range invalid {
		if ValidLifecycleTransition(
			Phase(transition[0]),
			CommitState(transition[1]),
			Phase(transition[2]),
			CommitState(transition[3]),
		) {
			t.Fatalf("invalid transition accepted: %+v", transition)
		}
	}
	if Phase(0).Valid() || Phase(99).Valid() || CommitState(0).Valid() || CommitState(99).Valid() {
		t.Fatal("phase or commit vocabulary is open")
	}
}

func canonicalRecordSpec(t *testing.T) RecordSpec {
	t.Helper()
	var fileID catalog.FileID
	var revision content.FileRevision
	for index := range fileID {
		fileID[index] = byte(index + 1)
		revision[index] = byte(index + 2)
	}
	operation, err := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{0x21}, receivecontract.StableIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.ReceiveIntentDigestFromBytes(bytes.Repeat([]byte{0x31}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	binding, err := receivecontract.BindingDigestFromBytes(bytes.Repeat([]byte{0x39}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	return RecordSpec{
		OperationID: operation, ReceiveIntentDigest: intent,
		MaterializationBindingDigest: binding,
		FileID:                       fileID, FileRevision: revision, CanonicalPath: "folder/file.bin",
		ExactSize: 64, MaterializerKind: MaterializerNativeTree,
		AuthorityRef:    bytes.Repeat([]byte{0x41}, sha256.Size),
		OwnedObjectID:   bytes.Repeat([]byte{0x51}, sha256.Size),
		StateGeneration: 1, CheckpointGeneration: 1,
		VerifiedRanges: []Range{{Offset: 0, End: 16}, {Offset: 32, End: 64}},
		Phase:          PhaseActive, CommitState: CommitCandidate,
	}
}

func mustCanonicalRecord(t *testing.T, spec RecordSpec) Record {
	t.Helper()
	record, err := NewRecord(spec)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func recordEnvelope(payload []byte) []byte {
	encoded := make([]byte, 0, len(recordMagic)+4+len(payload)+sha256.Size)
	encoded = append(encoded, recordMagic...)
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
	encoded = append(encoded, length[:]...)
	encoded = append(encoded, payload...)
	hash := sha256.New()
	_, _ = hash.Write([]byte(recordChecksumDomain))
	_, _ = hash.Write([]byte{0, SchemaVersion})
	writeRecordFrame(hash, payload)
	return append(encoded, hash.Sum(nil)...)
}

func slicesEqualRanges(left, right []Range) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
