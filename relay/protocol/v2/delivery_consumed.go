package v2

import "encoding/binary"

const (
	DeliveryConsumedMagic = "WS2B"
	DeliveryConsumedBytes = 8 + RelaySessionIDBytes + 8 + 8

	deliveryConsumedFramesOffset = 8 + RelaySessionIDBytes
	deliveryConsumedBytesOffset  = deliveryConsumedFramesOffset + 8
	minimumOpaqueRouteBytes      = OpaqueRouteHeaderBytes + 1
	maximumOpaqueRouteBytes      = OpaqueRouteHeaderBytes + MaxOpaqueCiphertextBytes
)

// DeliveryConsumed reports a cumulative prefix of complete WS2O envelopes taken
// by the destination application. The authenticated connection supplies its
// generation and receive direction; counters never reset within that flow.
// This contract is staged for the coordinated delivery-protocol cutover and is
// not interchangeable with the current runtime's ReceiveCredit delta message.
type DeliveryConsumed struct {
	RelaySessionID    RelaySessionID
	TotalFrames       uint64
	TotalEncodedBytes uint64
}

func (f DeliveryConsumed) validate() error {
	if !nonzero(f.RelaySessionID[:]) {
		return ErrIdentity
	}
	if f.TotalEncodedBytes == 0 {
		if f.TotalFrames != 0 {
			return ErrMalformed
		}
		return nil
	}
	// Division keeps validation safe at the u64 boundary. These are envelope
	// bounds, independent of the host's much smaller outstanding window.
	if f.TotalFrames > f.TotalEncodedBytes/minimumOpaqueRouteBytes ||
		f.TotalFrames < (f.TotalEncodedBytes-1)/maximumOpaqueRouteBytes+1 {
		return ErrMalformed
	}
	return nil
}

func (f DeliveryConsumed) MarshalBinary() ([]byte, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	result := append(appendReservedPrefix(nil, DeliveryConsumedMagic), f.RelaySessionID[:]...)
	result = binary.BigEndian.AppendUint64(result, f.TotalFrames)
	return binary.BigEndian.AppendUint64(result, f.TotalEncodedBytes), nil
}

func ParseDeliveryConsumed(encoded []byte) (DeliveryConsumed, error) {
	if len(encoded) != DeliveryConsumedBytes || !reservedPrefix(encoded, DeliveryConsumedMagic) {
		return DeliveryConsumed{}, ErrMalformed
	}
	var frame DeliveryConsumed
	copy(frame.RelaySessionID[:], encoded[8:deliveryConsumedFramesOffset])
	frame.TotalFrames = binary.BigEndian.Uint64(encoded[deliveryConsumedFramesOffset:])
	frame.TotalEncodedBytes = binary.BigEndian.Uint64(encoded[deliveryConsumedBytesOffset:])
	if err := frame.validate(); err != nil {
		return DeliveryConsumed{}, err
	}
	return frame, nil
}
