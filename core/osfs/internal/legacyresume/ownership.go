// Package legacyresume recognizes only the retired filesystem namespace facts
// needed to decide whether explicit maintenance may remove private control
// state. It deliberately has no decoder for session headers or file records.
package legacyresume

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/transfer"
)

const (
	MaxOwnershipRecordBytes = 64 << 10
	RootIdentityBytes       = sha256.Size

	CertificationLinuxExt4ProcessRestart   = "linux/ext4/process-restart/v2"
	CertificationWindowsNTFSProcessRestart = "windows/ntfs/process-restart/v1"

	legacySchemaVersion        = uint32(1)
	legacyStoredProcessRestart = uint8(1)
	legacyEnvelopeLengthBytes  = 4
)

var (
	ErrInvalidExpectedOwnership = errors.New("legacy resume ownership expectation is invalid")
	ErrInvalidOwnershipRecord   = errors.New("legacy resume ownership record is invalid")
	legacyControlMagic          = [8]byte{'W', 'S', 'O', 'C', 'T', 'L', '0', '1'}
)

type ExpectedOwnership struct {
	Backend       transfer.OutputBackendID
	RootIdentity  []byte
	Certification string
	Durability    transfer.DurabilityLevel
}

// Ownership contains only the retired control envelope's root-level ownership
// facts. Keeping generation private prevents callers from turning this decoder
// into a compatibility reader for the retired runtime state machine.
type Ownership struct {
	backend       transfer.OutputBackendID
	rootIdentity  [RootIdentityBytes]byte
	certification string
	durability    transfer.DurabilityLevel
}

type storedOwnership struct {
	Schema        uint32 `cbor:"0,keyasint"`
	Backend       string `cbor:"1,keyasint"`
	RootIdentity  []byte `cbor:"2,keyasint"`
	Certification string `cbor:"3,keyasint"`
	Durability    uint8  `cbor:"4,keyasint"`
	Generation    uint64 `cbor:"5,keyasint"`
}

var (
	legacyEncoder = func() cbor.EncMode {
		options := cbor.CoreDetEncOptions()
		options.NilContainers = cbor.NilContainerAsEmpty
		mode, err := options.EncMode()
		if err != nil {
			panic(err)
		}
		return mode
	}()
	legacyDecoder = func() cbor.DecMode {
		mode, err := cbor.DecOptions{
			DupMapKey:         cbor.DupMapKeyEnforcedAPF,
			IndefLength:       cbor.IndefLengthForbidden,
			TagsMd:            cbor.TagsForbidden,
			ExtraReturnErrors: cbor.ExtraDecErrorUnknownField,
			FieldNameMatching: cbor.FieldNameMatchingCaseSensitive,
			MaxNestedLevels:   4,
			MaxArrayElements:  16,
			MaxMapPairs:       16,
		}.DecMode()
		if err != nil {
			panic(err)
		}
		return mode
	}()
)

func DecodeOwnership(encoded []byte) (Ownership, error) {
	stored, err := decodeStoredOwnership(encoded)
	if err != nil {
		return Ownership{}, err
	}
	backend, backendErr := transfer.NewOutputBackendID(stored.Backend)
	if backendErr != nil || len(stored.RootIdentity) != RootIdentityBytes ||
		allZero(stored.RootIdentity) || !validCertification(stored.Certification) ||
		stored.Durability != legacyStoredProcessRestart || stored.Generation == 0 {
		return Ownership{}, ErrInvalidOwnershipRecord
	}
	var rootIdentity [RootIdentityBytes]byte
	copy(rootIdentity[:], stored.RootIdentity)
	return Ownership{
		backend: backend, rootIdentity: rootIdentity,
		certification: stored.Certification, durability: transfer.DurabilityProcessRestart,
	}, nil
}

func (ownership Ownership) Matches(expected ExpectedOwnership) bool {
	if validateExpectedOwnership(expected) != nil {
		return false
	}
	return ownership.backend == expected.Backend &&
		ownership.certification == expected.Certification &&
		ownership.durability == expected.Durability &&
		bytes.Equal(ownership.rootIdentity[:], expected.RootIdentity)
}

func ValidateExpectedOwnership(expected ExpectedOwnership) error {
	return validateExpectedOwnership(expected)
}

func validateExpectedOwnership(expected ExpectedOwnership) error {
	backend, backendErr := transfer.NewOutputBackendID(string(expected.Backend))
	if backendErr != nil || backend != expected.Backend ||
		len(expected.RootIdentity) != RootIdentityBytes || allZero(expected.RootIdentity) ||
		!validCertification(expected.Certification) ||
		expected.Durability != transfer.DurabilityProcessRestart {
		return ErrInvalidExpectedOwnership
	}
	return nil
}

func decodeStoredOwnership(encoded []byte) (storedOwnership, error) {
	minimumLength := len(legacyControlMagic) + legacyEnvelopeLengthBytes + sha256.Size + 1
	if len(encoded) < minimumLength || len(encoded) > MaxOwnershipRecordBytes ||
		!bytes.Equal(encoded[:len(legacyControlMagic)], legacyControlMagic[:]) {
		return storedOwnership{}, ErrInvalidOwnershipRecord
	}
	payloadLengthOffset := len(legacyControlMagic)
	payloadLength := binary.BigEndian.Uint32(encoded[payloadLengthOffset : payloadLengthOffset+legacyEnvelopeLengthBytes])
	if payloadLength == 0 || payloadLength > uint32(MaxOwnershipRecordBytes) {
		return storedOwnership{}, ErrInvalidOwnershipRecord
	}
	payloadOffset := payloadLengthOffset + legacyEnvelopeLengthBytes
	checksumOffset := payloadOffset + int(payloadLength)
	if checksumOffset != len(encoded)-sha256.Size {
		return storedOwnership{}, ErrInvalidOwnershipRecord
	}
	expectedChecksum := sha256.Sum256(encoded[:checksumOffset])
	if !bytes.Equal(encoded[checksumOffset:], expectedChecksum[:]) {
		return storedOwnership{}, ErrInvalidOwnershipRecord
	}
	var stored storedOwnership
	if err := legacyDecoder.Unmarshal(encoded[payloadOffset:checksumOffset], &stored); err != nil ||
		stored.Schema != legacySchemaVersion {
		return storedOwnership{}, errors.Join(ErrInvalidOwnershipRecord, err)
	}
	// Old writers emitted deterministic CBOR. Requiring that exact image avoids
	// widening an obsolete format merely because the CBOR library can parse it.
	canonical, err := encodeStoredOwnership(stored)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return storedOwnership{}, errors.Join(ErrInvalidOwnershipRecord, err)
	}
	return stored, nil
}

func encodeStoredOwnership(stored storedOwnership) ([]byte, error) {
	payload, err := legacyEncoder.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("encode legacy ownership: %w", err)
	}
	if len(payload) > MaxOwnershipRecordBytes {
		return nil, ErrInvalidOwnershipRecord
	}
	encoded := make([]byte, 0, len(legacyControlMagic)+legacyEnvelopeLengthBytes+len(payload)+sha256.Size)
	encoded = append(encoded, legacyControlMagic[:]...)
	var length [legacyEnvelopeLengthBytes]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
	encoded = append(encoded, length[:]...)
	encoded = append(encoded, payload...)
	checksum := sha256.Sum256(encoded)
	encoded = append(encoded, checksum[:]...)
	if len(encoded) > MaxOwnershipRecordBytes {
		return nil, ErrInvalidOwnershipRecord
	}
	return encoded, nil
}

func validCertification(value string) bool {
	return value == CertificationLinuxExt4ProcessRestart ||
		value == CertificationWindowsNTFSProcessRestart
}

func allZero(value []byte) bool {
	var combined byte
	for _, current := range value {
		combined |= current
	}
	return combined == 0
}
