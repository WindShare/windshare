package resumestate

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestFileCheckpointV1Wave2IdentityAndRecoveryBranches(t *testing.T) {
	candidate := checkpointV1Fixture(t, 1, FileCheckpointCommitCandidate)
	verified, err := PromoteCheckpoint(candidate, FileCheckpointPhaseActive, FileCheckpointCommitVerified)
	if err != nil {
		t.Fatal(err)
	}
	next, err := CheckpointGenerationAdvance(verified, []FileCheckpointRange{{Offset: 0, End: 64}}, FileCheckpointPhasePaused, FileCheckpointCommitCandidate)
	if err != nil {
		t.Fatal(err)
	}
	nextPublished, err := PromoteCheckpoint(next, FileCheckpointPhasePaused, FileCheckpointCommitPublished)
	if err != nil {
		t.Fatal(err)
	}

	// Recovery keeps the last committed record for every ambiguous or uncommitted
	// candidate; only a matching committed record at a newer generation wins.
	if got, err := RecoverFileCheckpoint(&verified, nil); err != nil || got.RecordID() != verified.RecordID() {
		t.Fatalf("nil candidate recovery = %+v, %v", got, err)
	}
	if got, err := RecoverFileCheckpoint(&verified, &candidate); err != nil || got.RecordID() != verified.RecordID() {
		t.Fatalf("candidate recovery = %+v, %v", got, err)
	}
	if got, err := RecoverFileCheckpoint(&verified, &nextPublished); err != nil || got.CheckpointGeneration() != 2 {
		t.Fatalf("new committed recovery = %+v, %v", got, err)
	}
	older := checkpointV1Fixture(t, 1, FileCheckpointCommitVerified)
	if got, err := RecoverFileCheckpoint(&nextPublished, &older); err != nil || got.RecordID() != nextPublished.RecordID() {
		t.Fatalf("older candidate displaced committed record: %+v, %v", got, err)
	}
	identityMismatch := checkpointV1Fixture(t, 2, FileCheckpointCommitVerified)
	identityMismatch.canonicalPath = "other.bin"
	identityMismatch.recordID = identityMismatch.derivedRecordID()
	identityMismatch.checksum = identityMismatch.derivedChecksum()
	if got, err := RecoverFileCheckpoint(&verified, &identityMismatch); err != nil || got.RecordID() != verified.RecordID() {
		t.Fatalf("identity mismatch displaced committed record: %+v, %v", got, err)
	}
	sameGenerationConflict := verified
	sameGenerationConflict.phase = FileCheckpointPhasePaused
	sameGenerationConflict.checksum = sameGenerationConflict.derivedChecksum()
	if got, err := RecoverFileCheckpoint(&verified, &sameGenerationConflict); err != nil || got.RecordID() != verified.RecordID() {
		t.Fatalf("same-generation conflict displaced committed record: %+v, %v", got, err)
	}
	if _, err := RecoverFileCheckpoint(nil, &candidate); !errors.Is(err, ErrFileCheckpointRecovery) {
		t.Fatalf("nil committed error = %v", err)
	}

	if got, err := SelectVerifiedCheckpoint(candidate, verified, nextPublished); err != nil || got.CheckpointGeneration() != 2 {
		t.Fatalf("verified selection = %+v, %v", got, err)
	}
	if _, err := SelectVerifiedCheckpoint(candidate); !errors.Is(err, ErrFileCheckpointRecovery) {
		t.Fatalf("candidate-only selection = %v", err)
	}
	conflicting := nextPublished
	conflicting.phase = FileCheckpointPhasePublished
	conflicting.checksum = conflicting.derivedChecksum()
	if _, err := SelectVerifiedCheckpoint(nextPublished, conflicting); !errors.Is(err, ErrFileCheckpointCrashBoundary) {
		t.Fatalf("same-generation conflict = %v", err)
	}
	if _, err := SelectVerifiedCheckpoint(verified, identityMismatch); !errors.Is(err, ErrFileCheckpointBinding) {
		t.Fatalf("identity mismatch selection = %v", err)
	}
	invalid := verified
	invalid.recordID = FileCheckpointRecordID{}
	if _, err := SelectVerifiedCheckpoint(invalid); !errors.Is(err, ErrFileCheckpointBinding) {
		t.Fatalf("invalid record selection = %v", err)
	}
}

func TestFileCheckpointV1Wave2TransitionGuards(t *testing.T) {
	candidate := checkpointV1Fixture(t, 1, FileCheckpointCommitCandidate)
	verified, err := PromoteCheckpoint(candidate, FileCheckpointPhaseActive, FileCheckpointCommitVerified)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PromoteCheckpoint(candidate, FileCheckpointPhaseActive, FileCheckpointCommitCandidate); !errors.Is(err, ErrFileCheckpointCrashBoundary) {
		t.Fatalf("candidate-to-candidate promotion = %v", err)
	}
	if _, err := PromoteCheckpoint(verified, FileCheckpointPhaseActive, FileCheckpointCommitPublished); !errors.Is(err, ErrFileCheckpointCrashBoundary) {
		t.Fatalf("published promotion from verified = %v", err)
	}
	if _, err := PromoteCheckpoint(candidate, FileCheckpointPhase(99), FileCheckpointCommitVerified); !errors.Is(err, ErrInvalidFileCheckpoint) {
		t.Fatalf("invalid phase promotion = %v", err)
	}

	identityMismatch := verified
	identityMismatch.canonicalPath = "other.bin"
	identityMismatch.recordID = identityMismatch.derivedRecordID()
	identityMismatch.checksum = identityMismatch.derivedChecksum()
	if err := ValidateCheckpointTransition(verified, identityMismatch); !errors.Is(err, ErrFileCheckpointBinding) {
		t.Fatalf("identity mismatch transition = %v", err)
	}
	regressed := checkpointV1Fixture(t, 2, FileCheckpointCommitCandidate)
	if err := ValidateCheckpointTransition(regressed, candidate); !errors.Is(err, ErrFileCheckpointGeneration) {
		t.Fatalf("generation regression = %v", err)
	}
	// A same-generation candidate may only cross the atomic commit cut with the
	// same ranges and state generation.
	publishingCandidate := candidate
	publishingCandidate.phase = FileCheckpointPhasePublishing
	publishingCandidate.checksum = publishingCandidate.derivedChecksum()
	if promoted, err := PromoteCheckpoint(publishingCandidate, FileCheckpointPhasePublished, FileCheckpointCommitPublished); err != nil {
		t.Fatalf("publishing promotion = %v", err)
	} else if err := ValidateCheckpointTransition(publishingCandidate, promoted); err != nil {
		t.Fatalf("same-generation commit cut = %v", err)
	}
	sameGenerationMutation := candidate
	sameGenerationMutation.phase = FileCheckpointPhasePaused
	sameGenerationMutation.verifiedRanges = []FileCheckpointRange{{Offset: 0, End: 8}}
	sameGenerationMutation.checksum = sameGenerationMutation.derivedChecksum()
	if err := ValidateCheckpointTransition(candidate, sameGenerationMutation); !errors.Is(err, ErrFileCheckpointGeneration) {
		t.Fatalf("same-generation mutation = %v", err)
	}
	publishedNext := verified
	publishedNext.stateGeneration++
	publishedNext.checkpointGeneration++
	publishedNext.checksum = publishedNext.derivedChecksum()
	if err := ValidateCheckpointTransition(verified, publishedNext); err != nil {
		t.Fatalf("verified generation advance = %v", err)
	}
	if err := ValidateCheckpointTransition(publishedNext, publishedNext); !errors.Is(err, ErrFileCheckpointGeneration) {
		t.Fatalf("published immutable transition = %v", err)
	}
	rangeRegression := verified
	rangeRegression.stateGeneration++
	rangeRegression.checkpointGeneration++
	rangeRegression.verifiedRanges = []FileCheckpointRange{{Offset: 0, End: 8}}
	rangeRegression.commitState = FileCheckpointCommitCandidate
	rangeRegression.checksum = rangeRegression.derivedChecksum()
	if err := ValidateCheckpointTransition(verified, rangeRegression); !errors.Is(err, ErrFileCheckpointGeneration) {
		t.Fatalf("range regression = %v", err)
	}

	maxed := verified
	maxed.stateGeneration = ^uint64(0)
	maxed.checkpointGeneration = ^uint64(0)
	maxed.checksum = maxed.derivedChecksum()
	if _, err := CheckpointGenerationAdvance(maxed, maxed.verifiedRanges, FileCheckpointPhasePaused, FileCheckpointCommitCandidate); !errors.Is(err, ErrFileCheckpointGeneration) {
		t.Fatalf("generation overflow = %v", err)
	}
	if _, err := CheckpointGenerationAdvance(FileCheckpointV1{}, nil, FileCheckpointPhaseActive, FileCheckpointCommitCandidate); !errors.Is(err, ErrFileCheckpointOwnership) {
		t.Fatalf("invalid previous checkpoint = %v", err)
	}
}

func TestFileCheckpointV1Wave2CodecAndIdentityEdges(t *testing.T) {
	candidate := checkpointV1Fixture(t, 1, FileCheckpointCommitCandidate)
	for _, raw := range [][]byte{nil, {1}, make([]byte, 31), make([]byte, 33)} {
		if _, err := FileCheckpointRecordIDFromBytes(raw); err == nil {
			t.Fatalf("record ID length %d accepted", len(raw))
		}
		if _, err := FileCheckpointChecksumFromBytes(raw); err == nil {
			t.Fatalf("checksum length %d accepted", len(raw))
		}
	}
	validID, err := FileCheckpointRecordIDFromBytes(bytes.Repeat([]byte{0x7a}, 32))
	if err != nil || validID.IsZero() || !bytes.Equal(validID.Bytes(), bytes.Repeat([]byte{0x7a}, 32)) {
		t.Fatalf("valid record ID = %v", err)
	}
	checksum, err := FileCheckpointChecksumFromBytes(bytes.Repeat([]byte{0x7b}, 32))
	if err != nil || checksum.IsZero() || !bytes.Equal(checksum.Bytes(), bytes.Repeat([]byte{0x7b}, 32)) {
		t.Fatalf("valid checksum = %v", err)
	}
	if _, err := NewTransferIntentDigest(bytes.Repeat([]byte{1}, transfer.TransferIntentDigestBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTransferIntentDigest(nil); err == nil {
		t.Fatal("invalid transfer digest accepted")
	}
	if candidate.Ranges()[0].Length() != 16 || (FileCheckpointRange{Offset: 4, End: 4}).Length() != 0 {
		t.Fatal("range length semantics changed")
	}

	encoded, err := EncodeFileCheckpointV1(candidate)
	if err != nil {
		t.Fatal(err)
	}
	badMagic := append([]byte(nil), encoded...)
	badMagic[0] ^= 1
	if _, err := DecodeFileCheckpointV1(badMagic); !errors.Is(err, ErrInvalidFileCheckpoint) {
		t.Fatalf("bad magic = %v", err)
	}
	badLength := append([]byte(nil), encoded...)
	binary.BigEndian.PutUint32(badLength[len(fileCheckpointMagic):], 1)
	if _, err := DecodeFileCheckpointV1(badLength); !errors.Is(err, ErrInvalidFileCheckpoint) {
		t.Fatalf("bad payload length = %v", err)
	}
	badChecksum := append([]byte(nil), encoded...)
	badChecksum[len(badChecksum)-1] ^= 1
	if _, err := DecodeFileCheckpointV1(badChecksum); !errors.Is(err, ErrFileCheckpointChecksum) {
		t.Fatalf("bad checksum = %v", err)
	}
	trailing := append(append([]byte(nil), encoded[:len(encoded)-sha256SizeForWave2]...), bytes.Repeat([]byte{0}, sha256SizeForWave2)...)
	if _, err := DecodeFileCheckpointV1(trailing); err == nil {
		t.Fatal("non-canonical trailing payload accepted")
	}
}

const sha256SizeForWave2 = 32

func TestFileCheckpointV1Wave2NamespaceCheckpointNames(t *testing.T) {
	record := checkpointV1Fixture(t, 1, FileCheckpointCommitCandidate)
	name := FileCheckpointName(record.RecordID())
	parsed, err := ParseFileCheckpointName(name.Shard(), name.Name())
	if err != nil || parsed != record.RecordID() {
		t.Fatalf("checkpoint name round trip = %x, %v", parsed, err)
	}
	wrongShard := "ff"
	if wrongShard == name.Shard() {
		wrongShard = "00"
	}
	for _, test := range [][2]string{{"zz", name.Name()}, {name.Shard(), "bad.checkpoint"}, {wrongShard, name.Name()}} {
		if _, err := ParseFileCheckpointName(test[0], test[1]); err == nil {
			t.Fatalf("malformed checkpoint name accepted: %q/%q", test[0], test[1])
		}
	}
}

func TestFileCheckpointV1Wave2OwnershipValidationBranches(t *testing.T) {
	root := bytes.Repeat([]byte{0x33}, 32)
	valid, err := NewFileCheckpointOwnership("test/native", root)
	if err != nil {
		t.Fatal(err)
	}
	for name, invalid := range map[string]FileCheckpointOwnership{
		"marker":    func() FileCheckpointOwnership { value := valid; value.Marker = "wrong"; return value }(),
		"namespace": func() FileCheckpointOwnership { value := valid; value.Namespace = "wrong"; return value }(),
		"backend":   func() FileCheckpointOwnership { value := valid; value.BackendID = ""; return value }(),
		"root": func() FileCheckpointOwnership {
			value := valid
			value.RootIdentity = FileCheckpointRootID{}
			return value
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if invalid.valid() == nil || invalid.CanonicalBytes() != nil {
				t.Fatalf("invalid ownership projected as canonical: %+v", invalid)
			}
			if _, err := EncodeFileCheckpointOwnership(invalid); err == nil {
				t.Fatal("invalid ownership encoded")
			}
		})
	}
}
