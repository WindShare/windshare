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

// GetConnForEndpoint prevents a mapped port from borrowing another listener.
func (m *TCPMux) GetConnForEndpoint(ufrag string, local netip.AddrPort) (net.PacketConn, error) {
	for _, endpoint := range m.endpoints {
		if endpoint.address == local {
			return endpoint.mux.GetConnByUfrag(ufrag, local.Addr().Is6(), net.IP(local.Addr().AsSlice()))
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
func (entry *pathSockets) socketCount() int {
	count := len(entry.addresses)
	if entry.tcp != nil {
		count += len(entry.tcp.endpoints)
	}
	return count
}
func (entry *pathSockets) close() error { return errors.Join(entry.mux.Close(), entry.tcp.Close()) }

// PrepareTCP is called only after a proven peer capability profile is selected.
// It is demand-owned and bounded by the same process socket capacity as UDP.
func (l *Lease) PrepareTCP(includeIPv6 bool) error {
	if l == nil {
		return ErrInvalid
	}
	a := l.authority
	a.mu.Lock()
	defer a.mu.Unlock()
	if l.released || a.closed {
		return ErrClosed
	}
	if l.entry.key.generation <= a.retiredThrough {
		return ErrRetired
	}
	if l.entry.active {
		return ErrActive
	}
	var needed []netip.Addr
	for _, address := range l.entry.addresses {
		if address.Is6() && !includeIPv6 {
			continue
		}
		existing := false
		if l.entry.tcp != nil {
			for _, endpoint := range l.entry.tcp.endpoints {
				if endpoint.address.Addr() == address {
					existing = true
					break
				}
			}
		}
		if !existing {
			needed = append(needed, address)
		}
	}
	if a.socketCount+len(needed) > a.config.Capacity {
		return ErrCapacity
	}
	allocated := &TCPMux{}
	for _, address := range needed {
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
	if l.entry.tcp == nil {
		l.entry.tcp = allocated
	} else {
		l.entry.tcp.endpoints = append(l.entry.tcp.endpoints, allocated.endpoints...)
	}
	a.socketCount += len(needed)
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
