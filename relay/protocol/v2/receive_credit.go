package v2

import "encoding/binary"

const (
	ReceiveCreditMagic = "WS2B"
	ReceiveCreditBytes = 8 + RelaySessionIDBytes + 4 + 4
	// The receive window holds a full default eight-block response pipeline,
	// including envelopes and controls, without adding a round trip per block.
	ReceiveWindowFrames      = 256
	ReceiveWindowBytes       = 16 << 20
	ReceiveCreditBatchFrames = ReceiveWindowFrames / 4
)

// ReceiveCredit reserves receiver-owned ingress storage for relay delivery.
// The first grant opens an initially empty window; subsequent grants return
// capacity only after the application takes frames out of that storage.
type ReceiveCredit struct {
	RelaySessionID RelaySessionID
	Frames         uint32
	Bytes          uint32
}

func (f ReceiveCredit) MarshalBinary() ([]byte, error) {
	if !nonzero(f.RelaySessionID[:]) {
		return nil, ErrIdentity
	}
	if (f.Frames == 0 && f.Bytes == 0) || f.Frames > ReceiveWindowFrames || f.Bytes > ReceiveWindowBytes {
		return nil, ErrMalformed
	}
	result := append(appendReservedPrefix(nil, ReceiveCreditMagic), f.RelaySessionID[:]...)
	result = binary.BigEndian.AppendUint32(result, f.Frames)
	return binary.BigEndian.AppendUint32(result, f.Bytes), nil
}

func ParseReceiveCredit(encoded []byte) (ReceiveCredit, error) {
	if len(encoded) != ReceiveCreditBytes || !reservedPrefix(encoded, ReceiveCreditMagic) {
		return ReceiveCredit{}, ErrMalformed
	}
	var frame ReceiveCredit
	copy(frame.RelaySessionID[:], encoded[8:8+RelaySessionIDBytes])
	frame.Frames = binary.BigEndian.Uint32(encoded[8+RelaySessionIDBytes:])
	frame.Bytes = binary.BigEndian.Uint32(encoded[12+RelaySessionIDBytes:])
	if _, err := frame.MarshalBinary(); err != nil {
		return ReceiveCredit{}, err
	}
	return frame, nil
}
