package v2

import (
	"encoding/binary"
	"time"
)

const (
	ConnectionProbeMagic    = "WS2H"
	ConnectionProbeAckMagic = "WS2A"
	ConnectionProbeBytes    = 16

	// Clients probe at this cadence whether or not earlier probes were
	// acknowledged, so an acknowledgement queued behind content never silences
	// the client's direction of the connection.
	ConnectionProbeInterval = 15 * time.Second
	// Either side fails a connection after this long without an accepted inbound
	// frame. Three intervals absorb a probe delayed behind one bounded physical
	// write plus network delay without treating queue pressure as loss.
	ConnectionSilenceTimeout = 45 * time.Second
)

// ConnectionProbe is connection-local liveness traffic, never a routed session
// frame. The relay answers with the newest nonce it received; every accepted
// inbound frame, not only an acknowledgement, proves the peer is alive.
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
