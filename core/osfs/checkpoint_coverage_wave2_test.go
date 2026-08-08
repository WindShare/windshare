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
		candidate.OwnershipMarker() != FileCheckpointOwnershipMarker ||
		candidate.Namespace() != FileCheckpointNamespace {
		t.Fatal("candidate does not expose the canonical V1 contract")
	}
	if candidate.RecordID().IsZero() || candidate.RootIdentity().IsZero() ||
		candidate.OwnedOutputObject().IsZero() || candidate.Checksum().IsZero() {
		t.Fatal("checkpoint identities were not derived")
	}
	if candidate.TransferIntentDigest().IsZero() || candidate.FileID().IsZero() ||
		candidate.FileRevision().IsZero() || candidate.CanonicalPath() != "folder/file.bin" ||
		candidate.ExactSize() != 64 || candidate.BackendID() != transfer.OutputBackendID("test/native") ||
		candidate.StateGeneration() != 1 || candidate.CheckpointGeneration() != 1 ||
		candidate.Phase() != FileCheckpointPhaseActive ||
		candidate.CommitState() != FileCheckpointCommitCandidate {
		t.Fatal("checkpoint accessors changed the canonical binding")
	}
	ranges := candidate.VerifiedRanges()
	if len(ranges) != 2 || ranges[0].Length() != 16 || ranges[1].Length() != 32 {
		t.Fatalf("ranges = %+v", ranges)
	}
	ranges[0].End = 1
	if candidate.VerifiedRanges()[0].End != 16 {
		t.Fatal("range accessor leaked mutable storage")
	}

	encoded, err := EncodeFileCheckpointV1(candidate)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeFileCheckpointV1(encoded)
	if err != nil || candidate.RecordID() != decoded.RecordID() ||
		!bytes.Equal(candidate.CanonicalBytes(), decoded.CanonicalBytes()) {
		t.Fatalf("decode = %v, record IDs = (%x, %x)", err, candidate.RecordID(), decoded.RecordID())
	}
	tampered := append([]byte(nil), encoded...)
	tampered[len(tampered)-1] ^= 1
	if _, err := DecodeFileCheckpointV1(tampered); !errors.Is(err, ErrFileCheckpointChecksum) {
		t.Fatalf("tampered checkpoint error = %v", err)
	}
	if _, err := DecodeFileCheckpointV1(nil); !errors.Is(err, ErrInvalidFileCheckpoint) {
		t.Fatalf("empty decode error = %v", err)
	}
	if _, err := EncodeFileCheckpointV1(FileCheckpointV1{}); !errors.Is(err, ErrInvalidFileCheckpoint) {
		t.Fatalf("zero checkpoint encode error = %v", err)
	}
	if (FileCheckpointV1{}).Valid() || (FileCheckpointV1{}).CanonicalBytes() != nil {
		t.Fatal("zero checkpoint was projected as valid")
	}
}

func TestFileCheckpointFacadeOwnershipAndEnums(t *testing.T) {
	ownership, err := NewFileCheckpointOwnership(FileCheckpointOwnershipSpec{
		BackendID:           "test/native",
		Certification:       FileCheckpointCertificationWindowsNTFSProcessRestart,
		RootIdentity:        bytes.Repeat([]byte{9}, 32),
		RootOpenDisposition: FileCheckpointCallerProvidedContainer,
	})
	if err != nil || !ownership.Valid() ||
		ownership.Certification() != FileCheckpointCertificationWindowsNTFSProcessRestart ||
		ownership.RootOpenDisposition() != FileCheckpointCallerProvidedContainer {
		t.Fatalf("ownership = %+v, %v", ownership, err)
	}
	ownershipBytes, err := EncodeFileCheckpointOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}
	decodedOwnership, err := DecodeFileCheckpointOwnership(ownershipBytes)
	if err != nil || !bytes.Equal(ownership.CanonicalBytes(), decodedOwnership.CanonicalBytes()) {
		t.Fatalf("ownership round trip = %+v, %v", decodedOwnership, err)
	}
	foreignOwnership, err := NewFileCheckpointOwnership(FileCheckpointOwnershipSpec{
		BackendID:           "test/native",
		Certification:       FileCheckpointCertificationWindowsNTFSProcessRestart,
		RootIdentity:        bytes.Repeat([]byte{9}, 32),
		RootOpenDisposition: FileCheckpointAuthorityCreatedRoot,
	})
	if err != nil || bytes.Equal(ownership.CanonicalBytes(), foreignOwnership.CanonicalBytes()) {
		t.Fatal("ownership omitted the certified root disposition")
	}
	ownershipBytes[len(ownershipBytes)-1] ^= 1
	if _, err := DecodeFileCheckpointOwnership(ownershipBytes); !errors.Is(err, ErrFileCheckpointOwnershipChecksum) {
		t.Fatalf("ownership checksum error = %v", err)
	}
	if _, err := NewFileCheckpointOwnership(FileCheckpointOwnershipSpec{
		BackendID:           "test/native",
		Certification:       "future-certification",
		RootIdentity:        bytes.Repeat([]byte{9}, 32),
		RootOpenDisposition: FileCheckpointCallerProvidedContainer,
	}); !errors.Is(err, ErrFileCheckpointOwnership) {
		t.Fatalf("unknown certification error = %v", err)
	}
	for _, phase := range []FileCheckpointPhase{FileCheckpointPhaseReserved, FileCheckpointPhaseRetired} {
		if !phase.Valid() {
			t.Fatalf("phase %d reported invalid", phase)
		}
	}
	if FileCheckpointPhase(0).Valid() || FileCheckpointPhase(99).Valid() ||
		FileCheckpointCommitState(0).Valid() || FileCheckpointCommitState(99).Valid() {
		t.Fatal("invalid phase/commit state reported valid")
	}
}

func TestFileCheckpointFacadeCleanerProjectionAndOwnership(t *testing.T) {
	fixture := testoutputroot.New(t)
	if err := os.Mkdir(fixture.RootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	report, err := CleanLegacyResumeState(
		context.Background(), FilesystemResumeRoot{RootPath: fixture.RootPath},
	)
	if err != nil || !report.Complete || report.NeedsAttention() {
		t.Fatalf("cleanup report = %+v, %v", report, err)
	}
	second, err := CleanLegacyResumeState(
		context.Background(), FilesystemResumeRoot{RootPath: fixture.RootPath},
	)
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
	if _, err := CleanLegacyResumeState(
		context.Background(), FilesystemResumeRoot{RootPath: "relative"},
	); err == nil || !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("relative root error = %v", err)
	}
}
