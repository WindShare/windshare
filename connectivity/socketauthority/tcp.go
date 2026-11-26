package socketauthority

import (
	"errors"
	"net"
	"net/netip"

	"github.com/pion/ice/v4"
)

type tcpEndpoint struct {
	address netip.AddrPort
	mux     *ice.TCPMuxDefault
}

// TCPMux preserves exact local interface/listener affinity.
type TCPMux struct{ endpoints []tcpEndpoint }

func (m *TCPMux) GetConnByUfrag(ufrag string, isIPv6 bool, local net.IP) (net.PacketConn, error) {
	conns, err := m.GetAllConns(ufrag, isIPv6, local)
	if err != nil {
		return nil, err
	}
	return conns[0], nil
}
func (m *TCPMux) GetAllConns(ufrag string, isIPv6 bool, local net.IP) ([]net.PacketConn, error) {
	for _, endpoint := range m.endpoints {
		if endpoint.address.Addr().Is6() == isIPv6 && net.IP(endpoint.address.Addr().AsSlice()).Equal(local) {
			conn, err := endpoint.mux.GetConnByUfrag(ufrag, isIPv6, local)
			if err != nil {
				return nil, err
			}
			return []net.PacketConn{conn}, nil
		}
	}
	return nil, ErrInvalid
}
func (m *TCPMux) RemoveConnByUfrag(ufrag string) {
	for _, endpoint := range m.endpoints {
		endpoint.mux.RemoveConnByUfrag(ufrag)
	}
}
func (m *TCPMux) Close() error {
	if m == nil {
		return nil
	}
	var err error
	for _, endpoint := range m.endpoints {
		err = errors.Join(err, endpoint.mux.Close())
	}
	return err
}
func (entry *pathSockets) tcpCount() int {
	if entry.tcp == nil {
		return 0
	}
	return len(entry.tcp.endpoints)
}
func (entry *pathSockets) close() error { return errors.Join(entry.mux.Close(), entry.tcp.Close()) }

// PrepareTCP allocates optional passive TCP listeners for all lease addresses.
// Listeners draw from an independent TCP capacity pool so they can never starve
// mandatory UDP paths; optional paths additionally leave room for one essential
// path's TCP listeners.
func (l *Lease) PrepareTCP() error {
	if l == nil {
		return ErrInvalid
	}
	a := l.authority
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := l.unavailableLocked(); err != nil {
		return err
	}
	if l.entry.owner != iceUnclaimed {
		return ErrActive
	}
	// Lease addresses are immutable and PrepareTCP allocates atomically for the
	// full set, so an existing TCPMux already covers every address regardless of
	// OS-rewritten IPv6 zone representations (numeric index vs interface name).
	if l.entry.tcp != nil {
		return nil
	}
	addresses := l.entry.addresses
	reserve := 0
	if l.entry.optional {
		reserve = max(len(addresses), a.essentialReserveLocked())
	}
	if a.tcpCount+len(addresses)+reserve > a.config.TCPCapacity {
		return a.tcpCapacityLocked(len(addresses))
	}
	allocated := &TCPMux{}
	for _, address := range addresses {
		network := "tcp4"
		if address.Is6() {
			network = "tcp6"
		}
		listener, err := a.config.ListenTCP(network, net.JoinHostPort(address.String(), "0"))
		if err != nil {
			_ = allocated.Close()
			return err
		}
		endpoint := tcpEndpoint{address: listener.Addr().(*net.TCPAddr).AddrPort(), mux: ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: listener})}
		allocated.endpoints = append(allocated.endpoints, endpoint)
	}
	l.entry.tcp = allocated
	a.tcpCount += len(addresses)
	return nil
}
func (l *Lease) TCPEndpoints() []netip.AddrPort {
	a := l.authority
	a.mu.Lock()
	defer a.mu.Unlock()
	var endpoints []netip.AddrPort
	if l.entry.tcp != nil {
		for _, endpoint := range l.entry.tcp.endpoints {
			endpoints = append(endpoints, endpoint.address)
		}
	}
	return endpoints
}

// TCP returns the frozen listener set only while an ICE owner holds Claim.
func (l *Lease) TCP() *TCPMux {
	a := l.authority
	a.mu.Lock()
	defer a.mu.Unlock()
	return l.entry.tcp
}
