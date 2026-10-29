package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	OwnershipMarker = "windshare/file-checkpoint/v2"
	NamespaceName   = ".windshare-output/checkpoints-v2"

	CallerProvidedContainer RootOpenDisposition = "caller-provided-container"
	AuthorityCreatedRoot    RootOpenDisposition = "authority-created-root"

	IdentitySchemeLinuxHandleV2     IdentitySchemeID = "linux/ext4/handle/v2"
	IdentitySchemeWindowsObjectIDV1 IdentitySchemeID = "windows/ntfs/object-id/v1"
	IdentitySchemeDarwinObjectIDV1  IdentitySchemeID = "darwin/apfs/object-id/v1"
	IdentitySchemePortableWitnessV1 IdentitySchemeID = "portable/witness/v1"

	ownershipDomain            = "windshare/file-checkpoint-ownership/v2"
	maximumMarkerBytes         = 128
	maximumNamespaceBytes      = 256
	maximumIdentitySchemeBytes = 128
)

var (
	ErrInvalidOwnership      = errors.New("checkpoint ownership is invalid")
	ErrOwnershipChecksum     = errors.New("checkpoint ownership checksum is invalid")
	ErrOwnershipNonCanonical = errors.New("checkpoint ownership encoding is not canonical")
)

type MaterializerKind uint8

// Native records describe only the filesystem materializer implemented here.
// Browser storage identities are owned by web/output.
const MaterializerNativeTree MaterializerKind = 1

func (kind MaterializerKind) Valid() bool {
	return kind == MaterializerNativeTree
}

type IdentitySchemeID string

func NewIdentitySchemeID(value string) (IdentitySchemeID, error) {
	identityScheme := IdentitySchemeID(value)
	switch identityScheme {
	case IdentitySchemeLinuxHandleV2, IdentitySchemeWindowsObjectIDV1, IdentitySchemeDarwinObjectIDV1, IdentitySchemePortableWitnessV1:
		return identityScheme, nil
	default:
		return "", fmt.Errorf("%w: filesystem identityScheme ID", ErrInvalidOwnership)
	}
}

// RootOpenDisposition is a identityScheme fact used by native adapters. It is
// not destination naming authority and never replaces the opaque AuthorityRef.
type RootOpenDisposition string

func (disposition RootOpenDisposition) Valid() bool {
	return disposition == CallerProvidedContainer || disposition == AuthorityCreatedRoot
}

type OwnershipSpec struct {
	Materializer        MaterializerKind
	IdentityScheme      IdentitySchemeID
	AuthorityRef        []byte
	RootOpenDisposition RootOpenDisposition
}

// Ownership certifies the v2 repository root. Operation, intent, and file
// identities are deliberately absent so the same root marker cannot authorize
// cross-operation mutation.
type Ownership struct {
	materializer        MaterializerKind
	identityScheme      IdentitySchemeID
	authorityRef        receivecontract.AuthorityRef
	rootOpenDisposition RootOpenDisposition
}

func NewOwnership(spec OwnershipSpec) (Ownership, error) {
	identityScheme, identitySchemeErr := NewIdentitySchemeID(string(spec.IdentityScheme))
	authority, authorityErr := receivecontract.AuthorityRefFromBytes(spec.AuthorityRef)
	if !spec.Materializer.Valid() || identitySchemeErr != nil || authorityErr != nil ||
		!spec.RootOpenDisposition.Valid() {
		return Ownership{}, errors.Join(ErrInvalidOwnership, identitySchemeErr, authorityErr)
	}
	return Ownership{
		materializer: spec.Materializer, identityScheme: identityScheme,
		authorityRef: authority, rootOpenDisposition: spec.RootOpenDisposition,
	}, nil
}

func (ownership Ownership) MaterializerKind() MaterializerKind         { return ownership.materializer }
func (ownership Ownership) IdentityScheme() IdentitySchemeID           { return ownership.identityScheme }
func (ownership Ownership) AuthorityRef() receivecontract.AuthorityRef { return ownership.authorityRef }
func (ownership Ownership) RootOpenDisposition() RootOpenDisposition {
	return ownership.rootOpenDisposition
}

func (ownership Ownership) Valid() bool {
	rebuilt, err := NewOwnership(OwnershipSpec{
		Materializer: ownership.materializer, IdentityScheme: ownership.identityScheme,
		AuthorityRef: ownership.authorityRef.Bytes(), RootOpenDisposition: ownership.rootOpenDisposition,
	})
	return err == nil && rebuilt == ownership
}

func (ownership Ownership) CanonicalBytes() []byte {
	if !ownership.Valid() {
		return nil
	}
	var encoded bytes.Buffer
	writeOwnershipString(&encoded, ownershipDomain)
	writeOwnershipString(&encoded, OwnershipMarker)
	writeOwnershipString(&encoded, NamespaceName)
	encoded.WriteByte(byte(ownership.materializer))
	writeOwnershipString(&encoded, string(ownership.identityScheme))
	_, _ = encoded.Write(ownership.authorityRef.Bytes())
	writeOwnershipString(&encoded, string(ownership.rootOpenDisposition))
	return encoded.Bytes()
}

func EncodeOwnership(ownership Ownership) ([]byte, error) {
	payload := ownership.CanonicalBytes()
	if len(payload) == 0 {
		return nil, ErrInvalidOwnership
	}
	checksum := ownershipChecksum(payload)
	return append(payload, checksum[:]...), nil
}

func DecodeOwnership(encoded []byte) (Ownership, error) {
	if len(encoded) <= sha256.Size {
		return Ownership{}, ErrInvalidOwnership
	}
	payload := encoded[:len(encoded)-sha256.Size]
	supplied := encoded[len(encoded)-sha256.Size:]
	expected := ownershipChecksum(payload)
	if !bytes.Equal(supplied, expected[:]) {
		return Ownership{}, ErrOwnershipChecksum
	}
	cursor := ownershipCursor{encoded: payload}
	domain, err := cursor.string(maximumMarkerBytes)
	if err != nil || domain != ownershipDomain {
		return Ownership{}, ErrInvalidOwnership
	}
	marker, err := cursor.string(maximumMarkerBytes)
	if err != nil || marker != OwnershipMarker {
		return Ownership{}, ErrInvalidOwnership
	}
	namespace, err := cursor.string(maximumNamespaceBytes)
	if err != nil || namespace != NamespaceName {
		return Ownership{}, ErrInvalidOwnership
	}
	materializer, err := cursor.byte()
	if err != nil {
		return Ownership{}, err
	}
	identityScheme, err := cursor.string(maximumIdentitySchemeBytes)
	if err != nil {
		return Ownership{}, err
	}
	authority, err := cursor.take(receivecontract.AuthorityRefBytes)
	if err != nil {
		return Ownership{}, err
	}
	disposition, err := cursor.string(maximumMarkerBytes)
	if err != nil || cursor.offset != len(payload) {
		return Ownership{}, ErrInvalidOwnership
	}
	ownership, err := NewOwnership(OwnershipSpec{
		Materializer: MaterializerKind(materializer), IdentityScheme: IdentitySchemeID(identityScheme),
		AuthorityRef: authority, RootOpenDisposition: RootOpenDisposition(disposition),
	})
	if err != nil {
		return Ownership{}, err
	}
	if !bytes.Equal(ownership.CanonicalBytes(), payload) {
		return Ownership{}, ErrOwnershipNonCanonical
	}
	return ownership, nil
}

func ownershipChecksum(payload []byte) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte(recordChecksumDomain))
	_, _ = hash.Write([]byte{0, SchemaVersion})
	writeRecordFrame(hash, payload)
	var checksum [sha256.Size]byte
	copy(checksum[:], hash.Sum(nil))
	return checksum
}

func writeOwnershipString(target *bytes.Buffer, value string) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(value)))
	_, _ = target.Write(size[:])
	_, _ = target.WriteString(value)
}

type ownershipCursor struct {
	encoded []byte
	offset  int
}

func (cursor *ownershipCursor) take(count int) ([]byte, error) {
	if count < 0 || cursor.offset < 0 || count > len(cursor.encoded)-cursor.offset {
		return nil, ErrInvalidOwnership
	}
	value := cursor.encoded[cursor.offset : cursor.offset+count]
	cursor.offset += count
	return value, nil
}

func (cursor *ownershipCursor) byte() (byte, error) {
	value, err := cursor.take(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (cursor *ownershipCursor) string(maximum int) (string, error) {
	length, err := cursor.take(4)
	if err != nil {
		return "", err
	}
	size := binary.BigEndian.Uint32(length)
	if size > uint32(maximum) {
		return "", ErrInvalidOwnership
	}
	value, err := cursor.take(int(size))
	if err != nil || !utf8.Valid(value) {
		return "", ErrInvalidOwnership
	}
	return string(value), nil
}
