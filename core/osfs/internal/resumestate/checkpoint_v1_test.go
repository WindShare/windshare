package resumestate

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

func checkpointV1Fixture(t *testing.T, generation uint64, commit FileCheckpointCommitState) FileCheckpointV1 {
	t.Helper()
	var intent transfer.TransferIntentDigest
	var fileID catalog.FileID
	var revision content.FileRevision
	for index := range intent {
		intent[index] = byte(index + 1)
	}
	for index := range fileID {
		fileID[index] = byte(index + 2)
		revision[index] = byte(index + 3)
	}
	root := bytes.Repeat([]byte{0x31}, 32)
	object := bytes.Repeat([]byte{0x41}, 32)
	checkpoint, err := NewFileCheckpointV1(FileCheckpointSpec{
		TransferIntentDigest: intent, FileID: fileID, FileRevision: revision,
		CanonicalPath: "folder/file.bin", ExactSize: 64, BackendID: "test/native",
		RootIdentity: root, OwnedOutputObject: object, StateGeneration: generation,
		CheckpointGeneration: generation, VerifiedRanges: []FileCheckpointRange{{Offset: 0, End: 16}, {Offset: 32, End: 64}},
		Phase: FileCheckpointPhaseActive, CommitState: commit,
	})
	if err != nil {
		t.Fatalf("construct checkpoint: %v", err)
	}
	return checkpoint
}

func TestFileCheckpointV1RoundTripAndStableIdentity(t *testing.T) {
	candidate := checkpointV1Fixture(t, 1, FileCheckpointCommitCandidate)
	checkpoint, err := PromoteCheckpoint(candidate, FileCheckpointPhaseActive, FileCheckpointCommitVerified)
	if err != nil {
		t.Fatalf("promote candidate: %v", err)
	}
	encoded, err := EncodeFileCheckpointV1(checkpoint)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeFileCheckpointV1(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(checkpoint.CanonicalBytes(), decoded.CanonicalBytes()) || checkpoint.RecordID() != decoded.RecordID() ||
		checkpoint.Checksum() != decoded.Checksum() {
		t.Fatalf("round trip changed canonical state")
	}
	next, err := CheckpointGenerationAdvance(checkpoint, []FileCheckpointRange{{Offset: 0, End: 64}}, FileCheckpointPhasePaused, FileCheckpointCommitVerified)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if next.RecordID() != checkpoint.RecordID() || next.CheckpointGeneration() != 2 || next.StateGeneration() != 2 {
		t.Fatalf("generation changed immutable identity")
	}
	corrupt := append([]byte(nil), encoded...)
	corrupt[len(corrupt)-1] ^= 1
	if _, err := DecodeFileCheckpointV1(corrupt); !errors.Is(err, ErrFileCheckpointChecksum) {
		t.Fatalf("checksum corruption error = %v", err)
	}
}

func TestFileCheckpointV1RejectsRangeAndGenerationRegression(t *testing.T) {
	fixture := checkpointV1Fixture(t, 1, FileCheckpointCommitVerified)
	for name, ranges := range map[string][]FileCheckpointRange{
		"overlap":  {{Offset: 0, End: 8}, {Offset: 4, End: 12}},
		"adjacent": {{Offset: 0, End: 8}, {Offset: 8, End: 12}},
		"outside":  {{Offset: 0, End: 65}},
	} {
		t.Run(name, func(t *testing.T) {
			spec := FileCheckpointSpec{
				TransferIntentDigest: fixture.TransferIntentDigest(), FileID: fixture.FileID(), FileRevision: fixture.FileRevision(),
				CanonicalPath: fixture.CanonicalPath(), ExactSize: fixture.ExactSize(), BackendID: string(fixture.BackendID()),
				RootIdentity: fixture.RootIdentity().Bytes(), OwnedOutputObject: fixture.OwnedOutputObject().Bytes(),
				StateGeneration: 1, CheckpointGeneration: 1, VerifiedRanges: ranges,
				Phase: FileCheckpointPhaseActive, CommitState: FileCheckpointCommitCandidate,
			}
			if _, err := NewFileCheckpointV1(spec); err == nil {
				t.Fatal("invalid ranges accepted")
			}
		})
	}
	next, err := CheckpointGenerationAdvance(fixture, fixture.VerifiedRanges(), FileCheckpointPhasePaused, FileCheckpointCommitVerified)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if _, err := CheckpointGenerationAdvance(next, fixture.VerifiedRanges(), FileCheckpointPhaseActive, FileCheckpointCommitVerified); err != nil {
		// The helper increments generation, so this is a valid transition. Build a
		// deliberately regressed record to exercise the guard instead.
		t.Fatalf("second advance: %v", err)
	}
	regressed, err := NewFileCheckpointV1(FileCheckpointSpec{
		TransferIntentDigest: next.TransferIntentDigest(), FileID: next.FileID(), FileRevision: next.FileRevision(),
		CanonicalPath: next.CanonicalPath(), ExactSize: next.ExactSize(), BackendID: string(next.BackendID()),
		RootIdentity: next.RootIdentity().Bytes(), OwnedOutputObject: next.OwnedOutputObject().Bytes(),
		StateGeneration: next.StateGeneration(), CheckpointGeneration: next.CheckpointGeneration(),
		VerifiedRanges: fixture.VerifiedRanges(), Phase: FileCheckpointPhasePaused, CommitState: FileCheckpointCommitVerified,
	})
	if err != nil {
		t.Fatalf("construct regressed: %v", err)
	}
	if err := ValidateCheckpointTransition(next, regressed); !errors.Is(err, ErrFileCheckpointGeneration) {
		t.Fatalf("regressed transition error = %v", err)
	}
}

func TestFileCheckpointOwnershipMarkerRoundTrip(t *testing.T) {
	marker, err := NewFileCheckpointOwnership("test/native", bytes.Repeat([]byte{0x51}, 32))
	if err != nil {
		t.Fatalf("marker: %v", err)
	}
	encoded, err := EncodeFileCheckpointOwnership(marker)
	if err != nil {
		t.Fatalf("encode marker: %v", err)
	}
	restored, err := DecodeFileCheckpointOwnership(encoded)
	if err != nil || restored != marker {
		t.Fatalf("marker round trip = %+v, %v", restored, err)
	}
	encoded[len(encoded)-1] ^= 1
	if _, err := DecodeFileCheckpointOwnership(encoded); !errors.Is(err, ErrFileCheckpointChecksum) {
		t.Fatalf("marker checksum error = %v", err)
	}
}

func TestFileCheckpointV1PublicCodecAndRecoveryHelpers(t *testing.T) {
	candidate := checkpointV1Fixture(t, 1, FileCheckpointCommitCandidate)
	verified, err := PromoteCheckpoint(candidate, FileCheckpointPhaseActive, FileCheckpointCommitVerified)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if _, err := NewFileCheckpointRecord(FileCheckpointRecordSpec{
		TransferIntentDigest: candidate.TransferIntentDigest(), FileID: candidate.FileID(), FileRevision: candidate.FileRevision(),
		CanonicalPath: candidate.CanonicalPath(), ExactSize: candidate.ExactSize(), BackendID: string(candidate.BackendID()),
		RootIdentity: candidate.RootIdentity().Bytes(), OwnedOutputObject: candidate.OwnedOutputObject().Bytes(),
		StateGeneration: 1, CheckpointGeneration: 1, VerifiedRanges: candidate.VerifiedRanges(),
		Phase: candidate.Phase(), CommitState: candidate.CommitState(),
	}); err != nil {
		t.Fatalf("record alias: %v", err)
	}
	encoded, err := EncodeFileCheckpointRecord(verified)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeFileCheckpointRecord(encoded)
	if err != nil || decoded.RecordID() != verified.RecordID() {
		t.Fatalf("record codec: %v", err)
	}
	var written bytes.Buffer
	if err := WriteFileCheckpointV1(&written, verified); err != nil {
		t.Fatal(err)
	}
	read, err := ReadFileCheckpointV1(bytes.NewReader(written.Bytes()))
	if err != nil || read.RecordID() != verified.RecordID() {
		t.Fatalf("stream codec: %v", err)
	}
	if _, err := ReadFileCheckpointV1(nil); err == nil {
		t.Fatal("nil reader accepted")
	}
	if err := WriteFileCheckpointV1(nil, verified); err == nil {
		t.Fatal("nil writer accepted")
	}
	selected, err := SelectVerifiedCheckpoint(candidate, verified)
	if err != nil || selected.CheckpointGeneration() != verified.CheckpointGeneration() {
		t.Fatalf("verified selection = %+v, %v", selected, err)
	}
	recovered, err := RecoverFileCheckpoint(&verified, &candidate)
	if err != nil || recovered.RecordID() != verified.RecordID() {
		t.Fatalf("candidate recovery = %+v, %v", recovered, err)
	}
	if _, err := RecoverFileCheckpoint(nil, &candidate); !errors.Is(err, ErrFileCheckpointRecovery) {
		t.Fatalf("nil committed error = %v", err)
	}
	if _, err := SelectVerifiedCheckpoint(candidate); !errors.Is(err, ErrFileCheckpointRecovery) {
		t.Fatalf("candidate-only error = %v", err)
	}
	encodedAgain, encodeErr := candidate.Encode()
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	if candidate.SchemaVersion() != FileCheckpointV1SchemaVersion || candidate.OwnershipMarker() == "" ||
		candidate.Namespace() == "" || candidate.Ranges() == nil || candidate.Phase() == 0 || candidate.CommitState() == 0 ||
		candidate.OutputObject().IsZero() || candidate.Bytes() == nil || encodedAgain == nil {
		t.Fatal("public checkpoint accessors are incomplete")
	}
}

func TestFileCheckpointV1CanonicalizationAndInvalidOwnership(t *testing.T) {
	canonical, err := CanonicalizeFileCheckpointRanges([]FileCheckpointRange{{Offset: 8, End: 12}, {Offset: 0, End: 8}, {Offset: 11, End: 16}})
	if err != nil || len(canonical) != 1 || canonical[0] != (FileCheckpointRange{Offset: 0, End: 16}) {
		t.Fatalf("canonical ranges = %+v, %v", canonical, err)
	}
	if _, err := CanonicalizeFileCheckpointRanges([]FileCheckpointRange{{Offset: 2, End: 2}}); err == nil {
		t.Fatal("empty range accepted")
	}
	for _, raw := range [][]byte{nil, {1}, make([]byte, 31), make([]byte, 33)} {
		if _, err := FileCheckpointRootIDFromBytes(raw); err == nil {
			t.Fatalf("root identity length %d accepted", len(raw))
		}
	}
	if _, err := NewFileCheckpointOwnership("", bytes.Repeat([]byte{1}, 32)); err == nil {
		t.Fatal("empty backend accepted")
	}
	if _, err := DecodeFileCheckpointOwnership([]byte("not a marker")); err == nil {
		t.Fatal("malformed marker accepted")
	}
	fixture := checkpointV1Fixture(t, 1, FileCheckpointCommitCandidate)
	if _, err := PromoteCheckpoint(fixture, FileCheckpointPhaseActive, FileCheckpointCommitCandidate); !errors.Is(err, ErrFileCheckpointCrashBoundary) {
		t.Fatalf("invalid promotion error = %v", err)
	}
	if err := ValidateCheckpointTransition(fixture, fixture); !errors.Is(err, ErrFileCheckpointGeneration) {
		t.Fatalf("same generation transition error = %v", err)
	}
	if _, err := NewFileCheckpointV1(FileCheckpointSpec{
		TransferIntentDigest: fixture.TransferIntentDigest(), FileID: fixture.FileID(), FileRevision: fixture.FileRevision(),
		CanonicalPath: "bad/path/", ExactSize: fixture.ExactSize(), BackendID: string(fixture.BackendID()),
		RootIdentity: fixture.RootIdentity().Bytes(), OwnedOutputObject: fixture.OwnedOutputObject().Bytes(),
		StateGeneration: 1, CheckpointGeneration: 1, VerifiedRanges: fixture.VerifiedRanges(),
		Phase: FileCheckpointPhaseActive, CommitState: FileCheckpointCommitCandidate,
	}); err == nil || !strings.Contains(err.Error(), "canonical path") {
		t.Fatalf("invalid path error = %v", err)
	}
}

func TestFileCheckpointV1AllowsInitialZeroCheckpointGeneration(t *testing.T) {
	fixture := checkpointV1Fixture(t, 1, FileCheckpointCommitCandidate)
	spec := FileCheckpointSpec{
		TransferIntentDigest: fixture.TransferIntentDigest(), FileID: fixture.FileID(), FileRevision: fixture.FileRevision(),
		CanonicalPath: fixture.CanonicalPath(), ExactSize: fixture.ExactSize(), BackendID: string(fixture.BackendID()),
		RootIdentity: fixture.RootIdentity().Bytes(), OwnedOutputObject: fixture.OwnedOutputObject().Bytes(),
		StateGeneration: 1, CheckpointGeneration: 0, VerifiedRanges: nil,
		Phase: FileCheckpointPhaseReserved, CommitState: FileCheckpointCommitCandidate,
	}
	candidate, err := NewFileCheckpointV1(spec)
	if err != nil {
		t.Fatalf("initial checkpoint: %v", err)
	}
	if candidate.CheckpointGeneration() != 0 {
		t.Fatalf("initial generation = %d", candidate.CheckpointGeneration())
	}
	if _, err := PromoteCheckpoint(candidate, FileCheckpointPhaseReserved, FileCheckpointCommitVerified); err != nil {
		t.Fatalf("promote initial checkpoint: %v", err)
	}
	if _, err := CheckpointGenerationAdvance(candidate, nil, FileCheckpointPhaseActive, FileCheckpointCommitCandidate); err != nil {
		t.Fatalf("advance initial checkpoint: %v", err)
	}
}
