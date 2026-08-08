package legacyresume

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer"
)

func TestC5ClosureOwnershipDecoderRejectsMalformedOrNonCanonicalProofs(t *testing.T) {
	stored := c5ClosureOwnership()
	encoded, err := encodeStoredOwnership(stored)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := legacyEncoder.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	nonCanonicalPayload := bytes.Replace(payload, []byte{0x00, 0x01}, []byte{0x00, 0x18, 0x01}, 1)
	if bytes.Equal(nonCanonicalPayload, payload) {
		t.Fatal("test fixture did not find the schema value")
	}
	unknownFieldPayload, err := legacyEncoder.Marshal(map[uint64]any{
		0: legacySchemaVersion,
		1: string(transfer.NativeFilesystemOutputBackendID),
		2: stored.RootIdentity,
		3: stored.Certification,
		4: legacyStoredProcessRestart,
		5: uint64(1),
		6: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	zeroLength := make([]byte, len(legacyControlMagic)+legacyEnvelopeLengthBytes+sha256.Size+1)
	copy(zeroLength, legacyControlMagic[:])
	oversizedLength := append([]byte(nil), encoded...)
	binary.BigEndian.PutUint32(
		oversizedLength[len(legacyControlMagic):len(legacyControlMagic)+legacyEnvelopeLengthBytes],
		uint32(MaxOwnershipRecordBytes+1),
	)
	tests := map[string][]byte{
		"empty":                  nil,
		"oversized envelope":     make([]byte, MaxOwnershipRecordBytes+1),
		"zero payload":           zeroLength,
		"oversized payload size": oversizedLength,
		"truncated checksum":     encoded[:len(encoded)-1],
		"malformed payload":      c5ClosureOwnershipEnvelope([]byte{0xff}),
		"unknown field":          c5ClosureOwnershipEnvelope(unknownFieldPayload),
		"trailing payload":       c5ClosureOwnershipEnvelope(append(append([]byte(nil), payload...), 0x00)),
		"non-canonical integer":  c5ClosureOwnershipEnvelope(nonCanonicalPayload),
	}
	for name, candidate := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeOwnership(candidate); !errors.Is(err, ErrInvalidOwnershipRecord) {
				t.Fatalf("malformed proof error = %v", err)
			}
		})
	}
}

func TestC5ClosureOwnershipDecoderRejectsInvalidCertifiedFacts(t *testing.T) {
	tests := map[string]func(*storedOwnership){
		"schema":        func(stored *storedOwnership) { stored.Schema = 2 },
		"backend":       func(stored *storedOwnership) { stored.Backend = "" },
		"short root":    func(stored *storedOwnership) { stored.RootIdentity = stored.RootIdentity[:RootIdentityBytes-1] },
		"zero root":     func(stored *storedOwnership) { stored.RootIdentity = make([]byte, RootIdentityBytes) },
		"certification": func(stored *storedOwnership) { stored.Certification = "foreign/filesystem" },
		"durability":    func(stored *storedOwnership) { stored.Durability = 0 },
		"generation":    func(stored *storedOwnership) { stored.Generation = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			stored := c5ClosureOwnership()
			mutate(&stored)
			encoded, err := encodeStoredOwnership(stored)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeOwnership(encoded); !errors.Is(err, ErrInvalidOwnershipRecord) {
				t.Fatalf("invalid certified fact error = %v", err)
			}
		})
	}

	expected := ExpectedOwnership{
		Backend:       transfer.NativeFilesystemOutputBackendID,
		RootIdentity:  bytes.Repeat([]byte{0x41}, RootIdentityBytes),
		Certification: CertificationWindowsNTFSProcessRestart,
		Durability:    transfer.DurabilityProcessRestart,
	}
	if err := ValidateExpectedOwnership(expected); err != nil {
		t.Fatalf("valid expectation = %v", err)
	}
	expected.RootIdentity = nil
	if err := ValidateExpectedOwnership(expected); !errors.Is(err, ErrInvalidExpectedOwnership) {
		t.Fatalf("invalid expectation = %v", err)
	}
}

func TestC5ClosureOwnershipEncoderHonorsTheWholeEnvelopeBound(t *testing.T) {
	stored := c5ClosureOwnership()
	stored.Certification = strings.Repeat("x", MaxOwnershipRecordBytes)
	if _, err := encodeStoredOwnership(stored); !errors.Is(err, ErrInvalidOwnershipRecord) {
		t.Fatalf("oversized payload = %v", err)
	}

	stored = c5ClosureOwnership()
	stored.Certification = ""
	emptyPayload, err := legacyEncoder.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	envelopeOverhead := len(legacyControlMagic) + legacyEnvelopeLengthBytes + sha256.Size
	// A long text value uses two more CBOR header bytes than the empty value.
	certificationBytes := MaxOwnershipRecordBytes - envelopeOverhead - len(emptyPayload) - 2 + 1
	stored.Certification = strings.Repeat("x", certificationBytes)
	payload, err := legacyEncoder.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > MaxOwnershipRecordBytes || len(payload)+envelopeOverhead <= MaxOwnershipRecordBytes {
		t.Fatalf("boundary fixture payload=%d overhead=%d", len(payload), envelopeOverhead)
	}
	if _, err := encodeStoredOwnership(stored); !errors.Is(err, ErrInvalidOwnershipRecord) {
		t.Fatalf("oversized envelope = %v", err)
	}
}

func TestC5ClosureLayoutRecognizesNamesWithoutDecodingRetiredRecords(t *testing.T) {
	digest := strings.Repeat("a", encodedDigestCharacters)
	if !IsHeaderTemporary(HeaderRecord + legacyUpdateSeparator + digest) {
		t.Fatal("canonical header temporary was rejected")
	}
	for _, name := range []string{
		HeaderRecord,
		HeaderRecord + legacyUpdateSeparator,
		HeaderRecord + legacyUpdateSeparator + strings.Repeat("0", encodedDigestCharacters),
		HeaderRecord + legacyUpdateSeparator + strings.ToUpper(digest),
	} {
		if IsHeaderTemporary(name) {
			t.Fatalf("malformed header marker %q was accepted", name)
		}
	}
	if IsFileRecordTemporary("aa", digest+c5ClosureRecordSuffix) {
		t.Fatal("ordinary record was treated as a temporary")
	}
	if IsFileRecord("aa", digest+c5ClosureRecordSuffix+"extra") || IsAnchor("aa", digest) || IsStage("aa", digest) {
		t.Fatal("malformed sharded marker was accepted")
	}
}

const c5ClosureRecordSuffix = ".state"

func c5ClosureOwnership() storedOwnership {
	return storedOwnership{
		Schema:        legacySchemaVersion,
		Backend:       string(transfer.NativeFilesystemOutputBackendID),
		RootIdentity:  bytes.Repeat([]byte{0x41}, RootIdentityBytes),
		Certification: CertificationWindowsNTFSProcessRestart,
		Durability:    legacyStoredProcessRestart,
		Generation:    1,
	}
}

func c5ClosureOwnershipEnvelope(payload []byte) []byte {
	encoded := append([]byte(nil), legacyControlMagic[:]...)
	var length [legacyEnvelopeLengthBytes]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
	encoded = append(encoded, length[:]...)
	encoded = append(encoded, payload...)
	checksum := sha256.Sum256(encoded)
	return append(encoded, checksum[:]...)
}
