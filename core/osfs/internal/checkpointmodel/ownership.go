package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/windshare/windshare/core/transfer"
)

const (
	OwnershipMarker = "windshare/file-checkpoint/v1"
	NamespaceName   = ".windshare-output/checkpoints-v1"

	CallerProvidedContainer RootOpenDisposition = "caller-provided-container"
	AuthorityCreatedRoot    RootOpenDisposition = "authority-created-root"

	CertificationLinuxExt4ProcessRestart   CertificationID = "linux/ext4/process-restart/v2"
	CertificationWindowsNTFSProcessRestart CertificationID = "windows/ntfs/process-restart/v1"

	ownershipDomain           = "windshare/file-checkpoint-ownership/v1"
	maximumMarkerBytes        = 128
	maximumNamespaceBytes     = 256
	maximumCertificationBytes = 128
)

var (
	ErrInvalidOwnership      = errors.New("checkpoint ownership is invalid")
	ErrOwnershipChecksum     = errors.New("checkpoint ownership checksum is invalid")
	ErrOwnershipNonCanonical = errors.New("checkpoint ownership encoding is not canonical")
)

type CertificationID string

func NewCertificationID(value string) (CertificationID, error) {
	certification := CertificationID(value)
	switch certification {
	case CertificationLinuxExt4ProcessRestart, CertificationWindowsNTFSProcessRestart:
		return certification, nil
	default:
		return "", fmt.Errorf("%w: filesystem certification ID", ErrInvalidOwnership)
	}
}

// RootOpenDisposition is persisted because only an authority-created root may
// receive synthetic-root metadata after restart.
type RootOpenDisposition string

func (disposition RootOpenDisposition) Valid() bool {
	return disposition == CallerProvidedContainer || disposition == AuthorityCreatedRoot
}

type RootIdentity [sha256.Size]byte

func (identity RootIdentity) Bytes() []byte {
	return append([]byte(nil), identity[:]...)
}

func (identity RootIdentity) IsZero() bool {
	return identity == RootIdentity{}
}

type OwnershipSpec struct {
	Backend             transfer.OutputBackendID
	Certification       CertificationID
	RootIdentity        []byte
	RootOpenDisposition RootOpenDisposition
}

// Ownership certifies only the checkpoint root binding. Intent, file, selection,
// and deletion authority deliberately cannot enter this marker.
type Ownership struct {
	backend             transfer.OutputBackendID
	certification       CertificationID
	rootIdentity        RootIdentity
	rootOpenDisposition RootOpenDisposition
}

func NewOwnership(spec OwnershipSpec) (Ownership, error) {
	backend, backendErr := transfer.NewOutputBackendID(string(spec.Backend))
	certification, certificationErr := NewCertificationID(string(spec.Certification))
	root, rootErr := RootIdentityFromBytes(spec.RootIdentity)
	if backendErr != nil || certificationErr != nil || rootErr != nil ||
		!spec.RootOpenDisposition.Valid() {
		return Ownership{}, errors.Join(
			ErrInvalidOwnership,
			backendErr,
			certificationErr,
			rootErr,
		)
	}
	return Ownership{
		backend:             backend,
		certification:       certification,
		rootIdentity:        root,
		rootOpenDisposition: spec.RootOpenDisposition,
	}, nil
}

func (ownership Ownership) Backend() transfer.OutputBackendID {
	return ownership.backend
}

func (ownership Ownership) Certification() CertificationID {
	return ownership.certification
}

func (ownership Ownership) RootIdentity() RootIdentity {
	return ownership.rootIdentity
}

func (ownership Ownership) RootOpenDisposition() RootOpenDisposition {
	return ownership.rootOpenDisposition
}

func (ownership Ownership) Valid() bool {
	rebuilt, err := NewOwnership(OwnershipSpec{
		Backend:             ownership.backend,
		Certification:       ownership.certification,
		RootIdentity:        ownership.rootIdentity[:],
		RootOpenDisposition: ownership.rootOpenDisposition,
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
	writeOwnershipString(&encoded, string(ownership.backend))
	writeOwnershipString(&encoded, string(ownership.certification))
	_, _ = encoded.Write(ownership.rootIdentity[:])
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
	backend, err := cursor.string(transfer.MaxOutputBackendIDBytes)
	if err != nil {
		return Ownership{}, err
	}
	certification, err := cursor.string(maximumCertificationBytes)
	if err != nil {
		return Ownership{}, err
	}
	root, err := cursor.take(sha256.Size)
	if err != nil {
		return Ownership{}, err
	}
	disposition, err := cursor.string(maximumMarkerBytes)
	if err != nil || cursor.offset != len(payload) {
		return Ownership{}, ErrInvalidOwnership
	}
	validatedBackend, err := transfer.NewOutputBackendID(backend)
	if err != nil {
		return Ownership{}, errors.Join(ErrInvalidOwnership, err)
	}
	ownership, err := NewOwnership(OwnershipSpec{
		Backend:             validatedBackend,
		Certification:       CertificationID(certification),
		RootIdentity:        root,
		RootOpenDisposition: RootOpenDisposition(disposition),
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
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(payload)
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
