package v2peer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/socketauthority"
)

type addressedPacket struct {
	net.PacketConn
	address *net.UDPAddr
}

func (p addressedPacket) LocalAddr() net.Addr { return p.address }

func testSocketAuthority(t *testing.T, capacity int) *socketauthority.Authority {
	t.Helper()
	authority := socketauthority.New(socketauthority.Config{
		UDPCapacity: capacity,
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
	})
	t.Cleanup(func() { _ = authority.Close() })
	return authority
}

func newTestNative(t *testing.T, config nativepeer.Config) *nativepeer.NativePeerConnectivity {
	t.Helper()
	if config.Admission == nil {
		config.Admission = nativepeer.NewAdmission(nativepeer.AdmissionClock{})
	}
	if config.Sockets == nil {
		config.Sockets = testSocketAuthority(t, 0)
	}
	native, err := nativepeer.New(config)
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

func newTestSenderFactory(t *testing.T, config Config) (*Factory, error) {
	t.Helper()
	if config.Native == nil {
		config.Native = newTestNative(t, nativepeer.Config{})
	}
	return NewFactory(config)
}

func newTestReceiverFactory(t *testing.T, config ReceiverFactoryConfig) (*ReceiverFactory, error) {
	t.Helper()
	if config.Native == nil {
		config.Native = newTestNative(t, nativepeer.Config{})
	}
	return NewReceiverFactory(config)
}

func TestFactoriesRequireExplicitNativeOwnership(t *testing.T) {
	if factory, err := NewFactory(Config{}); factory != nil || !errors.Is(err, ErrConfig) {
		t.Fatalf("sender accepted missing native owner: factory=%v err=%v", factory, err)
	}
	if factory, err := NewReceiverFactory(ReceiverFactoryConfig{}); factory != nil || !errors.Is(err, ErrConfig) {
		t.Fatalf("receiver accepted missing native owner: factory=%v err=%v", factory, err)
	}
}
