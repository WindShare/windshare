package legacyresume

import (
	"bytes"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestDecodeOwnershipAcceptsOnlyExactLegacyControlEnvelope(t *testing.T) {
	root := bytes.Repeat([]byte{0x4a}, RootIdentityBytes)
	encoded, err := encodeStoredOwnership(storedOwnership{
		Schema: legacySchemaVersion, Backend: string(transfer.NativeFilesystemOutputBackendID),
		RootIdentity: root, Certification: CertificationWindowsNTFSProcessRestart,
		Durability: legacyStoredProcessRestart, Generation: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := DecodeOwnership(encoded)
	if err != nil {
		t.Fatal(err)
	}
	expected := ExpectedOwnership{
		Backend: transfer.NativeFilesystemOutputBackendID, RootIdentity: root,
		Certification: CertificationWindowsNTFSProcessRestart,
		Durability:    transfer.DurabilityProcessRestart,
	}
	if !ownership.Matches(expected) {
		t.Fatal("exact certified ownership did not match")
	}

	for name, mutate := range map[string]func([]byte){
		"magic":    func(raw []byte) { raw[0] ^= 0xff },
		"length":   func(raw []byte) { raw[len(legacyControlMagic)+3]++ },
		"payload":  func(raw []byte) { raw[len(legacyControlMagic)+legacyEnvelopeLengthBytes] ^= 0x01 },
		"checksum": func(raw []byte) { raw[len(raw)-1] ^= 0xff },
	} {
		t.Run(name, func(t *testing.T) {
			corrupt := append([]byte(nil), encoded...)
			mutate(corrupt)
			if _, err := DecodeOwnership(corrupt); err == nil {
				t.Fatal("corrupt legacy ownership was accepted")
			}
		})
	}
}

func TestOwnershipMatchingRequiresEveryCertifiedFact(t *testing.T) {
	root := bytes.Repeat([]byte{0x31}, RootIdentityBytes)
	encoded, err := encodeStoredOwnership(storedOwnership{
		Schema: legacySchemaVersion, Backend: string(transfer.NativeFilesystemOutputBackendID),
		RootIdentity: root, Certification: CertificationLinuxExt4ProcessRestart,
		Durability: legacyStoredProcessRestart, Generation: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := DecodeOwnership(encoded)
	if err != nil {
		t.Fatal(err)
	}
	exact := ExpectedOwnership{
		Backend: transfer.NativeFilesystemOutputBackendID, RootIdentity: root,
		Certification: CertificationLinuxExt4ProcessRestart,
		Durability:    transfer.DurabilityProcessRestart,
	}
	tests := map[string]ExpectedOwnership{
		"backend":       {Backend: "foreign", RootIdentity: root, Certification: exact.Certification, Durability: exact.Durability},
		"root":          {Backend: exact.Backend, RootIdentity: bytes.Repeat([]byte{0x32}, RootIdentityBytes), Certification: exact.Certification, Durability: exact.Durability},
		"certification": {Backend: exact.Backend, RootIdentity: root, Certification: CertificationWindowsNTFSProcessRestart, Durability: exact.Durability},
		"durability":    {Backend: exact.Backend, RootIdentity: root, Certification: exact.Certification, Durability: transfer.DurabilityPowerLoss},
		"missing-cert":  {Backend: exact.Backend, RootIdentity: root, Durability: exact.Durability},
	}
	for name, candidate := range tests {
		t.Run(name, func(t *testing.T) {
			if ownership.Matches(candidate) {
				t.Fatal("mismatched certified fact authorized legacy ownership")
			}
		})
	}
}

func TestLegacyLayoutClassifiersDoNotInterpretRecordContents(t *testing.T) {
	digest := string(bytes.Repeat([]byte{'a'}, encodedDigestCharacters))
	session := string(bytes.Repeat([]byte{'b'}, encodedSessionCharacters))
	if !IsIntentDirectory(digest) || !IsSessionDirectory(session) ||
		!IsSessionCandidate(SessionCandidatePrefix+session) ||
		!IsFileRecord("aa", digest+legacyRecordSuffix) ||
		!IsFileRecordTemporary("aa", digest+legacyRecordSuffix+legacyUpdateSeparator+digest) ||
		!IsAnchor("aa", digest+legacyAnchorSuffix) || !IsStage("aa", digest+legacyStageSuffix) {
		t.Fatal("canonical legacy control name was rejected")
	}
	for _, invalid := range []string{
		"", "AA", digest[:len(digest)-1], digest + "0", strings.Repeat("0", encodedDigestCharacters),
	} {
		if IsIntentDirectory(invalid) || IsShard(invalid) {
			t.Fatalf("non-canonical legacy name %q was accepted", invalid)
		}
	}
	zeroDigest := strings.Repeat("0", encodedDigestCharacters)
	zeroSession := strings.Repeat("0", encodedSessionCharacters)
	if IsSessionDirectory(zeroSession) || IsSessionCandidate(SessionCandidatePrefix+zeroSession) ||
		IsBootstrapCandidate(BootstrapCandidatePrefix+zeroDigest) ||
		IsControlTemporary(ControlRecord+legacyUpdateSeparator+zeroDigest) {
		t.Fatal("zero legacy identity was accepted")
	}
}
