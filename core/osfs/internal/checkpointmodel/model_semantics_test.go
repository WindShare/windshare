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
)

func TestRecordConstructorOwnsIdentityRangesAndLifecycleClaims(t *testing.T) {
	spec := canonicalRecordSpec(t)
	record := mustCanonicalRecord(t, spec)
	if !record.Valid() || record.SchemaVersion() != SchemaVersion ||
		record.OwnershipMarker() != OwnershipMarker || record.Namespace() != NamespaceName ||
		record.CanonicalPath() != spec.CanonicalPath || record.ExactSize() != spec.ExactSize ||
		record.BackendID() != transfer.OutputBackendID(spec.BackendID) ||
		record.StateGeneration() != spec.StateGeneration ||
		record.CheckpointGeneration() != spec.CheckpointGeneration ||
		record.Phase() != spec.Phase || record.CommitState() != spec.CommitState ||
		record.RecordID().IsZero() || record.RootIdentity().IsZero() ||
		record.OwnedOutputObject().IsZero() || record.Checksum().IsZero() {
		t.Fatal("record accessors changed the canonical state")
	}
	ranges := record.VerifiedRanges()
	ranges[0].End = 1
	if record.VerifiedRanges()[0].End != 16 {
		t.Fatal("verified ranges leaked mutable storage")
	}

	for name, mutate := range map[string]func(*RecordSpec){
		"marker":      func(value *RecordSpec) { value.OwnershipMarker = "foreign" },
		"namespace":   func(value *RecordSpec) { value.Namespace = "foreign" },
		"intent":      func(value *RecordSpec) { value.TransferIntentDigest = transfer.TransferIntentDigest{} },
		"file":        func(value *RecordSpec) { value.FileID = catalog.FileID{} },
		"revision":    func(value *RecordSpec) { value.FileRevision = content.FileRevision{} },
		"path":        func(value *RecordSpec) { value.CanonicalPath = "folder/../file.bin" },
		"size":        func(value *RecordSpec) { value.ExactSize = catalog.MaxFileSize + 1 },
		"backend":     func(value *RecordSpec) { value.BackendID = "" },
		"root":        func(value *RecordSpec) { value.RootIdentity = []byte{1} },
		"object":      func(value *RecordSpec) { value.OwnedOutputObject = []byte{1} },
		"state":       func(value *RecordSpec) { value.StateGeneration = 0 },
		"phase":       func(value *RecordSpec) { value.Phase = Phase(99) },
		"commit":      func(value *RecordSpec) { value.CommitState = CommitState(99) },
		"range-order": func(value *RecordSpec) { value.VerifiedRanges = []Range{{Offset: 16, End: 32}, {Offset: 8, End: 12}} },
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
		"root":   func(raw []byte) error { _, err := RootIdentityFromBytes(raw); return err },
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
		"empty":     {value: nil, want: ErrInvalidRecord},
		"bad-magic": {value: append([]byte{0}, encoded[1:]...), want: ErrInvalidRecord},
		"truncated": {value: encoded[:len(recordMagic)+4], want: ErrInvalidRecord},
		"bad-checksum": {value: func() []byte {
			value := append([]byte(nil), encoded...)
			value[len(value)-1] ^= 1
			return value
		}(), want: ErrRecordChecksum},
		"bad-length": {value: func() []byte {
			value := append([]byte(nil), encoded...)
			value[len(recordMagic)+3] ^= 1
			return value
		}(), want: ErrInvalidRecord},
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
	badDomain[4] ^= 1
	if _, err := DecodeRecord(recordEnvelope(badDomain)); !errors.Is(err, ErrRecordNonCanonical) {
		t.Fatalf("domain error = %v", err)
	}
	badVersion := append([]byte(nil), payload...)
	badVersion[4+len(recordDomain)] = 99
	if _, err := DecodeRecord(recordEnvelope(badVersion)); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("version error = %v", err)
	}
	trailing := append(append([]byte(nil), payload...), 0)
	if _, err := DecodeRecord(recordEnvelope(trailing)); !errors.Is(err, ErrRecordNonCanonical) {
		t.Fatalf("trailing payload error = %v", err)
	}

	identityOffset := 4 + len(recordDomain) + 1 +
		4 + len(OwnershipMarker) + 4 + len(NamespaceName)
	for name, offset := range map[string]int{
		"record":   identityOffset,
		"intent":   identityOffset + sha256.Size,
		"file":     identityOffset + 2*sha256.Size,
		"revision": identityOffset + 2*sha256.Size + catalog.IdentityBytes,
	} {
		t.Run(name, func(t *testing.T) {
			invalid := append([]byte(nil), payload...)
			width := sha256.Size
			if name == "file" || name == "revision" {
				width = catalog.IdentityBytes
			}
			clear(invalid[offset : offset+width])
			if _, err := DecodeRecord(recordEnvelope(invalid)); !errors.Is(err, ErrRecordBinding) {
				t.Fatalf("identity error = %v", err)
			}
		})
	}

	cursor := recordCursor{}
	if _, err := cursor.take(-1); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("negative take = %v", err)
	}
	if _, err := cursor.byte(); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("empty byte = %v", err)
	}
	if _, err := cursor.u32(); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("empty u32 = %v", err)
	}
	if _, err := cursor.u64(); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("empty u64 = %v", err)
	}
	oversized := recordCursor{bytes: []byte{0, 0, 1, 0}}
	if _, err := oversized.string(1); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("oversized string = %v", err)
	}
	invalidUTF8 := recordCursor{bytes: []byte{0, 0, 0, 1, 0xff}}
	if _, err := invalidUTF8.string(1); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("invalid UTF-8 = %v", err)
	}
	rangeCount := make([]byte, 4)
	binary.BigEndian.PutUint32(rangeCount, maximumRanges+1)
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
	foreignSpec.RootIdentity = bytes.Repeat([]byte{0x72}, sha256.Size)
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
	intent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0x31}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	return RecordSpec{
		TransferIntentDigest: intent,
		FileID:               fileID,
		FileRevision:         revision,
		CanonicalPath:        "folder/file.bin",
		ExactSize:            64,
		BackendID:            "checkpointmodel-test",
		RootIdentity:         bytes.Repeat([]byte{0x41}, sha256.Size),
		OwnedOutputObject:    bytes.Repeat([]byte{0x51}, sha256.Size),
		StateGeneration:      1,
		CheckpointGeneration: 1,
		VerifiedRanges:       []Range{{Offset: 0, End: 16}, {Offset: 32, End: 64}},
		Phase:                PhaseActive,
		CommitState:          CommitCandidate,
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
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(payload)
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
