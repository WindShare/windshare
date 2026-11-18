package v2

import "encoding/binary"

const (
	CapacityWaitMagic           = "WS2Z"
	CapacityWaitBytes           = 8 + RelaySessionIDBytes + 4
	MaxCapacityWaitMilliseconds = 60_000
)

// CapacityWait is advisory; it cannot renew authority or suppress liveness checks.
// Each direction has its own lease, so a clear cannot erase opposite-flow pressure.
type CapacityWait struct {
	RelaySessionID RelaySessionID
	Direction      uint8
	Milliseconds   uint32
}

func (f CapacityWait) MarshalBinary() ([]byte, error) {
	if !nonzero(f.RelaySessionID[:]) {
		return nil, ErrIdentity
	}
	if f.Direction > 1 || f.Milliseconds > MaxCapacityWaitMilliseconds {
		return nil, ErrMalformed
	}
	result := append(appendReservedPrefix(nil, CapacityWaitMagic), f.RelaySessionID[:]...)
	result[5] = f.Direction
	return binary.BigEndian.AppendUint32(result, f.Milliseconds), nil
}

func ParseCapacityWait(encoded []byte) (CapacityWait, error) {
	if len(encoded) != CapacityWaitBytes || string(encoded[:4]) != CapacityWaitMagic ||
		encoded[4] != WireVersion || encoded[5] > 1 || encoded[6] != 0 || encoded[7] != 0 {
		return CapacityWait{}, ErrMalformed
	}
	var frame CapacityWait
	copy(frame.RelaySessionID[:], encoded[8:8+RelaySessionIDBytes])
	frame.Direction = encoded[5]
	frame.Milliseconds = binary.BigEndian.Uint32(encoded[8+RelaySessionIDBytes:])
	if _, err := frame.MarshalBinary(); err != nil {
		return CapacityWait{}, err
	}
	return frame, nil
}
