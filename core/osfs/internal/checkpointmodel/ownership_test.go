package checkpointmodel

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestOwnershipRoundTripBindsRootDisposition(t *testing.T) {
	backend, err := transfer.NewOutputBackendID("checkpointmodel-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, disposition := range []RootOpenDisposition{CallerProvidedContainer, AuthorityCreatedRoot} {
		t.Run(string(disposition), func(t *testing.T) {
			ownership, err := NewOwnership(OwnershipSpec{
				Backend: backend, Certification: CertificationWindowsNTFSProcessRestart,
				RootIdentity:        bytes.Repeat([]byte{0x41}, 32),
				RootOpenDisposition: disposition,
			})
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := EncodeOwnership(ownership)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := DecodeOwnership(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if restored != ownership || restored.Certification() != CertificationWindowsNTFSProcessRestart ||
				restored.RootOpenDisposition() != disposition ||
				!bytes.Equal(restored.CanonicalBytes(), ownership.CanonicalBytes()) {
				t.Fatal("ownership round trip changed the certified binding")
			}
		})
	}
}

func TestOwnershipRejectsMissingUnknownAndTamperedCertification(t *testing.T) {
	backend, err := transfer.NewOutputBackendID("checkpointmodel-test")
	if err != nil {
		t.Fatal(err)
	}
	valid, err := NewOwnership(OwnershipSpec{
		Backend: backend, Certification: CertificationLinuxExt4ProcessRestart,
		RootIdentity:        bytes.Repeat([]byte{0x51}, 32),
		RootOpenDisposition: CallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewOwnership(OwnershipSpec{
		Backend: backend, Certification: CertificationLinuxExt4ProcessRestart,
		RootIdentity:        bytes.Repeat([]byte{0x51}, 32),
		RootOpenDisposition: "future-value",
	}); !errors.Is(err, ErrInvalidOwnership) {
		t.Fatalf("unknown disposition error = %v", err)
	}
	if _, err := NewOwnership(OwnershipSpec{
		Backend: backend, Certification: CertificationLinuxExt4ProcessRestart,
		RootIdentity:        bytes.Repeat([]byte{0x51}, 31),
		RootOpenDisposition: CallerProvidedContainer,
	}); !errors.Is(err, ErrInvalidOwnership) {
		t.Fatalf("short root identity error = %v", err)
	}
	if _, err := NewOwnership(OwnershipSpec{
		Backend: backend, Certification: "future-certification",
		RootIdentity:        bytes.Repeat([]byte{0x51}, 32),
		RootOpenDisposition: CallerProvidedContainer,
	}); !errors.Is(err, ErrInvalidOwnership) {
		t.Fatalf("unknown certification error = %v", err)
	}
	encoded, err := EncodeOwnership(valid)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), encoded...)
	tampered[len(tampered)-1] ^= 1
	if _, err := DecodeOwnership(tampered); !errors.Is(err, ErrOwnershipChecksum) {
		t.Fatalf("tampered ownership error = %v", err)
	}
	// The pre-disposition payload is intentionally not accepted as a second
	// current-state format, even if its checksum is internally consistent.
	payload := valid.CanonicalBytes()
	dispositionFrameLength := 4 + len(CallerProvidedContainer)
	legacyPayload := payload[:len(payload)-dispositionFrameLength]
	legacyChecksum := ownershipChecksum(legacyPayload)
	legacy := append(append([]byte(nil), legacyPayload...), legacyChecksum[:]...)
	if _, err := DecodeOwnership(legacy); !errors.Is(err, ErrInvalidOwnership) {
		t.Fatalf("pre-disposition ownership error = %v", err)
	}
	// Certification is an independent restart proof, not a value inferred from
	// the backend or root identity. The earlier field set is not dual-read.
	var preCertification bytes.Buffer
	writeOwnershipString(&preCertification, ownershipDomain)
	writeOwnershipString(&preCertification, OwnershipMarker)
	writeOwnershipString(&preCertification, NamespaceName)
	writeOwnershipString(&preCertification, string(valid.Backend()))
	_, _ = preCertification.Write(valid.RootIdentity().Bytes())
	writeOwnershipString(&preCertification, string(valid.RootOpenDisposition()))
	legacyPayload = preCertification.Bytes()
	legacyChecksum = ownershipChecksum(legacyPayload)
	legacy = append(append([]byte(nil), legacyPayload...), legacyChecksum[:]...)
	if _, err := DecodeOwnership(legacy); !errors.Is(err, ErrInvalidOwnership) {
		t.Fatalf("pre-certification ownership error = %v", err)
	}
}

func TestRootOpenDispositionValuesAreClosedAndStable(t *testing.T) {
	if string(CallerProvidedContainer) != "caller-provided-container" ||
		string(AuthorityCreatedRoot) != "authority-created-root" {
		t.Fatal("root disposition encoding changed")
	}
	for _, disposition := range []RootOpenDisposition{"", "caller", "future"} {
		if disposition.Valid() {
			t.Fatalf("unknown disposition %q is valid", disposition)
		}
	}
}
