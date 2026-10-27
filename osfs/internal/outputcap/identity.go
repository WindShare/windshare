package outputcap

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
)

const (
	OutputRootBindingBytes         = sha256.Size
	DestinationBindingIDBytes      = sha256.Size
	MaxRootIdentityClaimBytes      = 256
	MaxNamespaceIdentityClaimBytes = 256
	DestinationBindingIDDomainV1   = "windshare/destination-authority-id/v1"
)

var (
	ErrInvalidRootBinding          = errors.New("osfs: output root binding is invalid")
	ErrInvalidDestinationBindingID = errors.New("osfs: destination binding identity is invalid")
)

type IdentitySchemeID = checkpointmodel.IdentitySchemeID

const (
	IdentitySchemeLinuxHandleV2     = checkpointmodel.IdentitySchemeLinuxHandleV2
	IdentitySchemeWindowsObjectIDV1 = checkpointmodel.IdentitySchemeWindowsObjectIDV1
	IdentitySchemeDarwinObjectIDV1  = checkpointmodel.IdentitySchemeDarwinObjectIDV1
)

func NewIdentitySchemeID(value string) (IdentitySchemeID, error) {
	return checkpointmodel.NewIdentitySchemeID(value)
}

// OutputRootBinding commits to both the certified volume and the current root
// object. Keeping the opaque claims out of durable state prevents a copied
// native identifier from becoming filesystem authority after restart.
type OutputRootBinding struct {
	identityScheme IdentitySchemeID
	digest         [OutputRootBindingBytes]byte
}

func NewOutputRootBinding(
	identityScheme IdentitySchemeID,
	volumeIdentity []byte,
	objectIdentity []byte,
) (OutputRootBinding, error) {
	validated, err := NewIdentitySchemeID(string(identityScheme))
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
	return OutputRootBinding{identityScheme: validated, digest: digest}, nil
}

func writeRootBindingClaim(destination io.Writer, value []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = destination.Write(length[:])
	_, _ = destination.Write(value)
}

func (binding OutputRootBinding) IdentityScheme() IdentitySchemeID { return binding.identityScheme }

func (binding OutputRootBinding) Bytes() []byte {
	return append([]byte(nil), binding.digest[:]...)
}

func (binding OutputRootBinding) String() string {
	return hex.EncodeToString(binding.digest[:])
}

func (binding OutputRootBinding) IsZero() bool {
	return binding.identityScheme == "" && binding.digest == ([OutputRootBindingBytes]byte{})
}

// DestinationBindingID identifies a retained destination binding. Recoverable
// bindings derive it from restart-stable native objects; process bindings derive
// it from a fresh nonce and cannot use it to reopen a previous operation.
// Domain separators belong to this encoding: renaming the type must not change
// which retained binding the same native objects identify.
type DestinationBindingID [DestinationBindingIDBytes]byte

func NewDestinationBindingID(
	rootIdentity []byte,
	namespaceIdentity []byte,
) (DestinationBindingID, error) {
	if len(rootIdentity) == 0 || len(rootIdentity) > MaxRootIdentityClaimBytes ||
		len(namespaceIdentity) == 0 || len(namespaceIdentity) > MaxNamespaceIdentityClaimBytes {
		return DestinationBindingID{}, ErrInvalidDestinationBindingID
	}
	hash := sha256.New()
	writeRootBindingClaim(hash, []byte(DestinationBindingIDDomainV1))
	writeRootBindingClaim(hash, rootIdentity)
	writeRootBindingClaim(hash, namespaceIdentity)
	var id DestinationBindingID
	copy(id[:], hash.Sum(nil))
	if id.IsZero() {
		return DestinationBindingID{}, ErrInvalidDestinationBindingID
	}
	return id, nil
}

// NewProcessDestinationBindingID allocates an identity that cannot be recreated
// from a path or stale private files. The retained root handle remains authority.
func NewProcessDestinationBindingID(nonce []byte) (DestinationBindingID, error) {
	if len(nonce) != DestinationBindingIDBytes {
		return DestinationBindingID{}, ErrInvalidDestinationBindingID
	}
	var zero [DestinationBindingIDBytes]byte
	if string(nonce) == string(zero[:]) {
		return DestinationBindingID{}, ErrInvalidDestinationBindingID
	}
	hash := sha256.New()
	writeRootBindingClaim(hash, []byte("windshare/process-destination-authority-id/v1"))
	writeRootBindingClaim(hash, nonce)
	return DestinationBindingIDFromBytes(hash.Sum(nil))
}

func DestinationBindingIDFromBytes(raw []byte) (DestinationBindingID, error) {
	if len(raw) != DestinationBindingIDBytes {
		return DestinationBindingID{}, ErrInvalidDestinationBindingID
	}
	var id DestinationBindingID
	copy(id[:], raw)
	if id.IsZero() {
		return DestinationBindingID{}, ErrInvalidDestinationBindingID
	}
	return id, nil
}

func (id DestinationBindingID) Bytes() []byte  { return append([]byte(nil), id[:]...) }
func (id DestinationBindingID) String() string { return hex.EncodeToString(id[:]) }
func (id DestinationBindingID) IsZero() bool   { return id == DestinationBindingID{} }

func (id DestinationBindingID) AuthorityRef() (receivecontract.AuthorityRef, error) {
	if id.IsZero() {
		return receivecontract.AuthorityRef{}, ErrInvalidDestinationBindingID
	}
	authority, err := receivecontract.AuthorityRefFromBytes(id[:])
	if err != nil {
		return receivecontract.AuthorityRef{}, errors.Join(ErrInvalidDestinationBindingID, err)
	}
	return authority, nil
}
