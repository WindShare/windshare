package v2

import "encoding/binary"

const (
	SessionCreditMagic   = "WS2W"
	SessionAdmittedMagic = "WS2M"
	SessionCreditBytes   = 8 + RelaySessionIDBytes + 4 + 4
	SessionAdmittedBytes = 8 + RelaySessionIDBytes

	// The sender reserves both budgets before writing a routed frame. Credits
	// include the route header and remain spent until the destination write ends.
	SenderWindowFrames = 64
	SenderWindowBytes  = 4 << 20
)

// SessionCredit replenishes one sender-to-receiver window. Receivers have their
// own physical ingress connections and use connection-local TCP backpressure.
type SessionCredit struct {
	RelaySessionID RelaySessionID
	Frames         uint32
	Bytes          uint32
}

func (f SessionCredit) MarshalBinary() ([]byte, error) {
	if !nonzero(f.RelaySessionID[:]) {
		return nil, ErrIdentity
	}
	if f.Frames == 0 || f.Frames > SenderWindowFrames || f.Bytes < f.Frames*(OpaqueRouteHeaderBytes+1) ||
		f.Bytes > SenderWindowBytes || uint64(f.Bytes) > uint64(f.Frames)*(OpaqueRouteHeaderBytes+MaxOpaqueCiphertextBytes) {
		return nil, ErrMalformed
	}
	result := append(appendReservedPrefix(nil, SessionCreditMagic), f.RelaySessionID[:]...)
	result = binary.BigEndian.AppendUint32(result, f.Frames)
	return binary.BigEndian.AppendUint32(result, f.Bytes), nil
}

func ParseSessionCredit(encoded []byte) (SessionCredit, error) {
	if len(encoded) != SessionCreditBytes || !reservedPrefix(encoded, SessionCreditMagic) {
		return SessionCredit{}, ErrMalformed
	}
	var frame SessionCredit
	copy(frame.RelaySessionID[:], encoded[8:8+RelaySessionIDBytes])
	frame.Frames = binary.BigEndian.Uint32(encoded[8+RelaySessionIDBytes:])
	frame.Bytes = binary.BigEndian.Uint32(encoded[12+RelaySessionIDBytes:])
	if _, err := frame.MarshalBinary(); err != nil {
		return SessionCredit{}, err
	}
	return frame, nil
}

// SessionAdmitted is the registered sender's decision to retain a provisional
// relay session. The relay does not interpret the opaque E2E handshake itself.
type SessionAdmitted struct{ RelaySessionID RelaySessionID }

func (f SessionAdmitted) MarshalBinary() ([]byte, error) {
	if !nonzero(f.RelaySessionID[:]) {
		return nil, ErrIdentity
	}
	return append(appendReservedPrefix(nil, SessionAdmittedMagic), f.RelaySessionID[:]...), nil
}

func ParseSessionAdmitted(encoded []byte) (SessionAdmitted, error) {
	if len(encoded) != SessionAdmittedBytes || !reservedPrefix(encoded, SessionAdmittedMagic) {
		return SessionAdmitted{}, ErrMalformed
	}
	var frame SessionAdmitted
	copy(frame.RelaySessionID[:], encoded[8:])
	if !nonzero(frame.RelaySessionID[:]) {
		return SessionAdmitted{}, ErrIdentity
	}
	return frame, nil
}
