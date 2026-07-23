// Package protocolsession implements the authenticated end-to-end session
// primitives shared by protocol roles. Transport and operation routing remain
// outside this package so relay identity can never become an authorization input.
package protocolsession

import (
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/catalog"
)

const IdentityBytes = catalog.IdentityBytes

var ErrIdentityLength = errors.New("protocol session identity must be exactly 16 bytes")

type (
	ProtocolSessionID  [IdentityBytes]byte
	ReceiverInstanceID [IdentityBytes]byte
	OperationID        [IdentityBytes]byte
)

func identityFromBytes[T ~[IdentityBytes]byte](raw []byte) (T, error) {
	var value T
	if len(raw) != IdentityBytes {
		return value, fmt.Errorf("%w: got %d", ErrIdentityLength, len(raw))
	}
	copy(value[:], raw)
	return value, nil
}

func identityBytes[T ~[IdentityBytes]byte](value T) []byte {
	result := make([]byte, IdentityBytes)
	copy(result, value[:])
	return result
}

func identityEqual[T ~[IdentityBytes]byte](left, right T) bool {
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

func ProtocolSessionIDFromBytes(raw []byte) (ProtocolSessionID, error) {
	return identityFromBytes[ProtocolSessionID](raw)
}

func ReceiverInstanceIDFromBytes(raw []byte) (ReceiverInstanceID, error) {
	return identityFromBytes[ReceiverInstanceID](raw)
}

func OperationIDFromBytes(raw []byte) (OperationID, error) {
	return identityFromBytes[OperationID](raw)
}

func (id ProtocolSessionID) Bytes() []byte  { return identityBytes(id) }
func (id ReceiverInstanceID) Bytes() []byte { return identityBytes(id) }
func (id OperationID) Bytes() []byte        { return identityBytes(id) }

func (id ProtocolSessionID) IsZero() bool  { return id == ProtocolSessionID{} }
func (id ReceiverInstanceID) IsZero() bool { return id == ReceiverInstanceID{} }
func (id OperationID) IsZero() bool        { return id == OperationID{} }

func (id ProtocolSessionID) Equal(other ProtocolSessionID) bool { return identityEqual(id, other) }
func (id ReceiverInstanceID) Equal(other ReceiverInstanceID) bool {
	return identityEqual(id, other)
}
func (id OperationID) Equal(other OperationID) bool { return identityEqual(id, other) }
