package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

func TestRecordSeamOwnsCanonicalBindingAndInitialPromotion(t *testing.T) {
	ownership, intent := recordBindingFixture(t)
	record := initialRecordFixture(t, ownership, intent)
	binding, err := NewBinding(ownership, intent)
	if err != nil {
		t.Fatal(err)
	}
	if !InitialCandidate(record) || Committed(record) || !binding.Matches(record, record.RecordID()) {
		t.Fatal("initial record lost its repository binding or lifecycle state")
	}

	encoded, err := EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeRecord(encoded)
	if err != nil {
		t.Fatal(err)
	}
	restoredID, err := RecordIDFromBytes(restored.RecordID().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if restoredID != record.RecordID() || !bytes.Equal(restored.CanonicalBytes(), record.CanonicalBytes()) {
		t.Fatal("record seam changed the canonical FileCheckpointV1 image")
	}

	promoted, err := PromoteInitialCandidate(restored)
	if err != nil {
		t.Fatal(err)
	}
	if InitialCandidate(promoted) || !Committed(promoted) ||
		promoted.CommitState() != CommitVerified || ValidateTransition(restored, promoted) != nil {
		t.Fatal("initial promotion did not produce the canonical committed successor")
	}
	if _, err := PromoteInitialCandidate(promoted); !errors.Is(err, ErrRecordGeneration) {
		t.Fatalf("repeated initial promotion error = %v", err)
	}

	corrupt := append([]byte(nil), encoded...)
	corrupt[len(corrupt)-1] ^= 1
	if _, err := DecodeRecord(corrupt); !errors.Is(err, ErrRecordChecksum) {
		t.Fatalf("corrupt record error = %v", err)
	}
}

func TestBindingRejectsIncompleteAndDifferentRepositoryIdentities(t *testing.T) {
	ownership, intent := recordBindingFixture(t)
	record := initialRecordFixture(t, ownership, intent)
	if _, err := NewBinding(Ownership{}, intent); !errors.Is(err, ErrRecordBinding) {
		t.Fatalf("zero ownership binding error = %v", err)
	}
	if _, err := NewBinding(ownership, transfer.TransferIntentDigest{}); !errors.Is(err, ErrRecordBinding) {
		t.Fatalf("zero intent binding error = %v", err)
	}
	binding, err := NewBinding(ownership, intent)
	if err != nil {
		t.Fatal(err)
	}
	otherIDRaw := record.RecordID().Bytes()
	otherIDRaw[0] ^= 1
	otherID, err := RecordIDFromBytes(otherIDRaw)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Matches(record, otherID) {
		t.Fatal("binding accepted a different fixed-record identity")
	}
	if _, err := RecordIDFromBytes([]byte{1}); err == nil {
		t.Fatal("short fixed-record identity was accepted")
	}
	if _, err := NewRecord(RecordSpec{}); !errors.Is(err, ErrRecordBinding) {
		t.Fatalf("empty record error = %v", err)
	}
}

func recordBindingFixture(t *testing.T) (Ownership, transfer.TransferIntentDigest) {
	t.Helper()
	backend, err := transfer.NewOutputBackendID("checkpointmodel-test")
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := NewOwnership(OwnershipSpec{
		Backend: backend, Certification: CertificationWindowsNTFSProcessRestart,
		RootIdentity:        bytes.Repeat([]byte{0x41}, sha256.Size),
		RootOpenDisposition: CallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0x51}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	return ownership, intent
}

func initialRecordFixture(t *testing.T, ownership Ownership, intent transfer.TransferIntentDigest) Record {
	t.Helper()
	var fileID catalog.FileID
	var revision content.FileRevision
	for index := range fileID {
		fileID[index] = byte(index + 1)
		revision[index] = byte(index + 2)
	}
	record, err := NewRecord(RecordSpec{
		TransferIntentDigest: intent,
		FileID:               fileID,
		FileRevision:         revision,
		CanonicalPath:        "folder/file.bin",
		ExactSize:            64,
		BackendID:            string(ownership.Backend()),
		RootIdentity:         ownership.RootIdentity().Bytes(),
		OwnedOutputObject:    bytes.Repeat([]byte{0x61}, sha256.Size),
		StateGeneration:      1,
		CheckpointGeneration: 0,
		Phase:                PhaseActive,
		CommitState:          CommitCandidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}
