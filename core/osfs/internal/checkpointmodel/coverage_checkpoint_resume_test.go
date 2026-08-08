package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestCertifiedBindingKeepsRepositoryAndRecordAuthorityDistinct(t *testing.T) {
	spec := canonicalRecordSpec(t)
	record := mustCanonicalRecord(t, spec)
	backend, err := transfer.NewOutputBackendID(spec.BackendID)
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := NewOwnership(OwnershipSpec{
		Backend:             backend,
		Certification:       CertificationLinuxExt4ProcessRestart,
		RootIdentity:        spec.RootIdentity,
		RootOpenDisposition: AuthorityCreatedRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := NewBinding(ownership, spec.TransferIntentDigest)
	if err != nil {
		t.Fatal(err)
	}

	if binding.Ownership() != ownership ||
		binding.TransferIntentDigest() != spec.TransferIntentDigest ||
		binding.Ownership().Certification() != CertificationLinuxExt4ProcessRestart ||
		binding.Ownership().RootOpenDisposition() != AuthorityCreatedRoot {
		t.Fatal("binding lost certified repository ownership")
	}
	if record.FileID() != spec.FileID || record.FileRevision() != spec.FileRevision ||
		record.QuarantineReason() != 0 || record.QuarantineOrigin() != 0 ||
		record.RetirementReason() != 0 {
		t.Fatal("record lost immutable file identity or introduced terminal claims")
	}
	if !binding.Matches(record, record.RecordID()) {
		t.Fatal("exact record did not match its certified intent binding")
	}

	wrongID, err := RecordIDFromBytes(bytes.Repeat([]byte{0xf1}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	if binding.Matches(record, wrongID) || binding.Matches(record, RecordID{}) ||
		(Binding{}).Matches(record, record.RecordID()) {
		t.Fatal("invalid repository or record identity granted authority")
	}

	foreignIntent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0xe1}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RecordSpec){
		"intent": func(value *RecordSpec) {
			value.TransferIntentDigest = foreignIntent
		},
		"root": func(value *RecordSpec) {
			value.RootIdentity = bytes.Repeat([]byte{0xe2}, sha256.Size)
		},
		"backend": func(value *RecordSpec) {
			value.BackendID = "checkpointmodel-foreign"
		},
	} {
		t.Run(name, func(t *testing.T) {
			foreign := spec
			mutate(&foreign)
			foreignRecord := mustCanonicalRecord(t, foreign)
			if binding.Matches(foreignRecord, foreignRecord.RecordID()) {
				t.Fatal("record crossed its certified repository boundary")
			}
		})
	}

	if _, err := NewBinding(Ownership{}, spec.TransferIntentDigest); !errors.Is(err, ErrRecordBinding) {
		t.Fatalf("zero ownership binding error = %v", err)
	}
	if _, err := NewBinding(ownership, transfer.TransferIntentDigest{}); !errors.Is(err, ErrRecordBinding) {
		t.Fatalf("zero intent binding error = %v", err)
	}
}

func TestOwnershipDecoderRejectsAuthenticatedUnknownRootDisposition(t *testing.T) {
	backend, err := transfer.NewOutputBackendID("checkpointmodel-certified")
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := NewOwnership(OwnershipSpec{
		Backend:             backend,
		Certification:       CertificationWindowsNTFSProcessRestart,
		RootIdentity:        bytes.Repeat([]byte{0x91}, sha256.Size),
		RootOpenDisposition: CallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A valid checksum proves only byte integrity; the closed disposition remains
	// an independent authority decision during restart.
	var payload bytes.Buffer
	writeOwnershipString(&payload, ownershipDomain)
	writeOwnershipString(&payload, OwnershipMarker)
	writeOwnershipString(&payload, NamespaceName)
	writeOwnershipString(&payload, string(ownership.Backend()))
	writeOwnershipString(&payload, string(ownership.Certification()))
	_, _ = payload.Write(ownership.RootIdentity().Bytes())
	writeOwnershipString(&payload, "future-root-disposition")
	checksum := ownershipChecksum(payload.Bytes())
	encoded := append(append([]byte(nil), payload.Bytes()...), checksum[:]...)

	if _, err := DecodeOwnership(encoded); !errors.Is(err, ErrInvalidOwnership) {
		t.Fatalf("authenticated unknown disposition error = %v", err)
	}
	if encoded, err := EncodeOwnership(Ownership{}); encoded != nil || !errors.Is(err, ErrInvalidOwnership) {
		t.Fatalf("zero ownership encoding = %x, %v", encoded, err)
	}
}
