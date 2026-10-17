package v2

import "encoding/binary"

const (
	deliveryTotalsBytes        = 8 + RelaySessionIDBytes + 8 + 8
	deliveryTotalsFramesOffset = 8 + RelaySessionIDBytes
	deliveryTotalsBytesOffset  = deliveryTotalsFramesOffset + 8
	minimumOpaqueRouteBytes    = OpaqueRouteHeaderBytes + 1
	maximumOpaqueRouteBytes    = OpaqueRouteHeaderBytes + MaxOpaqueCiphertextBytes
)

// Only the layout is shared; public controls remain distinct named types.
type deliveryTotals struct {
	RelaySessionID    RelaySessionID
	TotalFrames       uint64
	TotalEncodedBytes uint64
}

func (f deliveryTotals) validate() error {
	if !nonzero(f.RelaySessionID[:]) {
		return ErrIdentity
	}
	if f.TotalEncodedBytes == 0 {
		if f.TotalFrames != 0 {
			return ErrMalformed
		}
		return nil
	}
	// Division keeps validation safe at the u64 boundary, independently of windows.
	if f.TotalFrames > f.TotalEncodedBytes/minimumOpaqueRouteBytes ||
		f.TotalFrames < (f.TotalEncodedBytes-1)/maximumOpaqueRouteBytes+1 {
		return ErrMalformed
	}
	return nil
}

func (f deliveryTotals) marshal(magic string) ([]byte, error) {
	if err := f.validate(); err != nil {
		return nil, err
	}
	result := append(appendReservedPrefix(nil, magic), f.RelaySessionID[:]...)
	result = binary.BigEndian.AppendUint64(result, f.TotalFrames)
	return binary.BigEndian.AppendUint64(result, f.TotalEncodedBytes), nil
}

func parseDeliveryTotals(encoded []byte, magic string) (deliveryTotals, error) {
	if len(encoded) != deliveryTotalsBytes || !reservedPrefix(encoded, magic) {
		return deliveryTotals{}, ErrMalformed
	}
	var frame deliveryTotals
	copy(frame.RelaySessionID[:], encoded[8:deliveryTotalsFramesOffset])
	frame.TotalFrames = binary.BigEndian.Uint64(encoded[deliveryTotalsFramesOffset:])
	frame.TotalEncodedBytes = binary.BigEndian.Uint64(encoded[deliveryTotalsBytesOffset:])
	if err := frame.validate(); err != nil {
		return deliveryTotals{}, err
	}
	return frame, nil
}
