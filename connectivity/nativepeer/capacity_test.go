package nativepeer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/windshare/windshare/connectivity/socketauthority"
)

type addressedListener struct {
	net.Listener
	address *net.TCPAddr
}

func (l addressedListener) Addr() net.Addr { return l.address }

// testSocketAuthority binds local ephemeral sockets while reporting requested
// documentation IPs so unit tests exercise production non-loopback invariants
// without depending on host NIC configuration.
func testSocketAuthority(t *testing.T, capacity int) *socketauthority.Authority {
	t.Helper()
	authority := socketauthority.New(socketauthority.Config{
		Capacity: capacity,
		ListenPacket: func(_, address string) (net.PacketConn, error) {
			requested, err := netip.ParseAddrPort(address)
			if err != nil {
				return nil, err
			}
			conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			port := conn.LocalAddr().(*net.UDPAddr).AddrPort().Port()
			return addressedPacket{PacketConn: conn, address: net.UDPAddrFromAddrPort(netip.AddrPortFrom(requested.Addr().Unmap(), port))}, nil
		},
		ListenTCP: func(_, address string) (net.Listener, error) {
			requested, err := netip.ParseAddrPort(address)
			if err != nil {
				return nil, err
			}
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			port := listener.Addr().(*net.TCPAddr).AddrPort().Port()
			return addressedListener{Listener: listener, address: net.TCPAddrFromAddrPort(netip.AddrPortFrom(requested.Addr().Unmap(), port))}, nil
		},
	})
	t.Cleanup(func() { _ = authority.Close() })
	return authority
}

func newTestConnectivity(t *testing.T, config Config) *NativePeerConnectivity {
	t.Helper()
	if config.Admission == nil {
		config.Admission = NewAdmission(AdmissionClock{})
	}
	native, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := native.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return native
}

func TestConnectivityRequiresExplicitCapacity(t *testing.T) {
	if native, err := New(Config{}); native != nil || !errors.Is(err, ErrCapacityRequired) {
		t.Fatalf("missing capacity accepted: native=%v err=%v", native, err)
	}
}
