package v2

import "encoding/binary"

const (
	ConnectionProbeMagic    = "WS2H"
	ConnectionProbeAckMagic = "WS2A"
	ConnectionProbeBytes    = 16
)

// ConnectionProbe is connection-local liveness evidence, never a routed session
// frame. Only its matching acknowledgement establishes a successful round trip.
type ConnectionProbe struct{ Nonce uint64 }
type ConnectionProbeAck struct{ Nonce uint64 }

func (f ConnectionProbe) MarshalBinary() ([]byte, error) {
	return marshalConnectionProbe(ConnectionProbeMagic, f.Nonce)
}
func (f ConnectionProbeAck) MarshalBinary() ([]byte, error) {
	return marshalConnectionProbe(ConnectionProbeAckMagic, f.Nonce)
}
func marshalConnectionProbe(magic string, nonce uint64) ([]byte, error) {
	if nonce == 0 {
		return nil, ErrIdentity
	}
	return binary.BigEndian.AppendUint64(appendReservedPrefix(nil, magic), nonce), nil
}
func ParseConnectionProbe(encoded []byte) (ConnectionProbe, error) {
	nonce, err := parseConnectionProbe(encoded, ConnectionProbeMagic)
	return ConnectionProbe{Nonce: nonce}, err
}
func ParseConnectionProbeAck(encoded []byte) (ConnectionProbeAck, error) {
	nonce, err := parseConnectionProbe(encoded, ConnectionProbeAckMagic)
	return ConnectionProbeAck{Nonce: nonce}, err
}
func parseConnectionProbe(encoded []byte, magic string) (uint64, error) {
	if len(encoded) != ConnectionProbeBytes || !reservedPrefix(encoded, magic) {
		return 0, ErrMalformed
	}
	nonce := binary.BigEndian.Uint64(encoded[8:])
	if nonce == 0 {
		return 0, ErrIdentity
	}
	return nonce, nil
}
