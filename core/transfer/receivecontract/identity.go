package receivecontract

import (
	"crypto/sha256"
	"errors"
)

const (
	StableIdentityBytes = 16
	AuthorityRefBytes   = sha256.Size
)

var ErrInvalidReceiveContract = errors.New("receive contract is invalid")

type OperationID [StableIdentityBytes]byte
type DestinationReservationID [StableIdentityBytes]byte
type WorkspaceID [StableIdentityBytes]byte
type PortablePlanID [StableIdentityBytes]byte
type AuthorityRef [AuthorityRefBytes]byte
type RepositoryRef [AuthorityRefBytes]byte
type ArtifactDigest [sha256.Size]byte
type BindingDigest [sha256.Size]byte

func OperationIDFromBytes(raw []byte) (OperationID, error) {
	var value OperationID
	if !copyIdentity(value[:], raw) {
		return OperationID{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func DestinationReservationIDFromBytes(raw []byte) (DestinationReservationID, error) {
	var value DestinationReservationID
	if !copyIdentity(value[:], raw) {
		return DestinationReservationID{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func WorkspaceIDFromBytes(raw []byte) (WorkspaceID, error) {
	var value WorkspaceID
	if !copyIdentity(value[:], raw) {
		return WorkspaceID{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func PortablePlanIDFromBytes(raw []byte) (PortablePlanID, error) {
	var value PortablePlanID
	if !copyIdentity(value[:], raw) {
		return PortablePlanID{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func AuthorityRefFromBytes(raw []byte) (AuthorityRef, error) {
	var value AuthorityRef
	if !copyIdentity(value[:], raw) {
		return AuthorityRef{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func RepositoryRefFromBytes(raw []byte) (RepositoryRef, error) {
	var value RepositoryRef
	if !copyIdentity(value[:], raw) {
		return RepositoryRef{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func ArtifactDigestFromBytes(raw []byte) (ArtifactDigest, error) {
	var value ArtifactDigest
	if !copyIdentity(value[:], raw) {
		return ArtifactDigest{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func BindingDigestFromBytes(raw []byte) (BindingDigest, error) {
	var value BindingDigest
	if !copyIdentity(value[:], raw) {
		return BindingDigest{}, ErrInvalidReceiveContract
	}
	return value, nil
}

func copyIdentity(destination, raw []byte) bool {
	if len(destination) != len(raw) || !nonZero(raw) {
		return false
	}
	copy(destination, raw)
	return true
}

func (id OperationID) Bytes() []byte              { return clone(id[:]) }
func (id DestinationReservationID) Bytes() []byte { return clone(id[:]) }
func (id WorkspaceID) Bytes() []byte              { return clone(id[:]) }
func (id PortablePlanID) Bytes() []byte           { return clone(id[:]) }
func (id AuthorityRef) Bytes() []byte             { return clone(id[:]) }
func (id RepositoryRef) Bytes() []byte            { return clone(id[:]) }
func (id ArtifactDigest) Bytes() []byte           { return clone(id[:]) }
func (id BindingDigest) Bytes() []byte            { return clone(id[:]) }

func (id OperationID) IsZero() bool              { return id == OperationID{} }
func (id DestinationReservationID) IsZero() bool { return id == DestinationReservationID{} }
func (id WorkspaceID) IsZero() bool              { return id == WorkspaceID{} }
func (id PortablePlanID) IsZero() bool           { return id == PortablePlanID{} }
func (id AuthorityRef) IsZero() bool             { return id == AuthorityRef{} }
func (id RepositoryRef) IsZero() bool            { return id == RepositoryRef{} }
func (id ArtifactDigest) IsZero() bool           { return id == ArtifactDigest{} }
func (id BindingDigest) IsZero() bool            { return id == BindingDigest{} }
