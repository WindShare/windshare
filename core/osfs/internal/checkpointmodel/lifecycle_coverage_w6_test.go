package checkpointmodel

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestCheckpointAuthorityCodecsRejectEveryFreshProcessPrefix(t *testing.T) {
	record := mustCanonicalRecord(t, canonicalRecordSpec(t))
	recordImage, err := EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	for cut := range len(recordImage) {
		if _, err := DecodeRecord(recordImage[:cut]); err == nil {
			t.Fatalf("record prefix %d/%d decoded", cut, len(recordImage))
		}
	}
	recordPayload := record.CanonicalBytes()
	for cut := range len(recordPayload) {
		if _, err := DecodeRecord(recordEnvelope(recordPayload[:cut])); err == nil {
			t.Fatalf("authenticated record payload prefix %d/%d decoded", cut, len(recordPayload))
		}
	}

	ownership, err := NewOwnership(OwnershipSpec{
		Materializer: MaterializerNativeTree, Certification: CertificationWindowsNTFSProcessRestart,
		AuthorityRef:        bytes.Repeat([]byte{0xf2}, 32),
		RootOpenDisposition: CallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	ownershipImage, err := EncodeOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}
	for cut := range len(ownershipImage) {
		if _, err := DecodeOwnership(ownershipImage[:cut]); err == nil {
			t.Fatalf("ownership prefix %d/%d decoded", cut, len(ownershipImage))
		}
	}
	ownershipPayload := ownership.CanonicalBytes()
	for cut := range len(ownershipPayload) {
		prefix := bytes.Clone(ownershipPayload[:cut])
		checksum := ownershipChecksum(prefix)
		prefix = append(prefix, checksum[:]...)
		if _, err := DecodeOwnership(prefix); err == nil {
			t.Fatalf("authenticated ownership payload prefix %d/%d decoded", cut, len(ownershipPayload))
		}
	}
}

func TestRecordValidationRejectsInMemoryAuthorityTampering(t *testing.T) {
	canonical := mustCanonicalRecord(t, canonicalRecordSpec(t))
	mutations := map[string]func(*Record){
		"ownership marker": func(record *Record) { record.ownershipMarker = "foreign" },
		"namespace":        func(record *Record) { record.namespace = "foreign" },
		"identity":         func(record *Record) { record.stateGeneration = 0 },
		"canonical path":   func(record *Record) { record.canonicalPath = "folder/../file.bin" },
		"phase":            func(record *Record) { record.phase = 0 },
		"commit state":     func(record *Record) { record.commitState = 0 },
		"phase commit": func(record *Record) {
			record.phase, record.commitState = PhasePublished, CommitCandidate
		},
		"lifecycle claim": func(record *Record) {
			record.quarantineReason = QuarantineAnchorMissing
		},
		"verified ranges": func(record *Record) {
			record.verifiedRanges = []Range{{Offset: 16, End: 16}}
		},
		"record identity": func(record *Record) { record.recordID[0] ^= 0xff },
		"checksum":        func(record *Record) { record.checksum[0] ^= 0xff },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			tampered := canonical
			tampered.verifiedRanges = append([]Range(nil), canonical.verifiedRanges...)
			mutate(&tampered)
			if tampered.Valid() || tampered.CanonicalBytes() != nil {
				t.Fatal("tampered checkpoint remained canonical")
			}
			if _, err := EncodeRecord(tampered); err == nil {
				t.Fatal("tampered checkpoint acquired persisted authority")
			}
		})
	}
}

func TestQuarantineHistoryAllowsOnlyReachableRuntimeOrigins(t *testing.T) {
	tests := []struct {
		name   string
		origin QuarantineOrigin
		reason QuarantineReason
		want   bool
	}{
		{"anchor missing after witness", QuarantineOriginWitnessed, QuarantineAnchorMissing, true},
		{"anchor missing while retiring", QuarantineOriginRetiring, QuarantineAnchorMissing, true},
		{"anchor missing before witness", QuarantineOriginReserved, QuarantineAnchorMissing, false},
		{"stage missing while publishing", QuarantineOriginPublishing, QuarantineStageMissing, true},
		{"stage missing after publish", QuarantineOriginPublished, QuarantineStageMissing, false},
		{"stage missing while retiring", QuarantineOriginRetiring, QuarantineStageMissing, true},
		{"final mismatch after publish", QuarantineOriginPublished, QuarantineFinalMismatch, true},
		{"final mismatch before publish", QuarantineOriginPublishing, QuarantineFinalMismatch, false},
		{"final mismatch while retiring", QuarantineOriginRetiring, QuarantineFinalMismatch, true},
		{"final unsafe from active runtime", QuarantineOriginPublishBlocked, QuarantineFinalUnsafe, true},
		{"final unsafe while retiring", QuarantineOriginRetiring, QuarantineFinalUnsafe, true},
		{"partial object at reservation", QuarantineOriginReserved, QuarantinePartialObjectCreation, true},
		{"partial object while retiring", QuarantineOriginRetiring, QuarantinePartialObjectCreation, true},
		{"partial object after witness", QuarantineOriginWitnessed, QuarantinePartialObjectCreation, false},
		{"publication history while blocked", QuarantineOriginPublishBlocked, QuarantinePublicationHistory, true},
		{"publication history after publish", QuarantineOriginPublished, QuarantinePublicationHistory, false},
		{"metadata mismatch while publishing", QuarantineOriginPublishing, QuarantineMetadataMismatch, true},
		{"metadata mismatch at reservation", QuarantineOriginReserved, QuarantineMetadataMismatch, false},
		{"unsafe stage at any valid origin", QuarantineOriginRetiring, QuarantineStageUnsafe, true},
		{"unknown reason", QuarantineOriginReserved, 0, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validQuarantineHistory(test.origin, test.reason); got != test.want {
				t.Fatalf("history validity = %t, want %t", got, test.want)
			}
		})
	}

	spec := canonicalRecordSpec(t)
	spec.Phase, spec.CommitState = PhaseQuarantined, CommitQuarantined
	spec.QuarantineReason, spec.QuarantineOrigin = QuarantineMetadataMismatch, QuarantineOriginPublishing
	quarantined := mustCanonicalRecord(t, spec)
	if quarantined.FileID().IsZero() || quarantined.FileRevision().IsZero() ||
		quarantined.QuarantineReason() != QuarantineMetadataMismatch ||
		quarantined.QuarantineOrigin() != QuarantineOriginPublishing || quarantined.RetirementReason() != 0 {
		t.Fatal("quarantine authority projection lost immutable evidence")
	}
}

func TestCheckpointReducerRejectsEveryNonAdjacentAuthorityCut(t *testing.T) {
	candidate := mustCanonicalRecord(t, canonicalRecordSpec(t))
	committed, err := Promote(candidate, PhaseActive, CommitVerified)
	if err != nil {
		t.Fatal(err)
	}

	invalidPrevious := committed
	invalidPrevious.stateGeneration = 0
	if err := ValidateTransition(invalidPrevious, committed); err == nil {
		t.Fatal("invalid previous record entered the reducer")
	}
	invalidNext := committed
	invalidNext.checksum[0] ^= 0xff
	if err := ValidateTransition(committed, invalidNext); err == nil {
		t.Fatal("invalid next record entered the reducer")
	}

	pausedSpec := canonicalRecordSpec(t)
	pausedSpec.StateGeneration = committed.StateGeneration() + 1
	pausedSpec.Phase, pausedSpec.CommitState = PhasePaused, CommitVerified
	paused := mustCanonicalRecord(t, pausedSpec)
	if err := ValidateTransition(paused, committed); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("generation regression error = %v", err)
	}
	if err := ValidateTransition(candidate, paused); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("candidate phase substitution error = %v", err)
	}

	activeSuccessorSpec := canonicalRecordSpec(t)
	activeSuccessorSpec.StateGeneration = committed.StateGeneration() + 1
	activeSuccessorSpec.CommitState = CommitVerified
	activeSuccessor := mustCanonicalRecord(t, activeSuccessorSpec)
	if err := ValidateTransition(committed, activeSuccessor); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("unsupported same-phase transition error = %v", err)
	}

	gapSpec := canonicalRecordSpec(t)
	gapSpec.StateGeneration = committed.StateGeneration() + 1
	gapSpec.CheckpointGeneration = committed.CheckpointGeneration() + 2
	gap := mustCanonicalRecord(t, gapSpec)
	if err := ValidateTransition(committed, gap); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("checkpoint generation gap error = %v", err)
	}

	regressedSpec := pausedSpec
	regressedSpec.VerifiedRanges = []Range{{Offset: 0, End: 16}}
	regressed := mustCanonicalRecord(t, regressedSpec)
	if err := ValidateTransition(committed, regressed); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("verified range regression error = %v", err)
	}
}

func TestCheckpointReducerErrorPathsPreserveLastCommittedEvidence(t *testing.T) {
	candidate := mustCanonicalRecord(t, canonicalRecordSpec(t))
	committed, err := Promote(candidate, PhaseActive, CommitVerified)
	if err != nil {
		t.Fatal(err)
	}

	if ValidLifecycleTransition(PhasePublishing, CommitVerified, PhasePublished, CommitVerified) ||
		ValidLifecycleTransition(PhaseActive, CommitVerified, PhaseQuarantined, CommitVerified) ||
		ValidLifecycleTransition(PhaseActive, CommitVerified, PhaseActive, CommitVerified) ||
		ValidLifecycleTransition(PhasePublished, CommitPublished, PhaseRetired, CommitPublished) {
		t.Fatal("open lifecycle edge acquired mutation authority")
	}
	if rangesContain([]Range{{Offset: 32, End: 64}}, []Range{{Offset: 0, End: 16}}) {
		t.Fatal("later range was treated as containing earlier durable bytes")
	}
	if _, err := SelectVerified(checkpointmodelInvalidRecordForCoverage(committed)); err == nil {
		t.Fatal("invalid checkpoint was selected as verified")
	}
	if _, err := Recover(&Record{}, nil); err == nil {
		t.Fatal("invalid committed checkpoint recovered")
	}
	recovered, err := Recover(&committed, nil)
	if err != nil || !IdentityEqual(recovered, committed) {
		t.Fatalf("nil candidate recovery = (%+v, %v)", recovered, err)
	}

	if _, err := AdvanceGeneration(Record{}, nil, PhaseActive, CommitCandidate); err == nil {
		t.Fatal("invalid generation source advanced")
	}
	overflowSpec := canonicalRecordSpec(t)
	overflowSpec.StateGeneration, overflowSpec.CheckpointGeneration = math.MaxUint64, math.MaxUint64
	overflowSpec.CommitState = CommitVerified
	overflow := mustCanonicalRecord(t, overflowSpec)
	if _, err := AdvanceGeneration(overflow, overflow.VerifiedRanges(), PhaseActive, CommitCandidate); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("generation overflow error = %v", err)
	}
	if _, err := AdvanceGeneration(committed, []Range{{Offset: 16, End: 16}}, PhaseActive, CommitCandidate); err == nil {
		t.Fatal("invalid next-generation ranges were admitted")
	}
	if _, err := AdvanceGeneration(committed, []Range{{Offset: 0, End: 16}}, PhaseActive, CommitCandidate); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("regressed next-generation ranges error = %v", err)
	}

	if _, err := AdvanceState(committed, committed.StateGeneration()+1, 0, CommitVerified, 0, 0, 0); err == nil {
		t.Fatal("invalid lifecycle state was constructed")
	}
	if _, err := AdvanceState(
		committed, committed.StateGeneration()+1, PhaseActive, CommitVerified, 0, 0, 0,
	); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("unsupported state transition error = %v", err)
	}
	if _, err := Promote(Record{}, PhaseActive, CommitVerified); err == nil {
		t.Fatal("invalid candidate was promoted")
	}
	if _, err := Promote(candidate, PhaseQuarantined, CommitVerified); err == nil {
		t.Fatal("invalid promoted phase was constructed")
	}
	if _, err := Promote(candidate, PhasePaused, CommitVerified); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("candidate phase substitution promotion error = %v", err)
	}
}

func checkpointmodelInvalidRecordForCoverage(record Record) Record {
	record.checksum[0] ^= 0xff
	return record
}

func TestRangeCanonicalizationAndChecksumViewsRemainBounded(t *testing.T) {
	if got := (Range{Offset: 4, End: 4}).Length(); got != 0 {
		t.Fatalf("empty range length = %d", got)
	}
	if got := (Range{Offset: 4, End: 9}).Length(); got != 5 {
		t.Fatalf("range length = %d", got)
	}
	if err := validateRanges(make([]Range, maximumRanges+1), catalog.MaxFileSize); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("range-count bound error = %v", err)
	}

	for name, test := range map[string]struct {
		ranges []Range
		want   Range
	}{
		"shorter duplicate first": {[]Range{{Offset: 0, End: 2}, {Offset: 0, End: 3}}, Range{Offset: 0, End: 3}},
		"longer duplicate first":  {[]Range{{Offset: 0, End: 3}, {Offset: 0, End: 2}}, Range{Offset: 0, End: 3}},
		"exact duplicate":         {[]Range{{Offset: 0, End: 2}, {Offset: 0, End: 2}}, Range{Offset: 0, End: 2}},
	} {
		t.Run(name, func(t *testing.T) {
			canonical, err := CanonicalizeRanges(test.ranges)
			if err != nil || len(canonical) != 1 || canonical[0] != test.want {
				t.Fatalf("canonical ranges = (%+v, %v)", canonical, err)
			}
		})
	}

	record := mustCanonicalRecord(t, canonicalRecordSpec(t))
	checksum := record.Checksum().Bytes()
	checksum[0] ^= 0xff
	if bytes.Equal(checksum, record.Checksum().Bytes()) {
		t.Fatal("checksum byte view aliases persisted checkpoint authority")
	}
}
