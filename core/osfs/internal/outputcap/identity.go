package outputcap

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const (
	OutputRootBindingBytes         = sha256.Size
	DestinationAuthorityIDBytes    = sha256.Size
	MaxRootIdentityClaimBytes      = 256
	MaxNamespaceIdentityClaimBytes = 256
	DestinationAuthorityIDDomainV1 = "windshare/destination-authority-id/v1"
)

var (
	ErrInvalidRootBinding            = errors.New("osfs: output root binding is invalid")
	ErrInvalidDestinationAuthorityID = errors.New("osfs: destination authority identity is invalid")
)

type CertificationID = checkpointmodel.CertificationID

const (
	CertificationLinuxExt4ProcessRestart   = checkpointmodel.CertificationLinuxExt4ProcessRestart
	CertificationWindowsNTFSProcessRestart = checkpointmodel.CertificationWindowsNTFSProcessRestart
)

func NewCertificationID(value string) (CertificationID, error) {
	return checkpointmodel.NewCertificationID(value)
}

// OutputRootBinding commits to both the certified volume and the current root
// object. Keeping the opaque claims out of durable state prevents a copied
// native identifier from becoming filesystem authority after restart.
type OutputRootBinding struct {
	certification CertificationID
	digest        [OutputRootBindingBytes]byte
}

func NewOutputRootBinding(
	certification CertificationID,
	volumeIdentity []byte,
	objectIdentity []byte,
) (OutputRootBinding, error) {
	validated, err := NewCertificationID(string(certification))
	if err != nil || len(volumeIdentity) == 0 || len(volumeIdentity) > MaxRootIdentityClaimBytes ||
		len(objectIdentity) == 0 || len(objectIdentity) > MaxRootIdentityClaimBytes {
		return OutputRootBinding{}, fmt.Errorf("%w: native identity claims", ErrInvalidRootBinding)
	}
	hash := sha256.New()
	writeRootBindingClaim(hash, []byte("windshare/output-root-binding/v1"))
	writeRootBindingClaim(hash, []byte(validated))
	writeRootBindingClaim(hash, volumeIdentity)
	writeRootBindingClaim(hash, objectIdentity)
	var digest [OutputRootBindingBytes]byte
	copy(digest[:], hash.Sum(nil))
	if digest == ([OutputRootBindingBytes]byte{}) {
		return OutputRootBinding{}, fmt.Errorf("%w: zero digest", ErrInvalidRootBinding)
	}
	return OutputRootBinding{certification: validated, digest: digest}, nil
}

func writeRootBindingClaim(destination io.Writer, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = destination.Write(length[:])
	_, _ = destination.Write(value)
}

func (binding OutputRootBinding) Certification() CertificationID { return binding.certification }

func (binding OutputRootBinding) Bytes() []byte {
	return append([]byte(nil), binding.digest[:]...)
}

func (binding OutputRootBinding) String() string {
	return hex.EncodeToString(binding.digest[:])
}

func (binding OutputRootBinding) IsZero() bool {
	return binding.certification == "" && binding.digest == ([OutputRootBindingBytes]byte{})
}

// DestinationAuthorityID commits only to restart-stable native objects: the
// opened destination root and its authenticated WindShare control namespace.
// Display paths and placement ancestry are omitted so a same-filesystem rename
// does not silently revoke valid recovery authority.
type DestinationAuthorityID [DestinationAuthorityIDBytes]byte

func NewDestinationAuthorityID(
	rootIdentity []byte,
	namespaceIdentity []byte,
) (DestinationAuthorityID, error) {
	if len(rootIdentity) == 0 || len(rootIdentity) > MaxRootIdentityClaimBytes ||
		len(namespaceIdentity) == 0 || len(namespaceIdentity) > MaxNamespaceIdentityClaimBytes {
		return DestinationAuthorityID{}, ErrInvalidDestinationAuthorityID
	}
	hash := sha256.New()
	writeRootBindingClaim(hash, []byte(DestinationAuthorityIDDomainV1))
	writeRootBindingClaim(hash, rootIdentity)
	writeRootBindingClaim(hash, namespaceIdentity)
	var id DestinationAuthorityID
	copy(id[:], hash.Sum(nil))
	if id.IsZero() {
		return DestinationAuthorityID{}, ErrInvalidDestinationAuthorityID
	}
	return id, nil
}

func DestinationAuthorityIDFromBytes(raw []byte) (DestinationAuthorityID, error) {
	if len(raw) != DestinationAuthorityIDBytes {
		return DestinationAuthorityID{}, ErrInvalidDestinationAuthorityID
	}
	var id DestinationAuthorityID
	copy(id[:], raw)
	if id.IsZero() {
		return DestinationAuthorityID{}, ErrInvalidDestinationAuthorityID
	}
	return id, nil
}

func (id DestinationAuthorityID) Bytes() []byte  { return append([]byte(nil), id[:]...) }
func (id DestinationAuthorityID) String() string { return hex.EncodeToString(id[:]) }
func (id DestinationAuthorityID) IsZero() bool   { return id == DestinationAuthorityID{} }

func (id DestinationAuthorityID) AuthorityRef() (receivecontract.AuthorityRef, error) {
	if id.IsZero() {
		return receivecontract.AuthorityRef{}, ErrInvalidDestinationAuthorityID
	}
	authority, err := receivecontract.AuthorityRefFromBytes(id[:])
	if err != nil {
		return receivecontract.AuthorityRef{}, errors.Join(ErrInvalidDestinationAuthorityID, err)
	}
	return authority, nil
}
