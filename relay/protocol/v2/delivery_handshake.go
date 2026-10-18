package v2

import (
	"crypto/ed25519"
	"encoding/binary"
)

// Explicit staged codecs: runtime activation requires cumulative consumption and
// settlement together. Neither parser accepts the live capacity-less handshake.
const ReceiveCapacityBytes = 16
const DeliveryRegisterInitBytes = RegisterInitBytes + ReceiveCapacityBytes
const DeliveryJoinBytes = JoinBytes + ReceiveCapacityBytes

// ReceiveCapacity bounds a connection's outstanding full WS2O frames and bytes.
// These are independent positive limits, not cumulative envelope totals.
type ReceiveCapacity struct {
	Frames       uint64
	EncodedBytes uint64
}

func (c ReceiveCapacity) Validate() error {
	if c.Frames == 0 || c.EncodedBytes == 0 {
		return ErrMalformed
	}
	return nil
}

func (c ReceiveCapacity) appendTo(encoded []byte) []byte {
	return binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(encoded, c.Frames), c.EncodedBytes)
}

func parseReceiveCapacity(encoded []byte) (ReceiveCapacity, error) {
	if len(encoded) != ReceiveCapacityBytes {
		return ReceiveCapacity{}, ErrMalformed
	}
	capacity := ReceiveCapacity{binary.BigEndian.Uint64(encoded), binary.BigEndian.Uint64(encoded[8:])}
	return capacity, capacity.Validate()
}

type DeliveryRegisterInit struct {
	Registration    RegisterInit
	ReceiveCapacity ReceiveCapacity
}

func (f DeliveryRegisterInit) MarshalBinary() ([]byte, error) {
	if err := f.ReceiveCapacity.Validate(); err != nil {
		return nil, err
	}
	encoded, err := f.Registration.MarshalBinary()
	if err != nil {
		return nil, err
	}
	return f.ReceiveCapacity.appendTo(encoded), nil
}

func ParseDeliveryRegisterInit(encoded []byte) (DeliveryRegisterInit, error) {
	if len(encoded) != DeliveryRegisterInitBytes {
		return DeliveryRegisterInit{}, ErrMalformed
	}
	registration, err := ParseRegisterInit(encoded[:RegisterInitBytes])
	if err != nil {
		return DeliveryRegisterInit{}, err
	}
	capacity, err := parseReceiveCapacity(encoded[RegisterInitBytes:])
	if err != nil {
		return DeliveryRegisterInit{}, err
	}
	return DeliveryRegisterInit{registration, capacity}, nil
}

// The capacity is signed for this handshake, but never becomes share ownership.
func DeliveryRegistrationPreimage(init DeliveryRegisterInit, challenge Challenge, relay RelayIdentity) ([]byte, error) {
	if err := init.ReceiveCapacity.Validate(); err != nil {
		return nil, err
	}
	preimage, err := RegistrationPreimage(init.Registration, challenge, relay)
	if err != nil {
		return nil, err
	}
	return init.ReceiveCapacity.appendTo(preimage), nil
}

type DeliveryJoin struct {
	ShareID         ShareID
	ReceiveCapacity ReceiveCapacity
}

// NewDeliveryRegisterProof binds the capacity used by the connection ledger to
// the same fresh challenge and authority as REGISTER or RESUME.
func NewDeliveryRegisterProof(init DeliveryRegisterInit, challenge Challenge, relay RelayIdentity, key ed25519.PrivateKey) (RegisterProof, error) {
	preimage, err := DeliveryRegistrationPreimage(init, challenge, relay)
	if err != nil {
		return RegisterProof{}, err
	}
	return signRegisterProof(init.Registration, preimage, key)
}

func (f DeliveryJoin) MarshalBinary() ([]byte, error) {
	if err := f.ReceiveCapacity.Validate(); err != nil {
		return nil, err
	}
	encoded, err := (Join{ShareID: f.ShareID}).MarshalBinary()
	if err != nil {
		return nil, err
	}
	return f.ReceiveCapacity.appendTo(encoded), nil
}

func ParseDeliveryJoin(encoded []byte) (DeliveryJoin, error) {
	if len(encoded) != DeliveryJoinBytes {
		return DeliveryJoin{}, ErrMalformed
	}
	join, err := ParseJoin(encoded[:JoinBytes])
	if err != nil {
		return DeliveryJoin{}, err
	}
	capacity, err := parseReceiveCapacity(encoded[JoinBytes:])
	if err != nil {
		return DeliveryJoin{}, err
	}
	return DeliveryJoin{join.ShareID, capacity}, nil
}
