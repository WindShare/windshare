package v2

import "encoding/binary"

const (
	SessionCreditMagic   = "WS2W"
	SessionAdmittedMagic = "WS2M"
	SessionCreditBytes   = 8 + RelaySessionIDBytes + 4 + 4
	SessionAdmittedBytes = 8 + RelaySessionIDBytes

	// Every source reserves both budgets, including the route header, before
	// writing a routed frame. Senders start with this window; receivers start
	// at zero and receive explicit grants from the destination reservation pool.
	SenderWindowFrames = 64
	SenderWindowBytes  = 4 << 20
)

// SessionCredit grants independent frame and byte deltas for one direction of
// a relay session. Neither routing nor heartbeat controls consume these grants.
type SessionCredit struct {
	RelaySessionID RelaySessionID
	Frames         uint32
	Bytes          uint32
}

func (f SessionCredit) MarshalBinary() ([]byte, error) {
	if !nonzero(f.RelaySessionID[:]) {
		return nil, ErrIdentity
	}
	if (f.Frames == 0 && f.Bytes == 0) || f.Frames > SenderWindowFrames || f.Bytes > SenderWindowBytes {
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
