package checkpointmodel

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestRecordSeamOwnsCanonicalBindingAndInitialPromotion(t *testing.T) {
	spec := canonicalRecordSpec(t)
	spec.CheckpointGeneration = 0
	spec.VerifiedRanges = nil
	record := mustCanonicalRecord(t, spec)
	ownership := ownershipForRecord(t, spec)
	binding, err := NewBinding(
		ownership,
		spec.OperationID,
		spec.ReceiveIntentDigest,
		spec.MaterializationBindingDigest,
	)
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
		t.Fatal("record seam changed the canonical FileCheckpointV2 image")
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
	spec := canonicalRecordSpec(t)
	record := mustCanonicalRecord(t, spec)
	ownership := ownershipForRecord(t, spec)
	if _, err := NewBinding(
		Ownership{}, spec.OperationID, spec.ReceiveIntentDigest, spec.MaterializationBindingDigest,
	); !errors.Is(err, ErrRecordBinding) {
		t.Fatalf("zero ownership binding error = %v", err)
	}
	if _, err := NewBinding(
		ownership, receivecontract.OperationID{}, spec.ReceiveIntentDigest, spec.MaterializationBindingDigest,
	); !errors.Is(err, ErrRecordBinding) {
		t.Fatalf("zero operation binding error = %v", err)
	}
	if _, err := NewBinding(
		ownership, spec.OperationID, transfer.ReceiveIntentDigest{}, spec.MaterializationBindingDigest,
	); !errors.Is(err, ErrRecordBinding) {
		t.Fatalf("zero intent binding error = %v", err)
	}
	if _, err := NewBinding(
		ownership, spec.OperationID, spec.ReceiveIntentDigest, receivecontract.BindingDigest{},
	); !errors.Is(err, ErrRecordBinding) {
		t.Fatalf("zero materialization binding error = %v", err)
	}
	binding, err := NewBinding(
		ownership, spec.OperationID, spec.ReceiveIntentDigest, spec.MaterializationBindingDigest,
	)
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
	zeroPhase := spec
	zeroPhase.Phase = 0
	if _, err := NewRecord(zeroPhase); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("zero checkpoint phase error = %v", err)
	}
	zeroCommit := spec
	zeroCommit.CommitState = 0
	if _, err := NewRecord(zeroCommit); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("zero checkpoint commit state error = %v", err)
	}
}

func ownershipForRecord(t *testing.T, spec RecordSpec) Ownership {
	t.Helper()
	ownership, err := NewOwnership(OwnershipSpec{
		Materializer:        spec.MaterializerKind,
		Certification:       CertificationWindowsNTFSProcessRestart,
		AuthorityRef:        spec.AuthorityRef,
		RootOpenDisposition: CallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ownership
}
