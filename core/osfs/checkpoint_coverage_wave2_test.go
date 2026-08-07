package osfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/internal/testoutputroot"
	"github.com/windshare/windshare/core/transfer"
)

func wave2CheckpointSpec() FileCheckpointSpec {
	var digest transfer.TransferIntentDigest
	digest[0] = 1
	var fileID catalog.FileID
	fileID[0] = 2
	var revision content.FileRevision
	revision[0] = 3
	return FileCheckpointSpec{
		TransferIntentDigest: digest,
		FileID:               fileID,
		FileRevision:         revision,
		CanonicalPath:        "folder/file.bin",
		ExactSize:            64,
		BackendID:            "test/native",
		RootIdentity:         bytes.Repeat([]byte{4}, 32),
		OwnedOutputObject:    bytes.Repeat([]byte{5}, 32),
		StateGeneration:      1,
		CheckpointGeneration: 1,
		VerifiedRanges:       []FileCheckpointRange{{Offset: 0, End: 16}, {Offset: 32, End: 64}},
		Phase:                FileCheckpointPhaseActive,
		CommitState:          FileCheckpointCommitCandidate,
	}
}

func TestFileCheckpointFacadeRoundTripAndAccessors(t *testing.T) {
	candidate, err := NewFileCheckpointV1(wave2CheckpointSpec())
	if err != nil {
		t.Fatalf("construct checkpoint: %v", err)
	}
	if !candidate.Valid() || candidate.SchemaVersion() != FileCheckpointV1SchemaVersion ||
		candidate.OwnershipMarker() != FileCheckpointOwnershipMarker || candidate.Namespace() != FileCheckpointNamespace {
		t.Fatalf("candidate does not expose a valid V1 contract")
	}
	if candidate.RecordID().IsZero() || candidate.RootIdentity().IsZero() || candidate.OwnedOutputObject().IsZero() || candidate.Checksum().IsZero() {
		t.Fatal("checkpoint identities were not derived")
	}
	if candidate.TransferIntentDigest().IsZero() || candidate.FileID().IsZero() || candidate.FileRevision().IsZero() ||
		candidate.CanonicalPath() != "folder/file.bin" || candidate.ExactSize() != 64 || candidate.BackendID() != transfer.OutputBackendID("test/native") ||
		candidate.StateGeneration() != 1 || candidate.CheckpointGeneration() != 1 || candidate.Phase() != FileCheckpointPhaseActive ||
		candidate.CommitState() != FileCheckpointCommitCandidate {
		t.Fatal("checkpoint accessor projection changed the binding")
	}
	if got := candidate.Ranges(); len(got) != 2 || got[0].Length() != 16 || got[1].Length() != 32 {
		t.Fatalf("ranges = %+v", got)
	}
	got := candidate.VerifiedRanges()
	got[0].End = 1
	if candidate.VerifiedRanges()[0].End != 16 {
		t.Fatal("range accessor leaked mutable storage")
	}

	encoded, err := candidate.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeFileCheckpointV1(encoded)
	if err != nil || !CheckpointIdentityEqual(candidate, decoded) || !bytes.Equal(candidate.CanonicalBytes(), decoded.CanonicalBytes()) {
		t.Fatalf("decode = %v, identity equal = %v", err, CheckpointIdentityEqual(candidate, decoded))
	}
	if raw, err := EncodeFileCheckpointV1(candidate); err != nil || !bytes.Equal(raw, encoded) {
		t.Fatalf("facade encode = %v", err)
	}
	if raw, err := EncodeFileCheckpointRecord(candidate); err != nil || !bytes.Equal(raw, encoded) {
		t.Fatalf("record alias encode = %v", err)
	}
	if record, err := NewFileCheckpointRecord(wave2CheckpointSpec()); err != nil || !record.Valid() {
		t.Fatalf("record alias construct = %v", err)
	}
	if record, err := DecodeFileCheckpointRecord(encoded); err != nil || !record.Valid() {
		t.Fatalf("record alias decode = %v", err)
	}

	var stream bytes.Buffer
	if err := WriteFileCheckpointV1(&stream, candidate); err != nil {
		t.Fatalf("write stream: %v", err)
	}
	read, err := ReadFileCheckpointV1(bytes.NewReader(stream.Bytes()))
	if err != nil || !bytes.Equal(read.Bytes(), candidate.Bytes()) {
		t.Fatalf("read stream = %v", err)
	}
	if err := WriteFileCheckpointRecord(&stream, candidate); err != nil {
		t.Fatalf("write record stream: %v", err)
	}
	if _, err := ReadFileCheckpointRecord(bytes.NewReader(encoded)); err != nil {
		t.Fatalf("read record stream: %v", err)
	}
	if _, err := ReadFileCheckpointV1(nil); err == nil || !errors.Is(err, ErrInvalidFileCheckpoint) {
		t.Fatalf("nil reader error = %v", err)
	}
	if err := WriteFileCheckpointV1(nil, candidate); err == nil || !errors.Is(err, ErrInvalidFileCheckpoint) {
		t.Fatalf("nil writer error = %v", err)
	}
	if _, err := DecodeFileCheckpointV1(nil); err == nil || !errors.Is(err, ErrInvalidFileCheckpoint) {
		t.Fatalf("empty decode error = %v", err)
	}
	if _, err := EncodeFileCheckpointV1(FileCheckpointV1{}); err == nil || !errors.Is(err, ErrInvalidFileCheckpoint) {
		t.Fatalf("zero checkpoint encode error = %v", err)
	}
	zero := FileCheckpointV1{}
	if zero.Valid() || zero.CanonicalBytes() != nil || zero.RecordID().IsZero() == false || zero.RootIdentity().IsZero() == false || zero.OwnedOutputObject().IsZero() == false {
		t.Fatal("zero checkpoint was projected as valid")
	}
}

func TestFileCheckpointFacadeTransitionsRecoveryAndOwnership(t *testing.T) {
	candidate, err := NewFileCheckpointV1(wave2CheckpointSpec())
	if err != nil {
		t.Fatal(err)
	}
	verified, err := PromoteCheckpoint(candidate, FileCheckpointPhaseActive, FileCheckpointCommitVerified)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	nextCandidate, err := CheckpointGenerationAdvance(verified,
		[]FileCheckpointRange{{Offset: 0, End: 64}}, FileCheckpointPhasePaused, FileCheckpointCommitCandidate)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	nextVerified, err := PromoteCheckpoint(nextCandidate, FileCheckpointPhasePaused, FileCheckpointCommitPublished)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if nextVerified.RecordID() != candidate.RecordID() || nextVerified.StateGeneration() != 2 || nextVerified.CheckpointGeneration() != 2 {
		t.Fatal("generation advance changed immutable identity")
	}
	if err := ValidateCheckpointTransition(verified, nextCandidate); err != nil {
		t.Fatalf("valid transition: %v", err)
	}
	if !CheckpointIdentityEqual(verified, nextVerified) || CheckpointIdentityEqual(verified, FileCheckpointV1{}) {
		t.Fatal("identity comparison is not binding-only")
	}
	selected, err := SelectVerifiedCheckpoint(candidate, verified, nextVerified)
	if err != nil || selected.CheckpointGeneration() != 2 {
		t.Fatalf("selected = %+v, %v", selected, err)
	}
	if _, err := SelectVerifiedCheckpoint(candidate); err == nil || !errors.Is(err, ErrFileCheckpointRecovery) {
		t.Fatalf("candidate-only selection = %v", err)
	}
	recovered, err := RecoverFileCheckpoint(&verified, &nextVerified)
	if err != nil || recovered.CheckpointGeneration() != 2 {
		t.Fatalf("recovered next = %+v, %v", recovered, err)
	}
	if recovered, err := RecoverFileCheckpoint(&verified, &candidate); err != nil || recovered.CheckpointGeneration() != verified.CheckpointGeneration() {
		t.Fatalf("candidate did not preserve committed record: %+v, %v", recovered, err)
	}
	if recovered, err := RecoverFileCheckpoint(&verified, nil); err != nil || recovered.RecordID() != verified.RecordID() {
		t.Fatalf("nil candidate recovery: %+v, %v", recovered, err)
	}
	if _, err := RecoverFileCheckpoint(nil, &candidate); err == nil || !errors.Is(err, ErrFileCheckpointRecovery) {
		t.Fatalf("nil committed recovery = %v", err)
	}

	ownership, err := NewFileCheckpointOwnership("test/native", bytes.Repeat([]byte{9}, 32))
	if err != nil || !ownership.Valid() || ownership.Marker != FileCheckpointOwnershipMarker || ownership.Namespace != FileCheckpointNamespace {
		t.Fatalf("ownership = %+v, %v", ownership, err)
	}
	ownershipBytes, err := EncodeFileCheckpointOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}
	decodedOwnership, err := DecodeFileCheckpointOwnership(ownershipBytes)
	if err != nil || decodedOwnership != ownership || !bytes.Equal(ownership.CanonicalBytes(), decodedOwnership.CanonicalBytes()) {
		t.Fatalf("ownership round trip = %+v, %v", decodedOwnership, err)
	}
	ownershipBytes[len(ownershipBytes)-1] ^= 1
	if _, err := DecodeFileCheckpointOwnership(ownershipBytes); err == nil || !errors.Is(err, ErrFileCheckpointChecksum) {
		t.Fatalf("ownership checksum error = %v", err)
	}
	if _, err := NewFileCheckpointOwnership("", bytes.Repeat([]byte{9}, 32)); err == nil || !errors.Is(err, ErrFileCheckpointOwnership) {
		t.Fatalf("invalid ownership backend = %v", err)
	}

	canonical, err := CanonicalizeFileCheckpointRanges([]FileCheckpointRange{{Offset: 8, End: 12}, {Offset: 0, End: 8}, {Offset: 11, End: 16}})
	if err != nil || len(canonical) != 1 || canonical[0] != (FileCheckpointRange{Offset: 0, End: 16}) {
		t.Fatalf("canonical ranges = %+v, %v", canonical, err)
	}
	if _, err := CanonicalizeFileCheckpointRanges([]FileCheckpointRange{{Offset: 2, End: 2}}); err == nil {
		t.Fatal("empty range accepted")
	}
	if _, err := NewTransferIntentDigest(bytes.Repeat([]byte{1}, transfer.TransferIntentDigestBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTransferIntentDigest(nil); err == nil {
		t.Fatal("invalid intent digest accepted")
	}
	for _, phase := range []FileCheckpointPhase{FileCheckpointPhaseReserved, FileCheckpointPhaseRetired} {
		if !phase.Valid() {
			t.Fatalf("phase %d reported invalid", phase)
		}
	}
	if FileCheckpointPhase(0).Valid() || FileCheckpointPhase(99).Valid() || FileCheckpointCommitState(0).Valid() || FileCheckpointCommitState(99).Valid() {
		t.Fatal("invalid phase/commit state reported valid")
	}
}

func TestFileCheckpointFacadeCleanerProjectionAndOwnership(t *testing.T) {
	fixture := testoutputroot.New(t)
	if err := os.Mkdir(fixture.RootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	config := OneShotCheckpointCleanerConfig{RootPath: fixture.RootPath}
	report, err := RunOneShotCheckpointCleanup(context.Background(), config)
	if err != nil || !report.Complete || report.NeedsAttention() {
		t.Fatalf("cleanup report = %+v, %v", report, err)
	}
	second, err := CleanOwnedNamespace(context.Background(), config)
	if err != nil || !second.Complete || second.Resumed {
		t.Fatalf("rerun report = %+v, %v", second, err)
	}

	unknownFixture := testoutputroot.New(t)
	if err := os.Mkdir(unknownFixture.RootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(unknownFixture.RootPath, ".wsresume-output-foreign.journal")
	if err := os.WriteFile(foreign, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	attention, err := CleanLegacyResumeState(
		context.Background(), FilesystemResumeRoot{RootPath: unknownFixture.RootPath},
	)
	if err != nil || !attention.NeedsAttention() || attention.Status != CheckpointCleanupStatusNeedsAttention {
		t.Fatalf("unknown owner report = %+v, %v", attention, err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("unowned root entry was mutated: %v", err)
	}
	if _, err := NewOneShotCheckpointCleaner(OneShotCheckpointCleanerConfig{RootPath: "relative"}); err == nil || !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("relative root error = %v", err)
	}
	var nilCleaner *OneShotCheckpointCleaner
	if _, err := nilCleaner.Run(context.Background()); err == nil || !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("nil cleaner error = %v", err)
	}
}
