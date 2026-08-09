package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestCertifiedBindingKeepsRepositoryAndRecordAuthorityDistinct(t *testing.T) {
	spec := canonicalRecordSpec(t)
	record := mustCanonicalRecord(t, spec)
	ownership, err := NewOwnership(OwnershipSpec{
		Materializer:        spec.MaterializerKind,
		Certification:       CertificationLinuxExt4ProcessRestart,
		AuthorityRef:        spec.AuthorityRef,
		RootOpenDisposition: AuthorityCreatedRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := NewBinding(
		ownership, spec.OperationID, spec.ReceiveIntentDigest, spec.MaterializationBindingDigest,
	)
	if err != nil {
		t.Fatal(err)
	}

	if binding.Ownership() != ownership || binding.OperationID() != spec.OperationID ||
		binding.ReceiveIntentDigest() != spec.ReceiveIntentDigest ||
		binding.MaterializationBindingDigest() != spec.MaterializationBindingDigest ||
		binding.Ownership().Certification() != CertificationLinuxExt4ProcessRestart ||
		binding.Ownership().RootOpenDisposition() != AuthorityCreatedRoot {
		t.Fatal("binding lost certified repository ownership")
	}
	if !binding.Matches(record, record.RecordID()) {
		t.Fatal("exact record did not match its certified operation binding")
	}

	wrongID, err := RecordIDFromBytes(bytes.Repeat([]byte{0xf1}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	if binding.Matches(record, wrongID) || binding.Matches(record, RecordID{}) ||
		(Binding{}).Matches(record, record.RecordID()) {
		t.Fatal("invalid repository or record identity granted authority")
	}

	foreignIntent, err := transfer.ReceiveIntentDigestFromBytes(bytes.Repeat([]byte{0xe1}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	foreignOperation, err := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{0xe2}, receivecontract.StableIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RecordSpec){
		"operation": func(value *RecordSpec) { value.OperationID = foreignOperation },
		"intent":    func(value *RecordSpec) { value.ReceiveIntentDigest = foreignIntent },
		"authority": func(value *RecordSpec) { value.AuthorityRef = bytes.Repeat([]byte{0xe3}, sha256.Size) },
		"materializer": func(value *RecordSpec) {
			value.MaterializerKind = MaterializerAtomicFile
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
}

func TestOwnershipDecoderRejectsAuthenticatedUnknownRootDisposition(t *testing.T) {
	ownership, err := NewOwnership(OwnershipSpec{
		Materializer:        MaterializerNativeTree,
		Certification:       CertificationWindowsNTFSProcessRestart,
		AuthorityRef:        bytes.Repeat([]byte{0x91}, sha256.Size),
		RootOpenDisposition: CallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Byte integrity cannot promote an unknown ownership claim into authority.
	var payload bytes.Buffer
	writeOwnershipString(&payload, ownershipDomain)
	writeOwnershipString(&payload, OwnershipMarker)
	writeOwnershipString(&payload, NamespaceName)
	payload.WriteByte(byte(ownership.MaterializerKind()))
	writeOwnershipString(&payload, string(ownership.Certification()))
	_, _ = payload.Write(ownership.AuthorityRef().Bytes())
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
