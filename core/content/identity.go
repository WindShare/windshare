package content

import (
	"crypto/subtle"
	"errors"
	"fmt"
)

const IdentityBytes = 16

var ErrIdentityLength = errors.New("content identity must be exactly 16 bytes")

type (
	FileRevision [IdentityBytes]byte
	LeaseID      [IdentityBytes]byte
)

func contentIdentityFromBytes[T ~[IdentityBytes]byte](raw []byte) (T, error) {
	var value T
	if len(raw) != IdentityBytes {
		return value, fmt.Errorf("%w: got %d", ErrIdentityLength, len(raw))
	}
	copy(value[:], raw)
	return value, nil
}

func FileRevisionFromBytes(raw []byte) (FileRevision, error) {
	return contentIdentityFromBytes[FileRevision](raw)
}

func LeaseIDFromBytes(raw []byte) (LeaseID, error) { return contentIdentityFromBytes[LeaseID](raw) }

func contentIdentityBytes[T ~[IdentityBytes]byte](value T) []byte {
	return append([]byte(nil), value[:]...)
}

func contentIdentityZero[T ~[IdentityBytes]byte](value T) bool {
	var zero T
	return subtle.ConstantTimeCompare(value[:], zero[:]) == 1
}

func contentIdentityEqual[T ~[IdentityBytes]byte](left, right T) bool {
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

func (id FileRevision) Bytes() []byte                 { return contentIdentityBytes(id) }
func (id LeaseID) Bytes() []byte                      { return contentIdentityBytes(id) }
func (id FileRevision) IsZero() bool                  { return contentIdentityZero(id) }
func (id LeaseID) IsZero() bool                       { return contentIdentityZero(id) }
func (id FileRevision) Equal(other FileRevision) bool { return contentIdentityEqual(id, other) }
func (id LeaseID) Equal(other LeaseID) bool           { return contentIdentityEqual(id, other) }
