package socketauthority

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v3"
)

// Mux routes each local interface/family to its own physical endpoint. It is
// immutable once published; only Authority closes it.
type Mux struct{ endpoints []*ice.UniversalUDPMuxDefault }

func (m *Mux) endpoint(addr net.Addr) (*ice.UniversalUDPMuxDefault, error) {
	for _, endpoint := range m.endpoints {
		if endpoint.GetListenAddresses()[0].String() == addr.String() {
			return endpoint, nil
		}
	}
	return nil, fmt.Errorf("socket endpoint unavailable: %s", addr)
}
func (m *Mux) GetConn(ufrag string, addr net.Addr) (net.PacketConn, error) {
	endpoint, err := m.endpoint(addr)
	if err != nil {
		return nil, err
	}
	return endpoint.GetConn(ufrag, addr)
}
func (m *Mux) GetConnForURL(ufrag, url string, addr net.Addr) (net.PacketConn, error) {
	endpoint, err := m.endpoint(addr)
	if err != nil {
		return nil, err
	}
	// The physical endpoint belongs to exactly one path and one active agent.
	// Sharing the ufrag connection keeps every local candidate reachable by an
	// initial check and gives Pion's shared-connection reference counting ownership.
	return endpoint.GetConn(ufrag, addr)
}
func (m *Mux) RemoveConnByUfrag(ufrag string) {
	for _, endpoint := range m.endpoints {
		endpoint.RemoveConnByUfrag(ufrag)
	}
}
func (m *Mux) GetListenAddresses() []net.Addr {
	var result []net.Addr
	for _, endpoint := range m.endpoints {
		result = append(result, endpoint.GetListenAddresses()...)
	}
	return result
}
func (m *Mux) Close() error {
	var err error
	for _, endpoint := range m.endpoints {
		err = errors.Join(err, endpoint.Close())
	}
	return err
}
func (m *Mux) GetXORMappedAddr(server net.Addr, deadline time.Duration) (*stun.XORMappedAddress, error) {
	if len(m.endpoints) != 1 {
		return nil, errors.New("STUN lookup requires explicit local endpoint")
	}
	return m.endpoints[0].GetXORMappedAddr(server, deadline)
}
func (m *Mux) GetXORMappedAddrForLocal(server, local net.Addr, deadline time.Duration) (*stun.XORMappedAddress, error) {
	endpoint, err := m.endpoint(local)
	if err != nil {
		return nil, err
	}
	return endpoint.GetXORMappedAddr(server, deadline)
}
func (m *Mux) GetRelayedAddr(net.Addr, time.Duration) (*net.Addr, error) {
	return nil, errors.New("TURN is not supported")
}
