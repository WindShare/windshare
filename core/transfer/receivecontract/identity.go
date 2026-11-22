package receivecontract

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

const (
	StableIdentityBytes = 16
	AuthorityRefBytes   = sha256.Size
)

var ErrInvalidReceiveContract = errors.New("receive contract is invalid")

type OperationID [StableIdentityBytes]byte
type DestinationReservationID [StableIdentityBytes]byte
type AuthorityRef [AuthorityRefBytes]byte
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

func AuthorityRefFromBytes(raw []byte) (AuthorityRef, error) {
	var value AuthorityRef
	if !copyIdentity(value[:], raw) {
		return AuthorityRef{}, ErrInvalidReceiveContract
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

func marshalIdentityHex(raw []byte) ([]byte, error) {
	if !nonZero(raw) {
		return nil, ErrInvalidReceiveContract
	}
	encoded := make([]byte, hex.EncodedLen(len(raw)))
	hex.Encode(encoded, raw)
	return encoded, nil
}

func unmarshalIdentityHex(destination, text []byte) error {
	if len(text) != hex.EncodedLen(len(destination)) {
		return ErrInvalidReceiveContract
	}
	decoded := make([]byte, len(destination))
	if _, err := hex.Decode(decoded, text); err != nil || !nonZero(decoded) {
		return ErrInvalidReceiveContract
	}
	copy(destination, decoded)
	return nil
}

func (id OperationID) Bytes() []byte              { return clone(id[:]) }
func (id DestinationReservationID) Bytes() []byte { return clone(id[:]) }
func (id AuthorityRef) Bytes() []byte             { return clone(id[:]) }
func (id ArtifactDigest) Bytes() []byte           { return clone(id[:]) }
func (id BindingDigest) Bytes() []byte            { return clone(id[:]) }

func (id OperationID) IsZero() bool              { return id == OperationID{} }
func (id DestinationReservationID) IsZero() bool { return id == DestinationReservationID{} }
func (id AuthorityRef) IsZero() bool             { return id == AuthorityRef{} }
func (id ArtifactDigest) IsZero() bool           { return id == ArtifactDigest{} }
func (id BindingDigest) IsZero() bool            { return id == BindingDigest{} }

func (id OperationID) MarshalText() ([]byte, error)              { return marshalIdentityHex(id[:]) }
func (id DestinationReservationID) MarshalText() ([]byte, error) { return marshalIdentityHex(id[:]) }
func (id AuthorityRef) MarshalText() ([]byte, error)             { return marshalIdentityHex(id[:]) }
func (id ArtifactDigest) MarshalText() ([]byte, error)           { return marshalIdentityHex(id[:]) }
func (id BindingDigest) MarshalText() ([]byte, error)            { return marshalIdentityHex(id[:]) }

func (id *OperationID) UnmarshalText(text []byte) error {
	return unmarshalIdentityHex(id[:], text)
}

func (id *DestinationReservationID) UnmarshalText(text []byte) error {
	return unmarshalIdentityHex(id[:], text)
}

func (id *AuthorityRef) UnmarshalText(text []byte) error {
	return unmarshalIdentityHex(id[:], text)
}

func (id *ArtifactDigest) UnmarshalText(text []byte) error {
	return unmarshalIdentityHex(id[:], text)
}

func (id *BindingDigest) UnmarshalText(text []byte) error {
	return unmarshalIdentityHex(id[:], text)
}
