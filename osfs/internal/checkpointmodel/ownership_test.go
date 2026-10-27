package checkpointmodel

import (
	"bytes"
	"errors"
	"testing"
)

func TestOwnershipRoundTripBindsRootDisposition(t *testing.T) {
	for _, disposition := range []RootOpenDisposition{CallerProvidedContainer, AuthorityCreatedRoot} {
		t.Run(string(disposition), func(t *testing.T) {
			ownership, err := NewOwnership(OwnershipSpec{
				Materializer: MaterializerNativeTree, IdentityScheme: IdentitySchemeWindowsObjectIDV1,
				AuthorityRef:        bytes.Repeat([]byte{0x41}, 32),
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
			if restored != ownership || restored.IdentityScheme() != IdentitySchemeWindowsObjectIDV1 ||
				restored.RootOpenDisposition() != disposition ||
				!bytes.Equal(restored.CanonicalBytes(), ownership.CanonicalBytes()) {
				t.Fatal("ownership round trip changed the certified binding")
			}
		})
	}
}

func TestOwnershipRejectsMissingUnknownAndTamperedIdentityScheme(t *testing.T) {
	valid, err := NewOwnership(OwnershipSpec{
		Materializer: MaterializerNativeTree, IdentityScheme: IdentitySchemeLinuxHandleV2,
		AuthorityRef:        bytes.Repeat([]byte{0x51}, 32),
		RootOpenDisposition: CallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewOwnership(OwnershipSpec{
		Materializer: MaterializerNativeTree, IdentityScheme: IdentitySchemeLinuxHandleV2,
		AuthorityRef:        bytes.Repeat([]byte{0x51}, 32),
		RootOpenDisposition: "future-value",
	}); !errors.Is(err, ErrInvalidOwnership) {
		t.Fatalf("unknown disposition error = %v", err)
	}
	if _, err := NewOwnership(OwnershipSpec{
		Materializer: MaterializerNativeTree, IdentityScheme: IdentitySchemeLinuxHandleV2,
		AuthorityRef:        bytes.Repeat([]byte{0x51}, 31),
		RootOpenDisposition: CallerProvidedContainer,
	}); !errors.Is(err, ErrInvalidOwnership) {
		t.Fatalf("short authority reference error = %v", err)
	}
	if _, err := NewOwnership(OwnershipSpec{
		Materializer: MaterializerNativeTree, IdentityScheme: "future-identityScheme",
		AuthorityRef:        bytes.Repeat([]byte{0x51}, 32),
		RootOpenDisposition: CallerProvidedContainer,
	}); !errors.Is(err, ErrInvalidOwnership) {
		t.Fatalf("unknown identityScheme error = %v", err)
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
	// IdentityScheme is an independent restart proof, not a value inferred from
	// the backend or root identity. The earlier field set is not dual-read.
	var preIdentityScheme bytes.Buffer
	writeOwnershipString(&preIdentityScheme, ownershipDomain)
	writeOwnershipString(&preIdentityScheme, OwnershipMarker)
	writeOwnershipString(&preIdentityScheme, NamespaceName)
	preIdentityScheme.WriteByte(byte(valid.MaterializerKind()))
	_, _ = preIdentityScheme.Write(valid.AuthorityRef().Bytes())
	writeOwnershipString(&preIdentityScheme, string(valid.RootOpenDisposition()))
	legacyPayload = preIdentityScheme.Bytes()
	legacyChecksum = ownershipChecksum(legacyPayload)
	legacy = append(append([]byte(nil), legacyPayload...), legacyChecksum[:]...)
	if _, err := DecodeOwnership(legacy); !errors.Is(err, ErrInvalidOwnership) {
		t.Fatalf("pre-identityScheme ownership error = %v", err)
	}
}

func TestNativeRepositoryRejectsUnsupportedMaterializers(t *testing.T) {
	if !MaterializerNativeTree.Valid() {
		t.Fatal("native materializer is invalid")
	}
	for _, kind := range []MaterializerKind{0, 2, 3, 4, 5, 255} {
		if kind.Valid() {
			t.Fatalf("unsupported materializer %d is valid", kind)
		}
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
